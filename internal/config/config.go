package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Transport selects how the MCP server communicates with clients.
type Transport string

const (
	// TransportStdio serves a single local client over stdin/stdout. This is
	// the default and is used for local development (the .mcp.json launch).
	TransportStdio Transport = "stdio"
	// TransportHTTP serves remote clients over Streamable HTTP behind OAuth.
	TransportHTTP Transport = "http"
)

// Config holds the MCP server configuration, loaded from environment variables.
type Config struct {
	APIToken   string
	RelayToken string
	APIURL     string
	RelayURL   string

	// Transport selects stdio (default) or http.
	Transport Transport
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
}

// IsRemote reports whether the server runs in network-exposed (http) mode.
func (c *Config) IsRemote() bool { return c.Transport == TransportHTTP }

// Load reads configuration from environment variables.
//
// stdio mode (default): PUSHWARD_API_TOKEN and PUSHWARD_RELAY_TOKEN are
// required (single shared identity, local use). http mode: tokens arrive per
// request via OAuth, so PUSHWARD_API_TOKEN is optional and PUSHWARD_RELAY_TOKEN
// is required only when relay tools are enabled.
//
// PUSHWARD_API_URL defaults to https://api.pushward.app and
// PUSHWARD_RELAY_URL to https://relay.pushward.app. Upstream URLs must be https
// unless the host is loopback (local development).
func Load() (*Config, error) {
	cfg := &Config{
		APIToken:    os.Getenv("PUSHWARD_API_TOKEN"),
		RelayToken:  os.Getenv("PUSHWARD_RELAY_TOKEN"),
		APIURL:      os.Getenv("PUSHWARD_API_URL"),
		RelayURL:    os.Getenv("PUSHWARD_RELAY_URL"),
		ListenAddr:  os.Getenv("PUSHWARD_MCP_LISTEN_ADDR"),
		MetricsAddr: os.Getenv("PUSHWARD_MCP_METRICS_ADDR"),
	}

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
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = ":8080"
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
	if cfg.RelayEnabled && cfg.RelayToken == "" {
		return nil, fmt.Errorf("PUSHWARD_RELAY_TOKEN is required when relay tools are enabled")
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
