// Package oauth implements an OAuth 2.1 Authorization Server and Resource
// Server for the remote (http transport) MCP endpoint. It makes
// mcp.pushward.app a spec-compliant remote MCP connector for Claude.ai,
// ChatGPT, and other OAuth-capable clients:
//
//   - The MCP client authenticates with a short-lived ES256 JWT this server
//     issues (aud = the MCP resource), never with the user's PushWard API key.
//   - During consent the user proves identity once with their PushWard hlk_
//     key (validated against GET /auth/me); the key is stored encrypted and
//     used server-side to call pushward-server. The key never reaches the MCP
//     client (confused-deputy / token-passthrough mitigation).
package oauth

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Token lifetimes. Access tokens are short so a leak is bounded; refresh tokens
// are long but single-use (rotated) so theft is detectable.
const (
	accessTokenTTL  = 15 * time.Minute
	authCodeTTL     = 5 * time.Minute
	refreshTokenTTL = 90 * 24 * time.Hour
	// refreshGrace tolerates a brief window where a just-rotated refresh token
	// is replayed (double-POST by some clients) without tripping theft response.
	refreshGrace = 30 * time.Second
	// janitorInterval is how often expired codes/revoked tokens are purged from
	// the store and the decrypted-credential cache is swept.
	janitorInterval = 10 * time.Minute
	// credCacheTTL bounds how long a decrypted hlk_ stays cached on the hot /mcp
	// path; short so a credential rotation/revocation propagates quickly.
	credCacheTTL = 60 * time.Second
	// credCacheMax hard-caps the decrypted-credential cache size.
	credCacheMax = 50_000
)

// Config holds the OAuth server configuration, loaded from environment.
type Config struct {
	// Issuer is the canonical https origin of this server (e.g.
	// https://mcp.pushward.app). Used as the OAuth issuer and the JWT iss/aud.
	Issuer string
	// Resource is the RFC 8707 resource identifier tokens are bound to. Defaults
	// to Issuer.
	Resource string
	// APIBaseURL is the upstream pushward-server base used to validate hlk_ keys
	// (GET /auth/me). Mirrors config.APIURL.
	APIBaseURL string
	// SigningKeyPEM is a PEM-encoded EC P-256 private key for signing JWTs.
	SigningKeyPEM string
	// HLKEncKey is the 32-byte master key (base64 std or raw) used to encrypt
	// stored hlk_ keys at rest.
	HLKEncKey string
	// DatabaseDSN, when set, selects the Postgres store; empty uses the
	// in-memory store (single-replica / development only).
	DatabaseDSN string
	// TrustProxy enables reading the client IP from forwarding headers
	// (CF-Connecting-IP, then X-Forwarded-For) for rate-limit keying. Enable
	// ONLY when the server sits behind a proxy that overwrites these headers
	// (Cloudflare + the Traefik gateway here); otherwise a client forges them to
	// mint a fresh rate-limit bucket per request. Defaults true (always hosted
	// behind the gateway); set PUSHWARD_MCP_TRUST_PROXY=false for direct exposure.
	TrustProxy bool
}

// LoadConfig reads OAuth configuration from the environment. apiBaseURL is the
// already-validated upstream API URL from the core config.
func LoadConfig(apiBaseURL string) (*Config, error) {
	cfg := &Config{
		Issuer:        strings.TrimRight(os.Getenv("PUSHWARD_MCP_ISSUER"), "/"),
		Resource:      strings.TrimRight(os.Getenv("PUSHWARD_MCP_RESOURCE"), "/"),
		APIBaseURL:    apiBaseURL,
		SigningKeyPEM: os.Getenv("PUSHWARD_MCP_SIGNING_KEY"),
		HLKEncKey:     os.Getenv("PUSHWARD_MCP_HLK_ENC_KEY"),
		DatabaseDSN:   os.Getenv("PUSHWARD_MCP_DB_DSN"),
		TrustProxy:    true,
	}
	if v := os.Getenv("PUSHWARD_MCP_TRUST_PROXY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid PUSHWARD_MCP_TRUST_PROXY %q: %w", v, err)
		}
		cfg.TrustProxy = b
	}

	if cfg.Issuer == "" {
		return nil, fmt.Errorf("PUSHWARD_MCP_ISSUER is required in http mode")
	}
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("PUSHWARD_MCP_ISSUER must be an https origin with no path, got %q", cfg.Issuer)
	}
	if cfg.Resource == "" {
		cfg.Resource = cfg.Issuer
	}
	if cfg.SigningKeyPEM == "" {
		return nil, fmt.Errorf("PUSHWARD_MCP_SIGNING_KEY (EC P-256 PEM) is required in http mode")
	}
	if cfg.HLKEncKey == "" {
		return nil, fmt.Errorf("PUSHWARD_MCP_HLK_ENC_KEY (32-byte master key) is required in http mode")
	}
	return cfg, nil
}

// Well-known discovery paths and the absolute OAuth endpoint URLs.
func (c *Config) prmPath() string  { return "/.well-known/oauth-protected-resource" }
func (c *Config) asPath() string   { return "/.well-known/oauth-authorization-server" }
func (c *Config) jwksPath() string { return "/.well-known/jwks.json" }

func (c *Config) authorizeURL() string { return c.Issuer + "/oauth/authorize" }
func (c *Config) tokenURL() string     { return c.Issuer + "/oauth/token" }
func (c *Config) registerURL() string  { return c.Issuer + "/oauth/register" }
func (c *Config) jwksURL() string      { return c.Issuer + c.jwksPath() }
