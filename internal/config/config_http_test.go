package config

import "testing"

func TestLoad_HTTPModeAPITokenOptional(t *testing.T) {
	t.Setenv("PUSHWARD_MCP_TRANSPORT", "http")
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
