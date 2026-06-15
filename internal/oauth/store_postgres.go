package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pgSchema = `
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
);`

type pgStore struct {
	pool *pgxpool.Pool
}

func newPostgresStore(ctx context.Context, dsn string) (*pgStore, error) {
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	pcfg.MaxConns = 10
	pcfg.MinConns = 2
	pcfg.MaxConnLifetime = time.Hour
	pcfg.MaxConnIdleTime = 30 * time.Minute
	pcfg.HealthCheckPeriod = time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := pool.Exec(ctx, pgSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &pgStore{pool: pool}, nil
}

func (s *pgStore) SaveClient(ctx context.Context, c *Client) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris, is_cimd)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (client_id) DO UPDATE SET client_name=EXCLUDED.client_name,
		   redirect_uris=EXCLUDED.redirect_uris, is_cimd=EXCLUDED.is_cimd`,
		c.ID, c.Name, c.RedirectURIs, c.IsCIMD)
	return err
}

func (s *pgStore) GetClient(ctx context.Context, id string) (*Client, error) {
	c := &Client{}
	err := s.pool.QueryRow(ctx,
		`SELECT client_id, client_name, redirect_uris, is_cimd, created_at
		 FROM oauth_clients WHERE client_id=$1`, id).
		Scan(&c.ID, &c.Name, &c.RedirectURIs, &c.IsCIMD, &c.CreatedAt)
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
	// Distinguish reuse (used_at set) from absent/expired.
	var used *time.Time
	e2 := s.pool.QueryRow(ctx, `SELECT used_at FROM oauth_auth_codes WHERE code_hash=$1`, codeHash).Scan(&used)
	if e2 == nil && used != nil {
		return nil, ErrCodeAlreadyUsed
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
	return nil
}

func (s *pgStore) Close() { s.pool.Close() }
