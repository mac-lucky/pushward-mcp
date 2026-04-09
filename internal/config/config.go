package config

import (
	"fmt"
	"os"
)

// Config holds the MCP server configuration, loaded from environment variables.
type Config struct {
	APIToken   string
	RelayToken string
	APIURL     string
	RelayURL   string
}

// Load reads configuration from environment variables.
// PUSHWARD_API_TOKEN and PUSHWARD_RELAY_TOKEN are required.
// PUSHWARD_API_URL defaults to https://api.pushward.app.
// PUSHWARD_RELAY_URL defaults to https://relay.pushward.app.
func Load() (*Config, error) {
	cfg := &Config{
		APIToken:   os.Getenv("PUSHWARD_API_TOKEN"),
		RelayToken: os.Getenv("PUSHWARD_RELAY_TOKEN"),
		APIURL:     os.Getenv("PUSHWARD_API_URL"),
		RelayURL:   os.Getenv("PUSHWARD_RELAY_URL"),
	}

	if cfg.APIToken == "" {
		return nil, fmt.Errorf("PUSHWARD_API_TOKEN is required")
	}
	if cfg.RelayToken == "" {
		return nil, fmt.Errorf("PUSHWARD_RELAY_TOKEN is required")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.pushward.app"
	}
	if cfg.RelayURL == "" {
		cfg.RelayURL = "https://relay.pushward.app"
	}

	return cfg, nil
}
