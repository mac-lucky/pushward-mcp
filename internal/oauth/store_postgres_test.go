package oauth

import (
	"context"
	"errors"
	"fmt"
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
			got[i], errs[i] = s.ConsumeAuthCode(ctx, code, "")
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

// TestPostgresUsedClientRetention checks that a client a grant used survives
// its tokens until it has been idle for usedClientRetention, while a
// never-used one is pruned after clientRetention as before.
func TestPostgresUsedClientRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := testPGStore(t, ctx)

	suffix, _ := randomToken(8)
	used, idle := "dcr_used_"+suffix, "dcr_idle_"+suffix
	t.Cleanup(func() {
		c := context.Background()
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_refresh_tokens WHERE client_id=$1`, used)
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_clients WHERE client_id IN ($1,$2)`, used, idle)
	})
	for _, id := range []string{used, idle} {
		if err := s.SaveClient(ctx, &Client{ID: id, RedirectURIs: []string{"https://client.test/cb"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveRefreshToken(ctx, &RefreshToken{TokenHash: "rt-" + suffix, UserID: "u-" + suffix, ClientID: used,
		Scope: "mcp", Resource: "https://mcp.test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var lastUsed *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT last_used_at FROM oauth_clients WHERE client_id=$1`, used).Scan(&lastUsed); err != nil || lastUsed == nil {
		t.Fatalf("saving a grant's token must mark its client used: %v %v", lastUsed, err)
	}
	// Its tokens are gone (a theft response, purged) and both rows are old.
	if _, err := s.pool.Exec(ctx, `DELETE FROM oauth_refresh_tokens WHERE client_id=$1`, used); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_clients SET updated_at=now()-interval '2 days' WHERE client_id IN ($1,$2)`, used, idle); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetClient(ctx, used); err != nil {
		t.Fatalf("a used client must outlive its tokens: %v", err)
	}
	if _, err := s.GetClient(ctx, idle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a never-used client must still be pruned, got %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_clients SET last_used_at=now()-make_interval(secs => $2) WHERE client_id=$1`,
		used, (usedClientRetention + time.Hour).Seconds()); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetClient(ctx, used); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a client idle past usedClientRetention must be pruned, got %v", err)
	}
}

// TestPostgresLegacyCredentialCleanup checks that Cleanup drops a per-user key
// only once no live legacy chain of that user can read it.
func TestPostgresLegacyCredentialCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := testPGStore(t, ctx)

	suffix, _ := randomToken(8)
	live, dead, fresh := "pglive-"+suffix, "pgdead-"+suffix, "pgfresh-"+suffix
	t.Cleanup(func() {
		c := context.Background()
		for _, u := range []string{live, dead, fresh} {
			_, _ = s.pool.Exec(c, `DELETE FROM oauth_refresh_tokens WHERE user_id=$1`, u)
			_, _ = s.pool.Exec(c, `DELETE FROM oauth_user_credentials WHERE user_id=$1`, u)
		}
	})
	for _, u := range []string{live, dead, fresh} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO oauth_user_credentials (user_id, encrypted_hlk) VALUES ($1,'k')`, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE oauth_user_credentials SET updated_at=now()-interval '2 hours' WHERE user_id IN ($1,$2)`, live, dead); err != nil {
		t.Fatal(err)
	}
	save := func(hash, user, credID string) {
		if err := s.SaveRefreshToken(ctx, &RefreshToken{TokenHash: hash + suffix, UserID: user, ClientID: "c", Scope: "mcp",
			Resource: "https://mcp.test", CredentialID: credID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	save("live-legacy-", live, "")
	save("dead-legacy-", dead, "")
	save("dead-bound-", dead, "cred-"+suffix) // a key-bound grant never reads the per-user row
	if _, err := s.RevokeRefreshToken(ctx, "dead-legacy-"+suffix); err != nil {
		t.Fatal(err)
	}

	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, live, ""); err != nil {
		t.Fatalf("a per-user key with a live legacy chain must stay: %v", err)
	}
	if _, err := s.GetCredential(ctx, fresh, ""); err != nil {
		t.Fatalf("a per-user key written within the hour must stay: %v", err)
	}
	if _, err := s.GetCredential(ctx, dead, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a per-user key no live legacy chain reads must go, got %v", err)
	}
}

// TestPostgresGrantScopedRevoke covers what a replay needs from the store: a
// used code names the credential it was exchanged into, and the grant and
// family revokes reach that grant's tokens and no other.
func TestPostgresGrantScopedRevoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := testPGStore(t, ctx)

	suffix, _ := randomToken(8)
	user := "pgrev-" + suffix
	t.Cleanup(func() {
		c := context.Background()
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_auth_codes WHERE user_id=$1`, user)
		_, _ = s.pool.Exec(c, `DELETE FROM oauth_refresh_tokens WHERE user_id=$1`, user)
	})

	code := "code-" + suffix
	if err := s.SaveAuthCode(ctx, &AuthCode{
		CodeHash: code, ClientID: "c", UserID: user, Scope: "mcp", RedirectURI: "https://client.test/cb",
		CodeChallenge: "x", Resource: "https://mcp.test", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeAuthCode(ctx, code, "cred-a"); err != nil {
		t.Fatal(err)
	}
	if ac, err := s.ConsumeAuthCode(ctx, code, "cred-other"); !errors.Is(err, ErrCodeAlreadyUsed) || ac.CredentialID != "cred-a" {
		t.Fatalf("a replay must name the first exchange's credential: %+v %v", ac, err)
	}

	live := func(hash string) bool {
		rt, err := s.GetRefreshToken(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		return rt.RevokedAt == nil
	}
	for _, rt := range []*RefreshToken{
		{TokenHash: "a1-" + suffix, CredentialID: "cred-a-" + suffix, Family: "fam-a-" + suffix},
		{TokenHash: "a2-" + suffix, CredentialID: "cred-a-" + suffix},
		{TokenHash: "b1-" + suffix, CredentialID: "cred-b-" + suffix, Family: "fam-b-" + suffix},
		{TokenHash: "l1-" + suffix},
	} {
		rt.UserID, rt.ClientID, rt.Scope, rt.Resource, rt.ExpiresAt = user, "c", "mcp", "https://mcp.test", time.Now().Add(time.Hour)
		if err := s.SaveRefreshToken(ctx, rt); err != nil {
			t.Fatal(err)
		}
	}
	if rt, err := s.GetRefreshToken(ctx, "a1-"+suffix); err != nil || rt.Family != "fam-a-"+suffix {
		t.Fatalf("refresh token lost its family: %+v %v", rt, err)
	}

	if revoked, err := s.RevokeRefreshFamily(ctx, "fam-a-"+suffix); err != nil || !revoked {
		t.Fatalf("family revoke: %v %v, want a live token revoked", revoked, err)
	}
	if revoked, err := s.RevokeRefreshFamily(ctx, "fam-a-"+suffix); err != nil || revoked {
		t.Fatalf("second family revoke: %v %v, want nothing left", revoked, err)
	}
	if live("a1-"+suffix) || !live("a2-"+suffix) || !live("b1-"+suffix) || !live("l1-"+suffix) {
		t.Fatal("a family revoke must reach exactly that family's tokens")
	}
	if err := s.RevokeGrantRefreshTokens(ctx, "cred-a-"+suffix); err != nil {
		t.Fatal(err)
	}
	if live("a2-"+suffix) || !live("b1-"+suffix) || !live("l1-"+suffix) {
		t.Fatal("a grant revoke must reach exactly that grant's tokens")
	}
	if err := s.RevokeGrantRefreshTokens(ctx, ""); err == nil || !live("l1-"+suffix) {
		t.Fatal("an empty credential id must be refused, not revoke legacy tokens")
	}
	if err := s.RevokeLegacyRefreshTokens(ctx, user); err != nil {
		t.Fatal(err)
	}
	if live("l1-"+suffix) || !live("b1-"+suffix) {
		t.Fatal("a legacy revoke must reach the user's legacy tokens and no key-bound grant")
	}
}

// TestPostgresRotateRefreshToken checks the rotation transaction: of
// concurrent rotations of one token exactly one saves a successor, and a
// failed save rolls the revoke back so the grant keeps a live token.
func TestPostgresRotateRefreshToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := testPGStore(t, ctx)

	suffix, _ := randomToken(8)
	user := "pgrot-" + suffix
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM oauth_refresh_tokens WHERE user_id=$1`, user)
	})
	tok := func(hash, prev string) *RefreshToken {
		return &RefreshToken{TokenHash: hash, UserID: user, ClientID: "c", Scope: "mcp", Resource: "https://mcp.test",
			PrevHash: prev, ExpiresAt: time.Now().Add(time.Hour)}
	}
	old := "old-" + suffix
	if err := s.SaveRefreshToken(ctx, tok(old, "")); err != nil {
		t.Fatal(err)
	}

	// A successor that cannot be saved (its hash is taken) must leave old live.
	if won, err := s.RotateRefreshToken(ctx, old, tok(old, old)); err == nil || won {
		t.Fatalf("rotation onto a taken hash: won=%v err=%v, want an error", won, err)
	}
	if rt, err := s.GetRefreshToken(ctx, old); err != nil || rt.RevokedAt != nil {
		t.Fatalf("a failed rotation must not revoke the old token: %+v %v", rt, err)
	}

	const racers = 4
	won := make([]bool, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won[i], errs[i] = s.RotateRefreshToken(ctx, old, tok(fmt.Sprintf("next-%d-%s", i, suffix), old))
		}()
	}
	wg.Wait()
	winners := 0
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("rotate: %v", errs[i])
		}
		if won[i] {
			winners++
		}
	}
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_refresh_tokens WHERE user_id=$1 AND prev_hash=$2`, user, old).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if winners != 1 || rows != 1 {
		t.Fatalf("%d rotations won and %d successors were saved, want 1 and 1", winners, rows)
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
	ac, err := s.ConsumeAuthCode(ctx, "code", "")
	if err != nil || ac.EncryptedHLK != nil {
		t.Fatalf("pre-upgrade code must carry no pending key: %+v %v", ac, err)
	}
	if b, err := s.GetCredential(ctx, "u", ""); err != nil || string(b) != "legacy" {
		t.Fatalf("per-user row after migration: %q %v", b, err)
	}
}
