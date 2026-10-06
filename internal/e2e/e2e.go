// Package e2e seals a notification's title, subtitle, body and url into the
// pw1 end-to-end envelope, so the PushWard server stores and relays only
// ciphertext that the user's own devices open. The server never holds the
// key; it only checks the envelope's shape. testdata/vectors-v1.json is the
// shared test vector file every implementation (server, apps, CLI) passes.
package e2e

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	version = "pw1"

	// MaxEnvelope is the longest envelope the server accepts, which leaves
	// about 2266 bytes of JSON plaintext.
	MaxEnvelope = 3072

	// The limits the apps clamp to after opening, in Unicode code points.
	maxTitle = 256 // subtitle too
	maxBody  = 4096
	maxURL   = 2048

	nonceSize = 12
	tagSize   = 16
	padBlock  = 64
)

// Key is a parsed encryption key: the AES key derived from it and its Key
// ID. The input bytes themselves are not kept.
type Key struct {
	enc [32]byte
	kid string
}

// ParseKey reads a key in its text form, 64 hex characters. Whitespace
// anywhere is dropped and either case is accepted, so a key copied out of
// the app with a line break still parses.
func ParseKey(s string) (*Key, error) {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(" \t\n\r\v\f", r) {
			return -1
		}
		return r
	}, s)
	if strings.HasPrefix(s, "hlk_") || strings.HasPrefix(s, "hla_") {
		return nil, errors.New("that is an integration key, not an encryption key")
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("an encryption key is 64 hex characters")
	}
	enc, err := hkdf.Key(sha256.New, raw, nil, "pushward/e2e/v1/enc", 32)
	if err != nil {
		return nil, err
	}
	kid, err := hkdf.Key(sha256.New, raw, nil, "pushward/e2e/v1/kid", 4)
	if err != nil {
		return nil, err
	}
	k := &Key{kid: hex.EncodeToString(kid)}
	copy(k.enc[:], enc)
	return k, nil
}

// KID is the Key ID: 8 hex characters the apps show next to the key, and
// the middle part of every envelope sealed with it.
func (k *Key) KID() string { return k.kid }

// String keeps the key material out of logs and %v output.
func (k *Key) String() string { return "e2e key " + k.kid }

// Message is the sealed part of a notification. Everything else in the
// request (level, sound, thread and collapse ids, source, media, metadata,
// actions) still travels in the clear.
type Message struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Body     string `json:"body"`
	URL      string `json:"url,omitempty"`
}

// Seal checks m against the limits the apps enforce after opening, then
// encrypts it under k with a fresh nonce read from random
// (crypto/rand.Reader outside tests). The plaintext is padded with spaces to
// a multiple of 64 bytes so the envelope length says little about the text.
func Seal(k *Key, m Message, random io.Reader) (string, error) {
	if err := m.validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return "", err
	}
	plaintext := pad(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	if n := envelopeLen(len(plaintext)); n > MaxEnvelope {
		return "", fmt.Errorf("encrypted, the notification would be %d characters, over the %d limit: shorten the title or body", n, MaxEnvelope)
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", fmt.Errorf("reading nonce: %w", err)
	}
	return k.seal(nonce, plaintext)
}

// seal is the bare AES-256-GCM step: no validation, no padding. The kid is
// in the additional data, so an envelope relabeled with another kid fails
// to open.
func (k *Key) seal(nonce, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(k.enc[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	head := version + "." + k.kid
	sealed := gcm.Seal(bytes.Clone(nonce), nonce, plaintext, []byte(head))
	return head + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// pad appends spaces up to the next multiple of 64 bytes, unless the padded
// envelope would no longer fit; then the plaintext goes out as it is.
func pad(p []byte) []byte {
	n := (len(p) + padBlock - 1) / padBlock * padBlock
	if envelopeLen(n) > MaxEnvelope {
		return p
	}
	return append(p, bytes.Repeat([]byte{' '}, n-len(p))...)
}

// envelopeLen is the length of the envelope for a plaintext of n bytes:
// "pw1." + kid + "." + base64url(nonce || ciphertext || 16-byte tag).
func envelopeLen(n int) int {
	return len(version) + 1 + 8 + 1 + base64.RawURLEncoding.EncodedLen(nonceSize+n+tagSize)
}

var envelopeShape = regexp.MustCompile(`^pw1\.([0-9a-f]{8})\.([A-Za-z0-9_-]{40,})$`)

// ParseEnvelope runs the server's checks on an envelope sealed elsewhere and
// returns its Key ID and the sealed bytes (nonce, ciphertext, tag). It cannot
// tell whether the envelope opens; only a key can.
func ParseEnvelope(s string) (kid string, sealed []byte, err error) {
	if s == "" || len(s) > MaxEnvelope {
		return "", nil, fmt.Errorf("an envelope is 1 to %d characters, got %d", MaxEnvelope, len(s))
	}
	m := envelopeShape.FindStringSubmatch(s)
	if m == nil {
		return "", nil, errors.New("not a pw1 envelope (pw1.<key id>.<base64url>)")
	}
	sealed, err = base64.RawURLEncoding.Strict().DecodeString(m[2])
	if err != nil {
		return "", nil, errors.New("envelope is not valid unpadded base64url")
	}
	if len(sealed) < nonceSize+2+tagSize {
		return "", nil, errors.New("envelope is too short")
	}
	return m[1], sealed, nil
}

func (m Message) validate() error {
	switch {
	case m.Title == "":
		return errors.New("an encrypted notification needs a title")
	case m.Body == "":
		return errors.New("an encrypted notification needs a body")
	case utf8.RuneCountInString(m.Title) > maxTitle:
		return fmt.Errorf("title must not exceed %d characters", maxTitle)
	case utf8.RuneCountInString(m.Subtitle) > maxTitle:
		return fmt.Errorf("subtitle must not exceed %d characters", maxTitle)
	case utf8.RuneCountInString(m.Body) > maxBody:
		return fmt.Errorf("body must not exceed %d characters", maxBody)
	}
	return validateURL(m.URL)
}

var blockedSchemes = map[string]bool{"javascript": true, "data": true, "file": true, "vbscript": true}

// validateURL is the server's url rule. The server cannot see a sealed url,
// so the apps re-apply it after opening and drop one that fails; refusing it
// here tells the sender instead of losing the link silently.
func validateURL(raw string) error {
	if raw == "" {
		return nil
	}
	if utf8.RuneCountInString(raw) > maxURL {
		return fmt.Errorf("url must not exceed %d characters", maxURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return errors.New("url must be a valid URL with a scheme")
	}
	if blockedSchemes[strings.ToLower(u.Scheme)] {
		return fmt.Errorf("url uses blocked scheme %q", u.Scheme)
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host == "" {
		return errors.New("url must include a host for http/https URLs")
	}
	return nil
}
