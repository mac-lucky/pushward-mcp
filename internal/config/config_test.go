package config

import (
	"strings"
	"testing"
)

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

const testE2EKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestLoad_E2EKey(t *testing.T) {
	t.Setenv("PUSHWARD_API_TOKEN", "atok")
	t.Setenv("PUSHWARD_RELAY_TOKEN", "rtok")

	t.Setenv("PUSHWARD_E2E_KEY", "")
	cfg, err := Load()
	if err != nil || cfg.E2EKey != nil {
		t.Fatalf("unset key: cfg.E2EKey = %v, err = %v", cfg.E2EKey, err)
	}

	t.Setenv("PUSHWARD_E2E_KEY", testE2EKey)
	cfg, err = Load()
	if err != nil || cfg.E2EKey == nil || cfg.E2EKey.KID() != "767c0806" {
		t.Fatalf("stdio key: cfg.E2EKey = %v, err = %v", cfg.E2EKey, err)
	}

	t.Setenv("PUSHWARD_E2E_KEY", "hlk_0123456789abcdef0123456789abcdef")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "integration key") {
		t.Fatalf("an hlk_ key must fail to load, got %v", err)
	}
}
