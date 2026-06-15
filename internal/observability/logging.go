// Package observability provides structured logging hooks and helpers for the
// MCP server. It deliberately never logs credentials (the per-user PushWard
// token or any Authorization header) — only tool names, outcomes, and the
// authenticated user id (the OAuth subject) for an audit trail.
package observability

import (
	"context"
	"log/slog"
	"os"

	mcp "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// NewLogger returns a JSON slog logger at the given level written to stderr.
// stderr is used so stdio-mode JSON-RPC on stdout is never corrupted by logs.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// LoggingHooks returns mcp-go server hooks that emit one structured audit line
// per tool call (name, outcome, user id) without ever touching arguments or
// credentials. Arguments are intentionally omitted because tool inputs can
// contain user content; the credential never appears in args (it travels in
// the context), so this is safe and minimal.
func LoggingHooks(log *slog.Logger) *mcpserver.Hooks {
	h := &mcpserver.Hooks{}

	h.AddBeforeCallTool(func(ctx context.Context, _ any, req *mcp.CallToolRequest) {
		log.LogAttrs(ctx, slog.LevelInfo, "tool.call",
			slog.String("tool", req.Params.Name),
			slog.String("user", client.UserIDFromContext(ctx)),
		)
	})

	h.AddOnError(func(ctx context.Context, _ any, method mcp.MCPMethod, _ any, err error) {
		log.LogAttrs(ctx, slog.LevelError, "mcp.error",
			slog.String("method", string(method)),
			slog.String("user", client.UserIDFromContext(ctx)),
			slog.String("err", err.Error()),
		)
	})

	return h
}
