package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/docs"
)

// bestPracticeTopics are the get_pushward_best_practices "topic" enum values.
// They map 1:1 to H2 headings in assets/best-practices.md (e.g. "## integration"),
// so a topic is passed straight to docs.Section — no mapping table to drift.
var bestPracticeTopics = []string{"integration", "live-activity", "relay-provider", "email"}

// registerDocsTools registers the embedded-content reference tools. These serve
// bundled docs/specs and need no API or relay client.
func registerDocsTools(s *mcpserver.MCPServer) {
	s.AddTool(
		mcp.NewTool("get_pushward_docs",
			mcp.WithDescription("Fetch official PushWard reference material: the LLM docs bundle and the API/relay OpenAPI specs. "+
				"Call this BEFORE writing any code that integrates with PushWard, creates Live Activities, sends notifications, or posts to the relay. "+
				"Start with kind=\"index\" to see what exists, then pull kind=\"full\" (optionally narrowed with section) for guides and examples, "+
				"or kind=\"api_openapi\" / kind=\"relay_openapi\" for exact request/response schemas. Content is embedded and offline-safe."),
			mcp.WithReadOnlyHintAnnotation(true),
			// Serves go:embed bundled content — no network, closed domain.
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("kind",
				mcp.Required(),
				mcp.Enum("index", "full", "api_openapi", "relay_openapi"),
				mcp.Description("Which document to return. \"index\" = short llms.txt map of all docs (start here); "+
					"\"full\" = complete llms-full.txt guide bundle (large — narrow it with section); "+
					"\"api_openapi\" = api.pushward.app OpenAPI spec (YAML); "+
					"\"relay_openapi\" = relay.pushward.app OpenAPI spec (JSON)."),
			),
			mcp.WithString("section",
				mcp.Description("Optional. For kind=\"index\" or kind=\"full\": return only the section whose heading matches this name "+
					"(case-insensitive substring or anchor slug, e.g. \"Countdown Template\" or \"live-activities\"), up to the next heading "+
					"of equal-or-higher level. Ignored for the OpenAPI kinds. On no match, the response lists the available headings."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleGetPushwardDocs(req)
		},
	)

	s.AddTool(
		mcp.NewTool("get_pushward_best_practices",
			mcp.WithDescription("Return PushWard's curated best-practices guide for writing correct integration code. "+
				"Call this BEFORE implementing a PushWard integration, a Live Activity, or a relay provider hookup. "+
				"Omit topic for the whole guide, or pass a topic to get just that part."),
			mcp.WithReadOnlyHintAnnotation(true),
			// Serves go:embed bundled content — no network, closed domain.
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("topic",
				mcp.Enum(bestPracticeTopics...),
				mcp.Description("Optional. Narrow to one area: \"integration\" (general API integration), "+
					"\"live-activity\" (Live Activity content, templates, lifecycle), "+
					"\"relay-provider\" (wiring external-service webhooks through the relay), "+
					"\"email\" (sending transactional email via POST /emails). Omit for the full guide."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleGetPushwardBestPractices(req)
		},
	)
}

func handleGetPushwardDocs(req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	kindStr, err := req.RequireString("kind")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	kind := docs.Kind(kindStr)
	doc, err := docs.Doc(kind)
	if err != nil {
		// mcp-go does not enforce the enum server-side, so validate here.
		return mcp.NewToolResultError(fmt.Sprintf("invalid kind %q; valid values: %s",
			kindStr, strings.Join(docs.Kinds(), ", "))), nil
	}

	section := req.GetString("section", "")
	if section == "" || !docs.IsMarkdown(kind) {
		return mcp.NewToolResultText(doc), nil
	}
	return mcp.NewToolResultText(sliceOrGuide(doc, section)), nil
}

func handleGetPushwardBestPractices(req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc, err := docs.BestPractices()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	topic := req.GetString("topic", "")
	if topic == "" {
		return mcp.NewToolResultText(doc), nil
	}
	return mcp.NewToolResultText(sliceOrGuide(doc, topic)), nil
}

// sliceOrGuide returns the requested section of a Markdown doc, or — when no
// heading matches — a short note listing the available headings so the caller
// can retry with a valid name.
func sliceOrGuide(doc, section string) string {
	text, ok, topLevel := docs.Section(doc, section)
	if ok {
		return text
	}
	return fmt.Sprintf("No section matching %q. Available headings:\n- %s",
		section, strings.Join(topLevel, "\n- "))
}
