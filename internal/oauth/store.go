package oauth

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Store lookups when no row matches.
var ErrNotFound = errors.New("not found")

// ErrCodeAlreadyUsed is returned when an authorization code is replayed.
var ErrCodeAlreadyUsed = errors.New("authorization code already used")

// Client is a registered OAuth client (via DCR or cached CIMD).
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	IsCIMD       bool
	CreatedAt    time.Time
}

// AuthCode is a pending authorization code grant. Code is stored hashed. The
// encrypted credential is NOT stored here — it lives once in user_credentials,
// written before the code is issued, so the short-lived code table never holds a
// second copy of a secret.
type AuthCode struct {
	CodeHash      string
	ClientID      string
	UserID        string
	Scope         string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	ExpiresAt     time.Time
}

// RefreshToken is an issued refresh token (stored hashed) with rotation
// lineage for theft detection.
type RefreshToken struct {
	TokenHash string
	UserID    string
	ClientID  string
	Scope     string
	Resource  string
	PrevHash  string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Store persists OAuth state. Implementations must be safe for concurrent use.
type Store interface {
	SaveClient(ctx context.Context, c *Client) error
	GetClient(ctx context.Context, id string) (*Client, error)

	SaveAuthCode(ctx context.Context, ac *AuthCode) error
	// ConsumeAuthCode atomically fetches and marks an unexpired, unused code as
	// used. It returns ErrNotFound if missing/expired and ErrCodeAlreadyUsed if
	// the code was already consumed (the caller should treat reuse as an attack
	// and revoke any tokens minted from it).
	ConsumeAuthCode(ctx context.Context, codeHash string) (*AuthCode, error)

	SaveRefreshToken(ctx context.Context, rt *RefreshToken) error
	GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error)
	// RevokeRefreshToken atomically marks a token revoked and reports whether
	// THIS call performed the revocation (rows affected > 0). A false result
	// means the token was already revoked — i.e. this caller lost a rotation
	// race — and the caller must not mint new tokens.
	RevokeRefreshToken(ctx context.Context, tokenHash string) (bool, error)
	// RevokeUserRefreshTokens revokes all of a user's refresh tokens (theft
	// response).
	RevokeUserRefreshTokens(ctx context.Context, userID string) error

	// SaveUserCredential upserts the encrypted hlk_ for a user.
	SaveUserCredential(ctx context.Context, userID string, encryptedHLK []byte) error
	GetUserCredential(ctx context.Context, userID string) ([]byte, error)

	// Cleanup purges expired authorization codes and expired/long-revoked
	// refresh tokens so neither table grows without bound.
	Cleanup(ctx context.Context) error

	Close()
}
