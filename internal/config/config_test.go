package config

import "testing"

func TestLoad_RequiresAPIToken(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when PUSHWARD_API_TOKEN is missing")
	}
}

func TestLoad_RequiresRelayToken(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when PUSHWARD_RELAY_TOKEN is missing")
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	t.Setenv("PUSHWARD_API_URL", "")
	t.Setenv("PUSHWARD_RELAY_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIURL != "https://api.pushward.app" {
		t.Errorf("APIURL = %q, want https://api.pushward.app", cfg.APIURL)
	}
	if cfg.RelayURL != "https://relay.pushward.app" {
		t.Errorf("RelayURL = %q, want https://relay.pushward.app", cfg.RelayURL)
	}
}

func TestLoad_OverridesAndTokens(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")
	t.Setenv("PUSHWARD_API_URL", "http://localhost:8080")
	t.Setenv("PUSHWARD_RELAY_URL", "http://localhost:9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APIURL != "http://localhost:8080" {
		t.Errorf("APIURL = %q, want override", cfg.APIURL)
	}
	if cfg.RelayURL != "http://localhost:9090" {
		t.Errorf("RelayURL = %q, want override", cfg.RelayURL)
	}
	if cfg.APIToken != "atok" || cfg.RelayToken != "rtok" {
		t.Errorf("tokens not loaded correctly: %+v", cfg)
	}
}
