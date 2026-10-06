//go:generate go run ../../cmd/generate

package tools

import (
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// RegisterAll registers all MCP tools: API (generated), relay (generated),
// composite, acknowledged-notification receipts and integration key management. A nil relay (the default in http/remote mode) skips the relay tools
// and the relay-dependent composite tools, so a multi-tenant endpoint never
// exposes the shared server-side relay credential.
func RegisterAll(s *mcpserver.MCPServer, api *client.APIClient, relay *client.RelayClient) {
	registerAPITools(s, api)
	if relay != nil {
		registerRelayTools(s, relay)
	}
	registerCompositeTools(s, api, relay)
	registerReceiptTools(s, api)
	registerIntegrationKeyTools(s, api)
	registerDocsTools(s)
}
