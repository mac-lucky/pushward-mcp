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
	// UpdatedAt is the last time this client row was written (registration or CIMD
	// re-fetch). It drives the CIMD re-fetch TTL and the stale-client Cleanup.
	UpdatedAt time.Time
}

// AuthCode is a pending authorization code grant. Code is stored hashed.
// EncryptedHLK is the key pasted at consent, held only here until the code is
// exchanged, so an abandoned consent changes no live credential. It is empty on
// codes minted before per-grant credentials, whose key went to the per-user row.
type AuthCode struct {
	CodeHash      string
	ClientID      string
	UserID        string
	Scope         string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	EncryptedHLK  []byte
	ExpiresAt     time.Time
}

// RefreshToken is an issued refresh token (stored hashed). Rotation-reuse theft
// detection is implemented by the atomic single-winner revoke in
// RotateRefreshToken plus the post-grace family revoke in grantRefreshToken - not
// by walking PrevHash, which is retained only as audit lineage of the rotation
// chain (it is written and scanned, but no logic branches on it).
//
// CredentialID names the grant's stored key and is carried unchanged through
// every rotation. Empty means a grant from before per-grant credentials, which
// resolves the per-user row.
type RefreshToken struct {
	TokenHash    string
	UserID       string
	ClientID     string
	Scope        string
	Resource     string
	PrevHash     string
	CredentialID string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
}

// Store persists OAuth state. Implementations must be safe for concurrent use.
type Store interface {
	SaveClient(ctx context.Context, c *Client) error
	GetClient(ctx context.Context, id string) (*Client, error)

	SaveAuthCode(ctx context.Context, ac *AuthCode) error
	// ConsumeAuthCode atomically fetches and marks an unexpired, unused code as
	// used, returning its pending EncryptedHLK and clearing it from the store.
	// It returns ErrNotFound if missing/expired and ErrCodeAlreadyUsed if the
	// code was already consumed - and on that reuse path it returns a non-nil
	// *AuthCode carrying at least the UserID, so the caller can treat the replay as
	// an attack and revoke the tokens minted from that grant.
	ConsumeAuthCode(ctx context.Context, codeHash string) (*AuthCode, error)

	SaveRefreshToken(ctx context.Context, rt *RefreshToken) error
	GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error)
	// RevokeRefreshToken atomically marks a token revoked and reports whether
	// THIS call performed the revocation (rows affected > 0). A false result
	// means the token was already revoked - i.e. this caller lost a rotation
	// race - and the caller must not mint new tokens.
	RevokeRefreshToken(ctx context.Context, tokenHash string) (bool, error)
	// RotateRefreshToken revokes oldHash and saves next in one transaction, with
	// RevokeRefreshToken's single-winner result: on false nothing is saved, and
	// a failed save leaves oldHash live, so a grant never ends up without a token.
	RotateRefreshToken(ctx context.Context, oldHash string, next *RefreshToken) (bool, error)
	// RevokeUserRefreshTokens revokes all of a user's refresh tokens (theft
	// response).
	RevokeUserRefreshTokens(ctx context.Context, userID string) error

	// SaveCredential stores the encrypted hlk_ of one grant under a new
	// credential id.
	SaveCredential(ctx context.Context, credentialID, userID string, encryptedHLK []byte) error
	// GetCredential returns the encrypted hlk_ of the user's grant credentialID.
	// An empty credentialID reads the per-user row, which only tokens issued
	// before per-grant credentials resolve and which nothing writes any more.
	GetCredential(ctx context.Context, userID, credentialID string) ([]byte, error)

	// Cleanup purges expired authorization codes, expired/long-revoked refresh
	// tokens, grant credentials no refresh token references any more, and stale
	// clients (older than clientRetention with no active refresh token or live
	// auth code) so none of the tables grows without bound.
	Cleanup(ctx context.Context) error

	Close()
}
