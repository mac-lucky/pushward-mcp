package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
	"github.com/mac-lucky/pushward-mcp/internal/config"
	"github.com/mac-lucky/pushward-mcp/internal/httpserve"
	"github.com/mac-lucky/pushward-mcp/internal/oauth"
	"github.com/mac-lucky/pushward-mcp/internal/observability"
	"github.com/mac-lucky/pushward-mcp/internal/tools"
)

// Build metadata, injected via -ldflags at build time (see Dockerfile).
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Redact upstream error detail from caller-facing errors when exposed to a
	// network (http mode).
	client.SetRemoteMode(cfg.IsRemote())

	log := observability.NewLogger(slog.LevelInfo)
	if cfg.IsRemote() {
		log.Info("pushward-mcp starting", "version", version, "commit", commit, "buildDate", buildDate, "transport", string(cfg.Transport))
	}

	apiClient := client.NewAPIClient(cfg.APIURL, cfg.APIToken)
	// relayClient stays nil when relay tools are disabled (http/remote default),
	// so a multi-tenant endpoint never carries the shared relay credential.
	var relayClient *client.RelayClient
	if cfg.RelayEnabled {
		relayClient = client.NewRelayClient(cfg.RelayURL, cfg.RelayToken)
	}

	s := server.NewMCPServer(
		"pushward",
		"1.0.0",
		server.WithToolCapabilities(false),
		// Convert a panic in any tool handler into an error result instead of
		// crashing the process and killing the agent's session.
		server.WithRecovery(),
		server.WithHooks(observability.LoggingHooks(log)),
		server.WithInstructions("PushWard MCP server for testing push notifications. "+
			"Use API tools to manage activities and notifications on api.pushward.app. "+
			"Use relay tools to simulate external service webhooks on relay.pushward.app. "+
			"Use composite test_ tools for common test workflows. "+
			"Before writing any code that integrates with PushWard, call get_pushward_docs "+
			"(start with kind=index) and get_pushward_best_practices to load the API reference, "+
			"OpenAPI schemas, and integration best practices into context."),
	)

	tools.RegisterAll(s, apiClient, relayClient)

	switch cfg.Transport {
	case config.TransportStdio:
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "server: %v\n", err)
			os.Exit(1)
		}
	case config.TransportHTTP:
		if err := runHTTP(cfg, s, log); err != nil {
			log.Error("http server exited", "err", err)
			os.Exit(1)
		}
	}
}

func runHTTP(cfg *config.Config, s *server.MCPServer, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	oauthCfg, err := oauth.LoadConfig(cfg.APIURL)
	if err != nil {
		return err
	}
	provider, err := oauth.New(ctx, oauthCfg, log)
	if err != nil {
		return err
	}
	defer provider.Close()

	return httpserve.Run(ctx, cfg, s, provider, log)
}
