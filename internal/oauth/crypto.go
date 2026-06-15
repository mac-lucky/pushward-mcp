package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// hlkCipher encrypts/decrypts stored PushWard API keys at rest with AES-256-GCM.
// The per-user data key is derived via HKDF-SHA256(master, salt=userID) so a
// dump of the credentials table is useless without both the master key and the
// user id, and no two users share a data key.
type hlkCipher struct {
	master []byte // 32 bytes
}

func newHLKCipher(key string) (*hlkCipher, error) {
	raw, err := decodeKey(key)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("hlk encryption key must be 32 bytes, got %d", len(raw))
	}
	return &hlkCipher{master: raw}, nil
}

// decodeKey accepts a base64 (std or url, with/without padding) or raw 32-byte
// key string.
func decodeKey(s string) ([]byte, error) {
	if len(s) == 32 {
		return []byte(s), nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("hlk encryption key is not a valid 32-byte raw or base64 value")
}

func (c *hlkCipher) aead(userID string) (cipher.AEAD, error) {
	dk := make([]byte, 32)
	r := hkdf.New(sha256.New, c.master, []byte(userID), []byte("pushward-mcp-hlk-v1"))
	if _, err := io.ReadFull(r, dk); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt returns nonce||ciphertext for the given plaintext bound to userID.
func (c *hlkCipher) Encrypt(userID, plaintext string) ([]byte, error) {
	gcm, err := c.aead(userID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), []byte(userID)), nil
}

// Decrypt reverses Encrypt for the given userID.
func (c *hlkCipher) Decrypt(userID string, blob []byte) (string, error) {
	gcm, err := c.aead(userID)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := gcm.Open(nil, nonce, ct, []byte(userID))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// csrfTokenizer issues and verifies the consent form's anti-forgery token.
//
// The token is STATELESS and self-contained: an expiry plus an HMAC over
// (expiry || client_id), keyed by a master-derived secret. It needs no cookie
// and no server-side record, so every consent render produces an independently
// verifiable token. That is the whole point — the previous design used one
// fixed-name cookie reissued on every render, so a second render before submit
// (another tab, a reload, or the OAuth client prefetching /oauth/authorize while
// also opening it) overwrote the cookie and invalidated the first form
// (last-write-wins race). A self-validating token has no shared mutable state to
// clobber. CSRF protection on this endpoint is defense-in-depth anyway: the
// binding control is the user-pasted hlk_ key, which a forged cross-site POST
// cannot supply; the HMAC proves the form was server-issued and the expiry
// bounds replay.
type csrfTokenizer struct {
	key []byte // 32-byte HMAC-SHA256 key derived from the master
	ttl time.Duration
	now func() time.Time
}

func newCSRFTokenizer(master []byte, ttl time.Duration, now func() time.Time) *csrfTokenizer {
	dk := make([]byte, 32)
	r := hkdf.New(sha256.New, master, nil, []byte("pushward-mcp-csrf-v1"))
	_, _ = io.ReadFull(r, dk) // reading 32 bytes from HKDF-SHA256 never fails
	return &csrfTokenizer{key: dk, ttl: ttl, now: now}
}

func (t *csrfTokenizer) mac(expiry []byte, clientID string) []byte {
	h := hmac.New(sha256.New, t.key)
	h.Write(expiry) // fixed 8 bytes, so clientID can't shift the boundary
	h.Write([]byte(clientID))
	return h.Sum(nil)
}

// issue returns a fresh token bound to clientID, valid for ttl.
func (t *csrfTokenizer) issue(clientID string) string {
	var eb [8]byte
	binary.BigEndian.PutUint64(eb[:], uint64(t.now().Add(t.ttl).Unix()))
	return base64.RawURLEncoding.EncodeToString(eb[:]) + "." +
		base64.RawURLEncoding.EncodeToString(t.mac(eb[:], clientID))
}

// verify reports whether token is a valid, unexpired token for clientID.
func (t *csrfTokenizer) verify(token, clientID string) bool {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		return false
	}
	eb, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil || len(eb) != 8 {
		return false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil || !hmac.Equal(gotMAC, t.mac(eb, clientID)) {
		return false
	}
	return t.now().Unix() < int64(binary.BigEndian.Uint64(eb))
}

// randomToken returns n cryptographically-random bytes as a url-safe string.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the base64url SHA-256 of a token, for storage (so a DB leak
// does not expose live codes/refresh tokens).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
