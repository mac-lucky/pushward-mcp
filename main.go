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
		// Convert a panic in any tool handler into an error result instead of
		// crashing the stdio process and killing the agent's session.
		server.WithRecovery(),
		server.WithInstructions("PushWard MCP server for testing push notifications. "+
			"Use API tools to manage activities and notifications on api.pushward.app. "+
			"Use relay tools to simulate external service webhooks on relay.pushward.app. "+
			"Use composite test_ tools for common test workflows. "+
			"Before writing any code that integrates with PushWard, call get_pushward_docs "+
			"(start with kind=index) and get_pushward_best_practices to load the API reference, "+
			"OpenAPI schemas, and integration best practices into context."),
	)

	tools.RegisterAll(s, apiClient, relayClient)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
}
