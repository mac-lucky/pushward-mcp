package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/mac-lucky/pushward-mcp/internal/e2e"
)

// Transport selects how the MCP server communicates with clients.
type Transport string

const (
	// TransportStdio serves a single local client over stdin/stdout. This is
	// the default and is used for local development (the .mcp.json launch).
	TransportStdio Transport = "stdio"
	// TransportHTTP serves remote clients over Streamable HTTP, behind OAuth
	// unless PUSHWARD_MCP_HTTP_AUTH=none.
	TransportHTTP Transport = "http"
)

// HTTPAuth selects how http mode authenticates its callers.
type HTTPAuth string

const (
	// HTTPAuthOAuth is the multi-tenant default: every caller signs in through
	// OAuth 2.1 and acts as the PushWard key they entered on the consent page.
	HTTPAuthOAuth HTTPAuth = "oauth"
	// HTTPAuthNone is single-user mode: no caller login, every request acts as
	// PUSHWARD_API_TOKEN. For an agent that can't run an interactive OAuth
	// flow, reachable only through a network policy or loopback; never expose
	// it publicly.
	HTTPAuthNone HTTPAuth = "none"
)

// Config holds the MCP server configuration, loaded from environment variables.
type Config struct {
	APIToken   string
	RelayToken string
	APIURL     string
	RelayURL   string

	// Transport selects stdio (default) or http.
	Transport Transport
	// HTTPAuth selects OAuth (default) or no caller auth in http mode. Unset
	// in stdio mode.
	HTTPAuth HTTPAuth
	// ListenAddr is the HTTP listen address in http mode (default ":8080").
	ListenAddr string
	// MetricsAddr, when non-empty, serves Prometheus metrics on a separate,
	// pod-private listener (e.g. ":9090"). Empty disables the metrics server.
	MetricsAddr string
	// RelayEnabled controls whether relay tools are registered. Defaults to
	// true in stdio mode and false in http mode (a multi-tenant endpoint must
	// not carry a shared relay credential); override with
	// PUSHWARD_MCP_RELAY_ENABLED.
	RelayEnabled bool
	// E2EKey, from PUSHWARD_E2E_KEY, seals notification title, subtitle,
	// body and url end to end. Nil when unset, and always nil in OAuth http
	// mode.
	E2EKey *e2e.Key
}

// IsRemote reports whether the server runs in network-exposed (http) mode.
func (c *Config) IsRemote() bool { return c.Transport == TransportHTTP }

// SingleUser reports whether http mode serves one fixed identity
// (PUSHWARD_MCP_HTTP_AUTH=none) instead of per-user OAuth.
func (c *Config) SingleUser() bool {
	return c.Transport == TransportHTTP && c.HTTPAuth == HTTPAuthNone
}

// RedactUpstreamErrors reports whether caller-facing errors must hide upstream
// Problem detail. Only OAuth http mode redacts: its callers are other users of
// a public endpoint. Single-user and stdio callers act as the key owner and
// would see the same detail calling the API directly.
func (c *Config) RedactUpstreamErrors() bool {
	return c.IsRemote() && !c.SingleUser()
}

// Load reads configuration from environment variables.
//
// stdio mode (default): PUSHWARD_API_TOKEN and PUSHWARD_RELAY_TOKEN are
// required (single shared identity, local use). http mode: tokens arrive per
// request via OAuth, so PUSHWARD_API_TOKEN is ignored and PUSHWARD_RELAY_TOKEN
// is required only when relay tools are enabled. http mode with
// PUSHWARD_MCP_HTTP_AUTH=none keeps PUSHWARD_API_TOKEN and requires it.
//
// PUSHWARD_API_URL defaults to https://api.pushward.app and
// PUSHWARD_RELAY_URL to https://relay.pushward.app. Upstream URLs must be https
// unless the host is loopback (local development).
//
// PUSHWARD_E2E_KEY (optional, 64 hex characters) turns on end-to-end
// encryption in stdio and single-user http mode; OAuth http mode ignores it
// like PUSHWARD_API_TOKEN.
func Load() (*Config, error) {
	cfg := &Config{
		APIToken:    os.Getenv("PUSHWARD_API_TOKEN"),
		RelayToken:  os.Getenv("PUSHWARD_RELAY_TOKEN"),
		APIURL:      os.Getenv("PUSHWARD_API_URL"),
		RelayURL:    os.Getenv("PUSHWARD_RELAY_URL"),
		ListenAddr:  os.Getenv("PUSHWARD_MCP_LISTEN_ADDR"),
		MetricsAddr: os.Getenv("PUSHWARD_MCP_METRICS_ADDR"),
	}
	e2eKey := os.Getenv("PUSHWARD_E2E_KEY")

	switch t := strings.ToLower(strings.TrimSpace(os.Getenv("PUSHWARD_MCP_TRANSPORT"))); t {
	case "", string(TransportStdio):
		cfg.Transport = TransportStdio
	case string(TransportHTTP):
		cfg.Transport = TransportHTTP
	default:
		return nil, fmt.Errorf("invalid PUSHWARD_MCP_TRANSPORT %q (want stdio or http)", t)
	}

	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.pushward.app"
	}
	if cfg.RelayURL == "" {
		cfg.RelayURL = "https://relay.pushward.app"
	}
	if err := validateUpstreamURL("PUSHWARD_API_URL", cfg.APIURL); err != nil {
		return nil, err
	}
	if err := validateUpstreamURL("PUSHWARD_RELAY_URL", cfg.RelayURL); err != nil {
		return nil, err
	}

	// Relay default depends on transport; explicit env overrides either way.
	cfg.RelayEnabled = cfg.Transport == TransportStdio
	if v := os.Getenv("PUSHWARD_MCP_RELAY_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid PUSHWARD_MCP_RELAY_ENABLED %q: %w", v, err)
		}
		cfg.RelayEnabled = b
	}

	if cfg.Transport == TransportHTTP {
		switch a := strings.ToLower(strings.TrimSpace(os.Getenv("PUSHWARD_MCP_HTTP_AUTH"))); a {
		case "", string(HTTPAuthOAuth):
			cfg.HTTPAuth = HTTPAuthOAuth
		case string(HTTPAuthNone):
			cfg.HTTPAuth = HTTPAuthNone
		default:
			return nil, fmt.Errorf("invalid PUSHWARD_MCP_HTTP_AUTH %q (want oauth or none)", a)
		}
		if cfg.HTTPAuth == HTTPAuthNone {
			// A deployment carrying OAuth settings is a hosted one; turning its
			// auth off must be loud, not a silent open endpoint.
			for _, k := range []string{"PUSHWARD_MCP_ISSUER", "PUSHWARD_MCP_SIGNING_KEY", "PUSHWARD_MCP_DB_DSN"} {
				if os.Getenv(k) != "" {
					return nil, fmt.Errorf("PUSHWARD_MCP_HTTP_AUTH=none refuses to start with %s set", k)
				}
			}
		}
		if cfg.HTTPAuth == HTTPAuthOAuth {
			// OAuth mode is multi-tenant: per-user tokens arrive via OAuth and
			// ride in the request context. Drop any process-wide
			// PUSHWARD_API_TOKEN so it can never become a silent shared fallback
			// credential used as every user's identity (an env copy-paste
			// foot-gun) if a request ever reaches the API client without a
			// context token.
			cfg.APIToken = ""
			// Same for the encryption key: every user's notifications would
			// be sealed to the devices of whoever set it.
			e2eKey = ""
		}
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = ":8080"
			if cfg.HTTPAuth == HTTPAuthNone {
				// Nothing checks the caller, so only listen beyond loopback
				// when asked to.
				cfg.ListenAddr = "127.0.0.1:8080"
			}
		}
		if err := validateHostPort("PUSHWARD_MCP_LISTEN_ADDR", cfg.ListenAddr); err != nil {
			return nil, err
		}
		if cfg.MetricsAddr != "" {
			if err := validateHostPort("PUSHWARD_MCP_METRICS_ADDR", cfg.MetricsAddr); err != nil {
				return nil, err
			}
		}
	}

	// Token requirements depend on mode.
	if cfg.Transport == TransportStdio && cfg.APIToken == "" {
		return nil, fmt.Errorf("PUSHWARD_API_TOKEN is required in stdio mode")
	}
	if cfg.SingleUser() && cfg.APIToken == "" {
		return nil, fmt.Errorf("PUSHWARD_API_TOKEN is required when PUSHWARD_MCP_HTTP_AUTH=none")
	}
	if cfg.RelayEnabled && cfg.RelayToken == "" {
		return nil, fmt.Errorf("PUSHWARD_RELAY_TOKEN is required when relay tools are enabled")
	}

	if e2eKey != "" {
		k, err := e2e.ParseKey(e2eKey)
		if err != nil {
			return nil, fmt.Errorf("invalid PUSHWARD_E2E_KEY: %w", err)
		}
		cfg.E2EKey = k
	}

	return cfg, nil
}

// validateUpstreamURL requires a parseable absolute URL whose scheme is https,
// allowing http only for loopback hosts (local development / tests).
func validateUpstreamURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL with a host, got %q", name, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s must use https for non-loopback host %q", name, u.Hostname())
	default:
		return fmt.Errorf("%s has unsupported scheme %q (want https)", name, u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// validateHostPort checks that addr parses as a "host:port" listen address.
func validateHostPort(name, addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("%s %q is not a valid host:port: %w", name, addr, err)
	}
	return nil
}
