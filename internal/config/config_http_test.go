package config

import (
	"strings"
	"testing"
)

func TestLoad_HTTPModeAPITokenOptional(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "")
	t.Setenv("PUSHWARD_API_TOKEN", "")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("http mode should not require API token: %v", err)
	}
	if cfg.Transport != TransportHTTP || !cfg.IsRemote() {
		t.Fatalf("expected http transport, got %q", cfg.Transport)
	}
	if cfg.RelayEnabled {
		t.Fatal("relay must default off in http mode")
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("default ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
}

func TestLoad_RejectsNonHTTPSUpstream(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	t.Setenv("PUSHWARD_API_URL", "http://api.evil.example")
	if _, err := Load(); err == nil {
		t.Fatal("expected rejection of non-https non-loopback upstream URL")
	}
}

func TestLoad_AllowsLoopbackHTTP(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	t.Setenv("PUSHWARD_API_URL", "http://127.0.0.1:8080")
	t.Setenv("PUSHWARD_RELAY_URL", "http://localhost:9090")
	if _, err := Load(); err != nil {
		t.Fatalf("loopback http should be allowed: %v", err)
	}
}

func TestLoad_RejectsBadTransport(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "websocket")
	if _, err := Load(); err == nil {
		t.Fatal("expected rejection of unknown transport")
	}
}

func TestLoad_HTTPRelayEnabledRequiresRelayToken(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_API_TOKEN", "")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "")
	t.Setenv("PUSHWARD_MCP_RELAY_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("enabling relay without a relay token should fail")
	}
}

func TestLoad_HTTPModeDropsAPITokenUnderOAuth(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "")
	t.Setenv("PUSHWARD_API_TOKEN", "hlk_shared")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAuth != HTTPAuthOAuth || cfg.SingleUser() {
		t.Fatalf("HTTPAuth = %q, want oauth by default", cfg.HTTPAuth)
	}
	if cfg.APIToken != "" {
		t.Fatal("OAuth mode must drop a process-wide PUSHWARD_API_TOKEN")
	}
}

func TestLoad_HTTPSingleUserKeepsAPIToken(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "none")
	t.Setenv("PUSHWARD_API_TOKEN", "hlk_owner")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SingleUser() {
		t.Fatal("expected single-user mode")
	}
	if cfg.APIToken != "hlk_owner" {
		t.Fatalf("APIToken = %q, want it kept in single-user mode", cfg.APIToken)
	}
	if cfg.RelayEnabled {
		t.Fatal("relay must still default off in http mode")
	}
}

func TestLoad_HTTPSingleUserRequiresAPIToken(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "none")
	t.Setenv("PUSHWARD_API_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("single-user mode without PUSHWARD_API_TOKEN should fail")
	}
}

func TestLoad_RejectsBadHTTPAuth(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "basic")
	// With a token set, only the unknown value itself can fail the load, so a
	// fail-open fallback to none would pass Load and fail this test.
	t.Setenv("PUSHWARD_API_TOKEN", "hlk_x")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PUSHWARD_MCP_HTTP_AUTH") {
		t.Fatalf("err = %v, want rejection of unknown PUSHWARD_MCP_HTTP_AUTH", err)
	}
}

func TestLoad_HTTPSingleUserRefusesOAuthSettings(t *testing.T) {
	for _, k := range []string{"PUSHWARD_MCP_ISSUER", "PUSHWARD_MCP_SIGNING_KEY", "PUSHWARD_MCP_DB_DSN"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
			t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "none")
			t.Setenv("PUSHWARD_API_TOKEN", "hlk_owner")
			t.Setenv(k, "set")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Fatalf("err = %v, want refusal naming %s", err, k)
			}
		})
	}
}

func TestLoad_HTTPSingleUserListenAddr(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "none")
	t.Setenv("PUSHWARD_API_TOKEN", "hlk_owner")
	t.Setenv("PUSHWARD_MCP_LISTEN_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("default ListenAddr = %q, want loopback in single-user mode", cfg.ListenAddr)
	}

	t.Setenv("PUSHWARD_MCP_LISTEN_ADDR", ":8080")
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("explicit ListenAddr = %q, want :8080 kept", cfg.ListenAddr)
	}
}

func TestRedactUpstreamErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"stdio", Config{Transport: TransportStdio}, false},
		{"http oauth", Config{Transport: TransportHTTP, HTTPAuth: HTTPAuthOAuth}, true},
		{"http single-user", Config{Transport: TransportHTTP, HTTPAuth: HTTPAuthNone}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.RedactUpstreamErrors(); got != tc.want {
			t.Errorf("%s: RedactUpstreamErrors() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoad_StdioIgnoresHTTPAuth(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "")
	t.Setenv("PUSHWARD_MCP_HTTP_AUTH", "none")
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SingleUser() {
		t.Fatal("stdio mode is never single-user http")
	}
}
