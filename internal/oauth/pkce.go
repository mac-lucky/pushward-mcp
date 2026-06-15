package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// verifyPKCES256 reports whether code_verifier matches code_challenge under the
// S256 method: BASE64URL-NOPAD(SHA256(ASCII(verifier))) == challenge. Only S256
// is supported (plain is rejected at the authorize endpoint).
func verifyPKCES256(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}
