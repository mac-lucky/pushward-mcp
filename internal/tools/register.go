//go:generate go run ../../cmd/generate

package tools

import (
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// RegisterAll registers all MCP tools: API (generated), relay (generated), and composite.
func RegisterAll(s *mcpserver.MCPServer, api *client.APIClient, relay *client.RelayClient) {
	registerAPITools(s, api)
	registerRelayTools(s, relay)
	registerCompositeTools(s, api, relay)
	registerDocsTools(s)
}
