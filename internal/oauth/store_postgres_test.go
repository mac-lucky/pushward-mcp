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
	"github.com/jackc/pgx/v5/pgxpool"
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

// testPGStore opens a store on PUSHWARD_MCP_TEST_DB_DSN or skips.
func testPGStore(t *testing.T, ctx context.Context) *pgStore {
	t.Helper()
	dsn := os.Getenv("PUSHWARD_MCP_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("PUSHWARD_MCP_TEST_DB_DSN not set")
	}
	s, err := newPostgresStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestPostgresGrantCredentials covers the per-grant key path against a real
// database: the key rides the auth code until exactly one exchange takes it,
// credentials resolve by (user, credential id), the per-user row stays readable
// for legacy tokens, and Cleanup drops only unreferenced old credentials.
func TestPostgresGrantCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := testPGStore(t, ctx)

	suffix, _ := randomToken(8)
	user := "pgtest-" + suffix
	code, cred, orphan := "code-"+suffix, "cred-"+suffix, "orphan-"+suffix
	t.Cleanup(func() {
		c := context.Background()
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_auth_codes WHERE user_id=$1`, user)
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_refresh_tokens WHERE user_id=$1`, user)
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_grant_credentials WHERE user_id=$1`, user)
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_user_credentials WHERE user_id=$1`, user)
	})

	if err := s.SaveAuthCode(ctx, &AuthCode{
		CodeHash: code, ClientID: "c", UserID: user, Scope: "mcp", RedirectURI: "https://client.test/cb",
		CodeChallenge: "x", Resource: "https://mcp.test", EncryptedHLK: []byte("pending"),
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// Concurrent exchanges of one code: one gets the key, the rest see a replay.
	const racers = 4
	got := make([]*AuthCode, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = s.ConsumeAuthCode(ctx, code)
		}()
	}
	wg.Wait()
	winners := 0
	for i := range racers {
		switch {
		case errs[i] == nil:
			winners++
			if string(got[i].EncryptedHLK) != "pending" || got[i].UserID != user {
				t.Fatalf("winner got %+v", got[i])
			}
		case errors.Is(errs[i], ErrCodeAlreadyUsed):
		default:
			t.Fatalf("consume: %v", errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("%d exchanges consumed the code, want 1", winners)
	}
	var left []byte
	if err := s.pool.QueryRow(ctx, `SELECT encrypted_hlk FROM oauth_auth_codes WHERE code_hash=$1`, code).Scan(&left); err != nil || left != nil {
		t.Fatalf("a used code must not keep the key: %q %v", left, err)
	}

	if _, err := s.pool.Exec(ctx, `INSERT INTO oauth_user_credentials (user_id, encrypted_hlk) VALUES ($1,$2)`, user, []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{cred, orphan} {
		if err := s.SaveCredential(ctx, id, user, []byte(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveRefreshToken(ctx, &RefreshToken{
		TokenHash: "rt-" + suffix, UserID: user, ClientID: "c", Scope: "mcp", Resource: "https://mcp.test",
		CredentialID: cred, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	rt, err := s.GetRefreshToken(ctx, "rt-"+suffix)
	if err != nil || rt.CredentialID != cred {
		t.Fatalf("refresh token lost its credential id: %+v %v", rt, err)
	}
	if b, err := s.GetCredential(ctx, user, cred); err != nil || string(b) != cred {
		t.Fatalf("grant credential: %q %v", b, err)
	}
	if b, err := s.GetCredential(ctx, user, ""); err != nil || string(b) != "legacy" {
		t.Fatalf("per-user row: %q %v", b, err)
	}
	if _, err := s.GetCredential(ctx, "someone-else", cred); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user must not read the credential, got %v", err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE oauth_grant_credentials SET created_at=now()-interval '2 hours' WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, user, orphan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unreferenced credential must be purged, got %v", err)
	}
	if _, err := s.GetCredential(ctx, user, cred); err != nil {
		t.Fatalf("a credential with a refresh token must stay: %v", err)
	}
	if _, err := s.GetCredential(ctx, user, ""); err != nil {
		t.Fatalf("cleanup must leave the per-user row: %v", err)
	}
}

// TestPostgresMigrationKeepsLegacyGrants migrates a database at version 2 that
// holds a pre-upgrade grant (per-user key, refresh token, pending code) and
// checks every part still reads as a legacy grant afterwards.
func TestPostgresMigrationKeepsLegacyGrants(t *testing.T) {
	dsn := os.Getenv("PUSHWARD_MCP_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("PUSHWARD_MCP_TEST_DB_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix, _ := randomToken(6)
	schema := "pwmig_" + strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(suffix))
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &pgStore{pool: pool}
	defer s.Close()

	old := `CREATE TABLE schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());` +
		migrations[0] + `;` + migrations[1] + `;
INSERT INTO schema_migrations (version) VALUES (1), (2);
INSERT INTO oauth_user_credentials (user_id, encrypted_hlk) VALUES ('u', 'legacy');
INSERT INTO oauth_refresh_tokens (token_hash, user_id, client_id, scope, resource, expires_at)
  VALUES ('rt', 'u', 'c', 'mcp', 'https://mcp.test', now() + interval '1 day');
INSERT INTO oauth_auth_codes (code_hash, client_id, user_id, scope, redirect_uri, code_challenge, resource, expires_at)
  VALUES ('code', 'c', 'u', 'mcp', 'https://client.test/cb', 'x', 'https://mcp.test', now() + interval '5 minutes');`
	if _, err := pool.Exec(ctx, old); err != nil {
		t.Fatalf("seed version 2: %v", err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rt, err := s.GetRefreshToken(ctx, "rt")
	if err != nil || rt.CredentialID != "" {
		t.Fatalf("pre-upgrade refresh token must have no credential id: %+v %v", rt, err)
	}
	ac, err := s.ConsumeAuthCode(ctx, "code")
	if err != nil || ac.EncryptedHLK != nil {
		t.Fatalf("pre-upgrade code must carry no pending key: %+v %v", ac, err)
	}
	if b, err := s.GetCredential(ctx, "u", ""); err != nil || string(b) != "legacy" {
		t.Fatalf("per-user row after migration: %q %v", b, err)
	}
}
