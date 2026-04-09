package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
	"github.com/mac-lucky/pushward-mcp/internal/config"
	"github.com/mac-lucky/pushward-mcp/internal/tools"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	apiClient := client.NewAPIClient(cfg.APIURL, cfg.APIToken)
	relayClient := client.NewRelayClient(cfg.RelayURL, cfg.RelayToken)

	s := server.NewMCPServer(
		"pushward",
		"1.0.0",
		server.WithToolCapabilities(false),
		server.WithInstructions("PushWard MCP server for testing push notifications. "+
			"Use API tools to manage activities and notifications on api.pushward.app. "+
			"Use relay tools to simulate external service webhooks on relay.pushward.app. "+
			"Use composite test_ tools for common test workflows."),
	)

	tools.RegisterAll(s, apiClient, relayClient)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
}
