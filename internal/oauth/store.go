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

var errNoCredentialID = errors.New("no credential id: not a key-bound grant")

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
	// CredentialID is the credential the code was exchanged into, recorded by
	// ConsumeAuthCode so a replay can end that one grant.
	CredentialID string
	ExpiresAt    time.Time
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
//
// Family is a random id of a key-bound grant, also carried through every
// rotation and sent as the "<family>." prefix of its raw refresh tokens, so a
// token replayed after its row was purged still names the grant to revoke. It
// is never the credential id, which every access token shows. Empty on legacy
// grants and on key-bound ones until their next rotation here.
type RefreshToken struct {
	TokenHash    string
	UserID       string
	ClientID     string
	Scope        string
	Resource     string
	PrevHash     string
	CredentialID string
	Family       string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
}

// Store persists OAuth state. Implementations must be safe for concurrent use.
type Store interface {
	SaveClient(ctx context.Context, c *Client) error
	GetClient(ctx context.Context, id string) (*Client, error)

	SaveAuthCode(ctx context.Context, ac *AuthCode) error
	// ConsumeAuthCode atomically fetches and marks an unexpired, unused code as
	// used, returning its pending EncryptedHLK and clearing it from the store,
	// and records credentialID as the credential it is exchanged into.
	// It returns ErrNotFound if missing/expired and ErrCodeAlreadyUsed if the
	// code was already consumed - and on that reuse path it returns a non-nil
	// *AuthCode carrying at least the UserID and the recorded CredentialID, so
	// the caller can treat the replay as an attack and revoke the tokens minted
	// from that grant.
	ConsumeAuthCode(ctx context.Context, codeHash, credentialID string) (*AuthCode, error)

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
	// RevokeLegacyRefreshTokens revokes the user's refresh tokens that carry no
	// credential id (theft response for a legacy chain, which no id tells apart
	// from the user's other legacy chains). Key-bound grants are left alone.
	RevokeLegacyRefreshTokens(ctx context.Context, userID string) error
	// RevokeGrantRefreshTokens revokes every refresh token of the key-bound grant
	// credentialID and no other grant. An empty id is an error, never "all
	// legacy tokens".
	RevokeGrantRefreshTokens(ctx context.Context, credentialID string) error
	// RevokeRefreshFamily revokes the live refresh tokens of family and reports
	// whether there were any.
	RevokeRefreshFamily(ctx context.Context, family string) (bool, error)

	// SaveCredential stores the encrypted hlk_ of one grant under a new
	// credential id.
	SaveCredential(ctx context.Context, credentialID, userID string, encryptedHLK []byte) error
	// GetCredential returns the encrypted hlk_ of the user's grant credentialID.
	// An empty credentialID reads the per-user row, which only tokens issued
	// before per-grant credentials resolve and which nothing writes any more.
	GetCredential(ctx context.Context, userID, credentialID string) ([]byte, error)

	// Cleanup purges expired authorization codes, expired/long-revoked refresh
	// tokens, grant credentials no refresh token references any more, per-user
	// keys no live legacy chain reads any more, and stale clients (no refresh
	// token or live auth code, and unwritten for clientRetention if no grant
	// ever used them, unused for usedClientRetention otherwise) so none of the
	// tables grows without bound.
	Cleanup(ctx context.Context) error

	Close()
}
