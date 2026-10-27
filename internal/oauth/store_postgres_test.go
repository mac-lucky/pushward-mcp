package oauth

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// notInTransaction matches statements Postgres refuses inside a transaction
// block. migrate runs every migration in one transaction, so none may use them.
var notInTransaction = regexp.MustCompile(`(?i)\b(CONCURRENTLY|VACUUM|CREATE\s+DATABASE|DROP\s+DATABASE|ALTER\s+SYSTEM|CREATE\s+TABLESPACE)\b`)

func TestMigrationsAreTransactional(t *testing.T) {
	for i, stmt := range migrations {
		if m := notInTransaction.FindString(stmt); m != "" {
			t.Errorf("migration %d uses %q, which cannot run inside the migration transaction", i+1, m)
		}
	}
}

// TestPostgresMigrateConcurrent boots several stores at once against
// PUSHWARD_MCP_TEST_DB_DSN, as replicas do on a rollout. Point it at a
// throwaway database, ideally fresh and through a transaction-mode PgBouncer,
// where a session advisory lock gives no exclusion and can stay held on a
// pooled server connection. Every boot must finish, the schema must be fully
// recorded, and no migration lock may be left behind.
func TestPostgresMigrateConcurrent(t *testing.T) {
	dsn := os.Getenv("PUSHWARD_MCP_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("PUSHWARD_MCP_TEST_DB_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const replicas = 8
	stores := make([]*pgStore, replicas)
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = newPostgresStore(ctx, dsn, "")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
		defer stores[i].Close()
	}

	s := stores[0]
	// A re-run on a migrated database is a no-op.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("re-run migrate: %v", err)
	}
	var rows, maxVersion int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&rows, &maxVersion); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if rows != len(migrations) || maxVersion != len(migrations) {
		t.Fatalf("schema_migrations has %d rows up to version %d, want %d", rows, maxVersion, len(migrations))
	}
	// pg_locks splits a bigint advisory key into classid (high 32 bits) and
	// objid (low 32 bits), with objsubid 1. Advisory locks are per database.
	var held int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks
		 WHERE locktype = 'advisory' AND objsubid = 1
		   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		   AND ((classid::bigint << 32) | objid::bigint) = $1`,
		migrationAdvisoryLock).Scan(&held); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	if held != 0 {
		t.Fatalf("migration advisory lock still held by %d backend(s)", held)
	}
}

// TestPostgresMigrateLockTimeout holds the migration lock in another
// transaction and checks that a boot gives up after migrationLockTimeout with
// an error that says why, instead of waiting for as long as the lock is held.
func TestPostgresMigrateLockTimeout(t *testing.T) {
	dsn := os.Getenv("PUSHWARD_MCP_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("PUSHWARD_MCP_TEST_DB_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := newPostgresStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()

	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryLock); err != nil {
		t.Fatalf("hold migration lock: %v", err)
	}

	prev := migrationLockTimeout
	migrationLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { migrationLockTimeout = prev })

	start := time.Now()
	err = s.migrate(ctx)
	if err == nil {
		t.Fatal("migrate succeeded while another transaction held the lock")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("want a 55P03 lock_not_available, got %v", err)
	}
	if !strings.Contains(err.Error(), "another session held it for over 300ms") {
		t.Fatalf("error should say why the boot gave up, got %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("migrate waited %s, want about the 300ms lock timeout", waited)
	}
}
