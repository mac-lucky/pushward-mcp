package oauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrations are applied in order, each recorded in schema_migrations, under an
// advisory lock so concurrently-booting replicas don't race the DDL. Migration 1
// is the original schema (idempotent CREATE ... IF NOT EXISTS, so it no-ops on an
// already-provisioned database and just records the version); later migrations
// evolve it with idempotent ALTERs. APPEND new migrations — never edit or reorder
// existing entries, or deployed databases will diverge.
var migrations = []string{
	// 1: initial schema.
	`
CREATE TABLE IF NOT EXISTS oauth_clients (
  client_id     TEXT PRIMARY KEY,
  client_name   TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT[] NOT NULL,
  is_cimd       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS oauth_auth_codes (
  code_hash      TEXT PRIMARY KEY,
  client_id      TEXT NOT NULL,
  user_id        TEXT NOT NULL,
  scope          TEXT NOT NULL,
  redirect_uri   TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  resource       TEXT NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  used_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_oauth_auth_codes_expires ON oauth_auth_codes(expires_at);
CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
  token_hash TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL,
  client_id  TEXT NOT NULL,
  scope      TEXT NOT NULL,
  resource   TEXT NOT NULL,
  prev_hash  TEXT NOT NULL DEFAULT '',
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_oauth_refresh_user ON oauth_refresh_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_oauth_refresh_expires ON oauth_refresh_tokens(expires_at);
CREATE TABLE IF NOT EXISTS oauth_user_credentials (
  user_id       TEXT PRIMARY KEY,
  encrypted_hlk BYTEA NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);`,
	// 2: track last-write time on clients for the CIMD re-fetch TTL and
	// stale-client cleanup, and index it so the cleanup DELETE is cheap.
	`
ALTER TABLE oauth_clients ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS idx_oauth_clients_updated ON oauth_clients(updated_at);`,
}

// migrationAdvisoryLock is a fixed key for pg_advisory_lock so only one pod runs
// migrations at a time (CREATE/ALTER ... IF NOT EXISTS are not race-safe against
// the system catalogs on simultaneous first boot).
const migrationAdvisoryLock int64 = 0x70757368776d6967 // "pushwmig"

type pgStore struct {
	pool *pgxpool.Pool
}

func newPostgresStore(ctx context.Context, dsn, passwordFile string) (*pgStore, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	pcfg.MaxConns = 10
	pcfg.MinConns = 2
	pcfg.MaxConnLifetime = time.Hour
	pcfg.MaxConnIdleTime = 30 * time.Minute
	pcfg.HealthCheckPeriod = time.Minute
	if passwordFile != "" {
		// Source the password from the mounted CNPG credential file rather than
		// the DSN, so it never lives in the DSN or SOPS. Read once up front to
		// fail fast on a missing/unreadable mount, then on every (re)connect via
		// BeforeConnect so a rotated password is picked up as the pool opens new
		// connections (within MaxConnLifetime) without a pod restart.
		pw, err := readPasswordFile(passwordFile)
		if err != nil {
			return nil, err
		}
		pcfg.ConnConfig.Password = pw
		pcfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
			pw, err := readPasswordFile(passwordFile)
			if err != nil {
				return err
			}
			cc.Password = pw
			return nil
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &pgStore{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return s, nil
}

// migrate applies any unapplied migrations in order under a session-level advisory
// lock, so simultaneously-booting replicas serialize their DDL instead of racing
// CREATE/ALTER ... IF NOT EXISTS against the catalogs. It is idempotent: an
// already-provisioned database whose schema_migrations is empty records the
// existing schema as version 1 (the CREATE IF NOT EXISTS no-op), then applies the
// rest.
func (s *pgStore) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationAdvisoryLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationAdvisoryLock) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i, stmt := range migrations {
		v := i + 1
		if v <= current {
			continue
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration %d: %w", v, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			return fmt.Errorf("record migration %d: %w", v, err)
		}
	}
	return nil
}

func (s *pgStore) SaveClient(ctx context.Context, c *Client) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, is_cimd, updated_at)
		 VALUES ($1,$2,$3,$4, now())
		 ON CONFLICT (client_id) DO UPDATE SET client_name=EXCLUDED.client_name,
		   redirect_uris=EXCLUDED.redirect_uris, is_cimd=EXCLUDED.is_cimd, updated_at=now()`,
		c.ID, c.Name, c.RedirectURIs, c.IsCIMD)
	return err
}

func (s *pgStore) GetClient(ctx context.Context, id string) (*Client, error) {
	c := &Client{}
	err := s.pool.QueryRow(ctx,
		`SELECT client_id, client_name, redirect_uris, is_cimd, created_at, updated_at
		 FROM oauth_clients WHERE client_id=$1`, id).
		Scan(&c.ID, &c.Name, &c.RedirectURIs, &c.IsCIMD, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *pgStore) SaveAuthCode(ctx context.Context, ac *AuthCode) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_auth_codes
		 (code_hash, client_id, user_id, scope, redirect_uri, code_challenge, resource, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ac.CodeHash, ac.ClientID, ac.UserID, ac.Scope, ac.RedirectURI, ac.CodeChallenge, ac.Resource, ac.ExpiresAt)
	return err
}

func (s *pgStore) ConsumeAuthCode(ctx context.Context, codeHash string) (*AuthCode, error) {
	ac := &AuthCode{CodeHash: codeHash}
	err := s.pool.QueryRow(ctx,
		`UPDATE oauth_auth_codes SET used_at=now()
		 WHERE code_hash=$1 AND expires_at>now() AND used_at IS NULL
		 RETURNING client_id, user_id, scope, redirect_uri, code_challenge, resource, expires_at`,
		codeHash).
		Scan(&ac.ClientID, &ac.UserID, &ac.Scope, &ac.RedirectURI, &ac.CodeChallenge, &ac.Resource, &ac.ExpiresAt)
	if err == nil {
		return ac, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// Distinguish reuse (used_at set) from absent/expired. On reuse, surface the
	// user_id so the caller can revoke the grant's token family (replay = attack).
	var used *time.Time
	var userID string
	e2 := s.pool.QueryRow(ctx, `SELECT used_at, user_id FROM oauth_auth_codes WHERE code_hash=$1`, codeHash).Scan(&used, &userID)
	if e2 == nil && used != nil {
		return &AuthCode{CodeHash: codeHash, UserID: userID}, ErrCodeAlreadyUsed
	}
	return nil, ErrNotFound
}

func (s *pgStore) SaveRefreshToken(ctx context.Context, rt *RefreshToken) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_refresh_tokens (token_hash, user_id, client_id, scope, resource, prev_hash, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rt.TokenHash, rt.UserID, rt.ClientID, rt.Scope, rt.Resource, rt.PrevHash, rt.ExpiresAt)
	return err
}

func (s *pgStore) GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	rt := &RefreshToken{TokenHash: tokenHash}
	err := s.pool.QueryRow(ctx,
		`SELECT user_id, client_id, scope, resource, prev_hash, expires_at, revoked_at
		 FROM oauth_refresh_tokens WHERE token_hash=$1`, tokenHash).
		Scan(&rt.UserID, &rt.ClientID, &rt.Scope, &rt.Resource, &rt.PrevHash, &rt.ExpiresAt, &rt.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return rt, err
}

func (s *pgStore) RevokeRefreshToken(ctx context.Context, tokenHash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE oauth_refresh_tokens SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, tokenHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *pgStore) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE oauth_refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	return err
}

func (s *pgStore) SaveUserCredential(ctx context.Context, userID string, encryptedHLK []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_user_credentials (user_id, encrypted_hlk)
		 VALUES ($1,$2)
		 ON CONFLICT (user_id) DO UPDATE SET encrypted_hlk=EXCLUDED.encrypted_hlk, updated_at=now()`,
		userID, encryptedHLK)
	return err
}

func (s *pgStore) GetUserCredential(ctx context.Context, userID string) ([]byte, error) {
	var blob []byte
	err := s.pool.QueryRow(ctx, `SELECT encrypted_hlk FROM oauth_user_credentials WHERE user_id=$1`, userID).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return blob, err
}

func (s *pgStore) Cleanup(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM oauth_auth_codes WHERE expires_at < now() - interval '1 hour'`); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM oauth_refresh_tokens
		 WHERE expires_at < now() OR (revoked_at IS NOT NULL AND revoked_at < now() - interval '1 hour')`); err != nil {
		return err
	}
	// Prune abandoned clients so anonymous DCR/CIMD traffic cannot grow the table
	// without bound: drop clients not written within clientRetention that hold no
	// active refresh token and no live authorization code. Run last so the token
	// deletes above have already removed expired grants this pass.
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM oauth_clients c
		 WHERE c.updated_at < now() - make_interval(secs => $1)
		   AND NOT EXISTS (SELECT 1 FROM oauth_refresh_tokens r WHERE r.client_id = c.client_id)
		   AND NOT EXISTS (SELECT 1 FROM oauth_auth_codes a WHERE a.client_id = c.client_id)`,
		clientRetention.Seconds()); err != nil {
		return err
	}
	return nil
}

func (s *pgStore) Close() { s.pool.Close() }

// readPasswordFile reads a DB password from a mounted credential file, trimming
// the trailing newline that secret tooling commonly appends.
func readPasswordFile(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- path is an operator-set env, not user input
	if err != nil {
		return "", fmt.Errorf("read db password file: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
