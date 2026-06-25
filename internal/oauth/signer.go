package oauth

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// signer issues and verifies ES256 access-token JWTs and exposes the public key
// as a JWKS document.
type signer struct {
	priv   *ecdsa.PrivateKey
	kid    string
	issuer string
}

func newSigner(pemKey, issuer string) (*signer, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("signing key is not valid PEM")
	}
	var priv *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse EC private key: %w", err)
		}
		priv = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("signing key is not an EC key")
		}
		priv = ec
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
	if priv.Curve.Params().BitSize != 256 {
		return nil, errors.New("signing key must be EC P-256")
	}
	return &signer{priv: priv, kid: keyID(priv), issuer: issuer}, nil
}

// keyID derives a stable kid from the public key bytes.
func keyID(priv *ecdsa.PrivateKey) string {
	pub := elliptic2Bytes(priv)
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func elliptic2Bytes(priv *ecdsa.PrivateKey) []byte {
	// Left-pad each coordinate to the curve's fixed 32-byte width before
	// concatenating, so the kid is derived from a canonical byte layout that
	// matches the padded x/y published in JWKS() - big.Int.Bytes() would strip
	// leading zeros and make the X|Y boundary ambiguous.
	const size = 32
	out := make([]byte, 2*size)
	priv.X.FillBytes(out[:size])
	priv.Y.FillBytes(out[size:])
	return out
}

// accessClaims are the registered claims of an issued access token.
type accessClaims struct {
	Scope    string `json:"scope,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	jwt.RegisteredClaims
}

// Sign issues an access-token JWT for the given subject, audience (resource),
// client and scope.
func (s *signer) Sign(subject, audience, clientID, scope string, now time.Time) (string, error) {
	claims := accessClaims{
		Scope:    scope,
		ClientID: clientID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   subject,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.kid
	return tok.SignedString(s.priv)
}

// Verify parses and validates a token, checking signature, issuer and audience.
func (s *signer) Verify(token, audience string) (*accessClaims, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return &s.priv.PublicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(s.issuer), jwt.WithAudience(audience))
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// JWKS returns the public key as a JWKS document.
func (s *signer) JWKS() []byte {
	curveBytes := func(b []byte) string {
		// P-256 coordinates are 32 bytes, left-padded.
		const size = 32
		if len(b) < size {
			pad := make([]byte, size-len(b))
			b = append(pad, b...)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	doc := map[string]any{
		"keys": []map[string]any{{
			"kty": "EC",
			"crv": "P-256",
			"use": "sig",
			"alg": "ES256",
			"kid": s.kid,
			"x":   curveBytes(s.priv.X.Bytes()),
			"y":   curveBytes(s.priv.Y.Bytes()),
		}},
	}
	out, _ := json.Marshal(doc)
	return out
}
