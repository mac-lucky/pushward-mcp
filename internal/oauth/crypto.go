package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

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
