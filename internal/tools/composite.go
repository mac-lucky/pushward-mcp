package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

const truncationNote = "\n\nNote: result truncated at the pagination cap (" +
	"~2000 activities). Narrow your filter or list in smaller batches to " +
	"see the rest."

const (
	stateOngoing   = "ongoing"
	stateEnded     = "ended"
	statePreempted = "preempted"
)

// Activity content template names. defaultTemplate is the lifecycle tool's
// fallback when the caller gives none.
const (
	tmplGeneric   = "generic"
	tmplCountdown = "countdown"
	tmplSteps     = "steps"
	tmplAlert     = "alert"
	tmplGauge     = "gauge"
	tmplTimeline  = "timeline"
	tmplBoard     = "board"
	tmplLog       = "log"

	defaultTemplate = tmplGeneric
)

// lifecycleTemplates is the set of content templates the test_activity_lifecycle
// tool advertises and that buildTestContent knows how to populate. Single source
// so the enum, the switch, and the parity test can't drift — mirrors the
// relayTestProviders pattern.
var lifecycleTemplates = []string{
	tmplGeneric, tmplCountdown, tmplSteps, tmplAlert, tmplGauge, tmplTimeline,
	tmplBoard, tmplLog,
}

// relayTestProviders is the set of providers accepted by the test_relay_provider
// tool's enum. Every entry must have a fixture in testPayloads — enforced by
// TestTestPayloads_AllProvidersPresent so the two lists can never drift.
var relayTestProviders = []string{
	"argocd", "backrest", "bazarr", "changedetection", "gatus",
	"grafana", "jellyfin", "overseerr", "paperless", "prowlarr",
	"proxmox", "radarr", "sonarr", "unmanic", "uptimekuma",
}

func registerCompositeTools(s *mcpserver.MCPServer, api *client.APIClient, relay *client.RelayClient) {
	// relay is nil in http/remote mode; derive the gate from it so the enable
	// state has a single source (the presence of a relay client).
	relayEnabled := relay != nil
	healthDesc := "Check health of the PushWard API endpoint"
	if relayEnabled {
		healthDesc = "Check health of both PushWard API and Relay endpoints"
	}
	// test_health
	s.AddTool(
		mcp.NewTool("test_health",
			mcp.WithDescription(healthDesc),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleTestHealth(ctx, api, relay)
		},
	)

	// get_ready — kept as a composite because /ready is filtered out of the
	// public OpenAPI spec the generator consumes (PublicOperationIDs in
	// pushward-server/internal/api/openapi_filter.go).
	s.AddTool(
		mcp.NewTool("get_ready",
			mcp.WithDescription("Check API readiness (GET /ready)"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			raw, err := api.GetReady(ctx)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(string(raw)), nil
		},
	)

	// test_activity_lifecycle
	s.AddTool(
		mcp.NewTool("test_activity_lifecycle",
			mcp.WithDescription("Run a full activity lifecycle: create -> start (ongoing) -> end (ended) -> verify -> optionally delete"),
			// Deletes the activity it created (cleanup defaults to true), so the
			// destructive default is correct here; keep it explicit alongside the
			// idempotent/open-world hints.
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("slug",
				mcp.Required(),
				mcp.Description("Activity slug (must be unique)"),
			),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("Human-readable activity name"),
			),
			mcp.WithString("template",
				mcp.Description("Content template (default: generic)"),
				mcp.Enum(lifecycleTemplates...),
			),
			mcp.WithBoolean("cleanup",
				mcp.Description("Delete the activity after test (default: true)"),
			),
			mcp.WithString("tap_action_url",
				mcp.Description("Optional. If set, the test content includes tap_action: {url: ...} so the lifecycle exercises Live Activity tap routing (foreground HTTPS URLs open in-app; custom schemes route cross-app)."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleTestActivityLifecycle(ctx, req, api)
		},
	)

	// test_notification
	s.AddTool(
		mcp.NewTool("test_notification",
			mcp.WithDescription("Create a test notification with standard fields"),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Notification title"),
			),
			mcp.WithString("body",
				mcp.Required(),
				mcp.Description("Notification body text"),
			),
			mcp.WithBoolean("push",
				mcp.Description("Whether to send an APNs push (default: false)"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleTestNotification(ctx, req, api)
		},
	)

	// test_email
	s.AddTool(
		mcp.NewTool("test_email",
			mcp.WithDescription("Send a test transactional email (plain text) to a verified recipient. The `to` address must already be a verified, non-unsubscribed recipient of the account."),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("to",
				mcp.Required(),
				mcp.Description("Recipient address. Must be a verified, non-unsubscribed recipient of the calling account."),
			),
			mcp.WithString("subject",
				mcp.Description("Email subject (default: \"PushWard MCP test email\")"),
			),
			mcp.WithString("body",
				mcp.Description("Plain-text body (default: \"This is a test email from the PushWard MCP server.\")"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleTestEmail(ctx, req, api)
		},
	)

	// test_relay_provider — only registered when relay tools are enabled.
	if relayEnabled {
		s.AddTool(
			mcp.NewTool("test_relay_provider",
				mcp.WithDescription("Send a standard test payload to a relay provider and verify the response"),
				mcp.WithDestructiveHintAnnotation(false),
				mcp.WithOpenWorldHintAnnotation(true),
				mcp.WithString("provider",
					mcp.Required(),
					mcp.Description("Relay provider name"),
					mcp.Enum(relayTestProviders...),
				),
			),
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return handleTestRelayProvider(ctx, req, relay)
			},
		)
	}

	// end_activity
	s.AddTool(
		mcp.NewTool("end_activity",
			mcp.WithDescription("End an activity by slug. Fetches the current content to preserve the template, then transitions to ended state."),
			// A state transition, not a data deletion; re-ending an ended activity
			// is a no-op, so this is non-destructive and idempotent.
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("slug",
				mcp.Required(),
				mcp.Description("Activity slug to end"),
			),
			mcp.WithString("reason",
				mcp.Description("Short reason shown as the activity's final state text (default: \"Ended manually\")"),
			),
			mcp.WithString("content_json",
				mcp.Description("Optional full content JSON override. If omitted, the activity's existing content is preserved with only the state text updated."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleEndActivity(ctx, req, api)
		},
	)

	// get_activity (enhanced, replaces generated version) — adds
	// include_log_backlog, which the generator can't express because it ignores
	// OpenAPI query parameters.
	s.AddTool(
		mcp.NewTool("get_activity",
			mcp.WithDescription("Get a single activity by slug. Set include_log_backlog=true to also return the server-accumulated rolling log history (log template only; sent as ?include=log_backlog)."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("slug",
				mcp.Required(),
				mcp.Description("Activity slug"),
			),
			mcp.WithBoolean("include_log_backlog",
				mcp.Description("Include the server-owned rolling log_backlog (newest-first, up to 1000 lines) for log-template activities. Omitted by default."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleGetActivity(ctx, req, api)
		},
	)

	// list_activities (enhanced, replaces generated version)
	s.AddTool(
		mcp.NewTool("list_activities",
			mcp.WithDescription("List activities with optional filtering and summary mode. Walks the server's cursor pagination automatically and returns the aggregated result (capped at ~2000 activities). Without parameters, returns the full JSON for every page concatenated."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("state",
				mcp.Description("Filter by activity state"),
				mcp.Enum(stateOngoing, stateEnded, statePreempted),
			),
			mcp.WithString("source",
				mcp.Description("Filter by source (matches slug prefix, e.g. \"grafana\" matches slugs starting with \"grafana-\" or \"grafana_\")"),
			),
			mcp.WithBoolean("summary",
				mcp.Description("Return compact summary instead of full JSON (slug, name, state, priority, age, template)"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum number of activities to return"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleListActivities(ctx, req, api)
		},
	)

	// bulk_end_activities
	s.AddTool(
		mcp.NewTool("bulk_end_activities",
			mcp.WithDescription("End multiple activities matching filters. At least one filter is required. Defaults to dry-run — pass confirm=true to actually end matching activities."),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("state",
				mcp.Description("Filter by current state. \"ended\" is omitted because already-ended activities are skipped, making it a no-op filter."),
				mcp.Enum(stateOngoing, statePreempted),
			),
			mcp.WithString("source",
				mcp.Description("Filter by source (slug prefix match, e.g. \"grafana\")"),
			),
			mcp.WithString("slug_prefix",
				mcp.Description("Filter by explicit slug prefix"),
			),
			mcp.WithString("reason",
				mcp.Description("Reason text for all ended activities (default: \"Ended in bulk\")"),
			),
			mcp.WithBoolean("confirm",
				mcp.Description("Must be true to actually end matching activities. When false (default), returns a dry-run preview of how many activities would be ended."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleBulkEndActivities(ctx, req, api)
		},
	)
}

func handleTestHealth(ctx context.Context, api *client.APIClient, relay *client.RelayClient) (*mcp.CallToolResult, error) {
	var results []string

	apiRaw, err := api.GetHealth(ctx)
	if err != nil {
		results = append(results, fmt.Sprintf("API: FAIL (%v)", err))
	} else {
		results = append(results, fmt.Sprintf("API: %s", string(apiRaw)))
	}

	// relay is nil when relay tools are disabled (http/remote mode) — only the
	// API health is reported in that case.
	if relay != nil {
		relayRaw, _, err := relay.Base.DoJSON(ctx, "GET", "/health", nil)
		if err != nil {
			results = append(results, fmt.Sprintf("Relay: FAIL (%v)", err))
		} else {
			results = append(results, fmt.Sprintf("Relay: %s", string(relayRaw)))
		}
	}

	return mcp.NewToolResultText(strings.Join(results, "\n")), nil
}

func handleTestActivityLifecycle(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	slug, err := req.RequireString("slug")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	tmpl := req.GetString("template", defaultTemplate)
	cleanup := req.GetBool("cleanup", true)
	tapURL := req.GetString("tap_action_url", "")

	var steps []string
	failed := false

	// Step 1: Create activity
	_, err = api.CreateActivity(ctx, client.CreateActivityInput{
		Slug: slug,
		Name: name,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Step 1 (create): %v", err)), nil
	}
	steps = append(steps, "1. Created activity: OK")

	// Step 2: Update to ongoing
	ongoingContent := buildTestContent(tmpl, 0.5, "Testing...", tapURL)
	if _, err = api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
		State:   stateOngoing,
		Content: ongoingContent,
	}); err != nil {
		steps = append(steps, fmt.Sprintf("2. Update %s: FAIL (%v)", stateOngoing, err))
		return mcp.NewToolResultError(strings.Join(steps, "\n")), nil
	}
	steps = append(steps, fmt.Sprintf("2. Updated to %s: OK", stateOngoing))

	// Step 3: Verify ongoing
	line, ok := verifyActivityState(ctx, api, slug, stateOngoing, 3)
	steps = append(steps, line)
	failed = failed || !ok

	// Step 4: Update to ended
	endedContent := buildTestContent(tmpl, 1.0, "Done", tapURL)
	if _, err = api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
		State:   stateEnded,
		Content: endedContent,
	}); err != nil {
		steps = append(steps, fmt.Sprintf("4. Update %s: FAIL (%v)", stateEnded, err))
		return mcp.NewToolResultError(strings.Join(steps, "\n")), nil
	}
	steps = append(steps, fmt.Sprintf("4. Updated to %s: OK", stateEnded))

	// Step 5: Verify ended
	line, ok = verifyActivityState(ctx, api, slug, stateEnded, 5)
	steps = append(steps, line)
	failed = failed || !ok

	// Step 6: Cleanup
	if cleanup {
		if err = api.DeleteActivity(ctx, slug); err != nil {
			steps = append(steps, fmt.Sprintf("6. Cleanup: FAIL (%v)", err))
			failed = true
		} else {
			steps = append(steps, "6. Cleanup (deleted): OK")
		}
	} else {
		steps = append(steps, "6. Cleanup: skipped (cleanup=false)")
	}

	out := strings.Join(steps, "\n")
	if failed {
		return mcp.NewToolResultError(out), nil
	}
	return mcp.NewToolResultText(out), nil
}

// verifyActivityState fetches the activity and confirms its state equals want,
// returning a numbered step line and whether the check passed. A false result
// (fetch error, parse error, or state mismatch) tells the caller to report the
// lifecycle as an MCP error rather than burying a failure in a success result.
func verifyActivityState(ctx context.Context, api *client.APIClient, slug, want string, step int) (string, bool) {
	raw, err := api.GetActivity(ctx, slug)
	if err != nil {
		return fmt.Sprintf("%d. Verify %s: FAIL (%v)", step, want, err), false
	}
	var activity map[string]any
	if err := json.Unmarshal(raw, &activity); err != nil {
		return fmt.Sprintf("%d. Verify %s: FAIL (parsing activity: %v)", step, want, err), false
	}
	got, _ := activity["state"].(string)
	if got == want {
		return fmt.Sprintf("%d. Verified state=%s: OK", step, want), true
	}
	return fmt.Sprintf("%d. Verify %s: expected %s, got %s", step, want, want, got), false
}

func buildTestContent(tmpl string, progress float64, state, tapURL string) json.RawMessage {
	content := map[string]any{
		"template":     tmpl,
		"progress":     progress,
		"state":        state,
		"icon":         "checkmark.circle",
		"accent_color": "blue",
	}

	switch tmpl {
	case tmplSteps:
		content["current_step"] = 1
		content["total_steps"] = 2
	case tmplAlert:
		content["severity"] = "info"
	case tmplGauge:
		content["value"] = progress * 100
		content["min_value"] = 0
		content["max_value"] = 100
		content["unit"] = "%"
	case tmplCountdown:
		// countdown requires an end_date; the server resolves a duration string
		// into start/end dates, so this satisfies validation on both PATCHes.
		content["duration"] = "5m"
	case tmplTimeline:
		// timeline value must be a labeled map ({key: number}), not a scalar —
		// the server rejects a bare number for this template.
		content["value"] = map[string]any{"Value": progress * 100}
	case tmplBoard:
		// board requires 1-4 tiles; tile values are strings (not numeric),
		// replaced wholesale on every update.
		content["tiles"] = []map[string]any{
			{"label": "Progress", "value": fmt.Sprintf("%d%%", int(progress*100)), "trend": "up"},
			{"label": "Status", "value": state, "icon": "checkmark.circle"},
		}
	case tmplLog:
		// log requires 1-20 lines, newest-first; each line needs text, replaced
		// wholesale on every update.
		content["lines"] = []map[string]any{
			{"text": state, "level": "info"},
			{"text": "Lifecycle test started", "level": "info"},
		}
	}

	if tapURL != "" {
		content["tap_action"] = map[string]any{"url": tapURL}
	}

	data, _ := json.Marshal(content)
	return data
}

func handleTestNotification(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body, err := req.RequireString("body")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	push := req.GetBool("push", false)

	raw, err := api.CreateNotification(ctx, client.CreateNotificationInput{
		Title:  title,
		Body:   body,
		Source: "pushward-mcp",
		Push:   &push,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleTestEmail(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	to, err := req.RequireString("to")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	subject := req.GetString("subject", "PushWard MCP test email")
	body := req.GetString("body", "This is a test email from the PushWard MCP server.")

	raw, err := api.SendEmail(ctx, client.SendEmailInput{
		To:       to,
		Subject:  subject,
		TextBody: body,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleTestRelayProvider(ctx context.Context, req mcp.CallToolRequest, relay *client.RelayClient) (*mcp.CallToolResult, error) {
	provider, err := req.RequireString("provider")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	payload, ok := testPayloads[provider]
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("no test payload defined for provider: %s", provider)), nil
	}

	raw, err := relay.PostWebhook(ctx, provider, payload)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("relay %s: %v", provider, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("relay %s: %s", provider, string(raw))), nil
}

// ---------- end_activity ----------

func handleEndActivity(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	slug, err := req.RequireString("slug")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	reason := req.GetString("reason", "Ended manually")
	contentOverride := req.GetString("content_json", "")

	// Fetch current activity
	raw, err := api.GetActivity(ctx, slug)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetching activity %q: %v", slug, err)), nil
	}

	var activity map[string]any
	if err := json.Unmarshal(raw, &activity); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parsing activity: %v", err)), nil
	}

	// Check if already ended
	if state, _ := activity["state"].(string); state == stateEnded {
		return mcp.NewToolResultText(fmt.Sprintf("Activity %q is already %s", slug, stateEnded)), nil
	}

	// Build content
	var content json.RawMessage
	if contentOverride != "" {
		if !json.Valid([]byte(contentOverride)) {
			return mcp.NewToolResultError("content_json is not valid JSON"), nil
		}
		content = json.RawMessage(contentOverride)
	} else {
		content = buildEndContent(activity, reason)
	}

	// End the activity
	_, err = api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
		State:   stateEnded,
		Content: content,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("ending activity %q: %v", slug, err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Ended activity %q: %s", slug, reason)), nil
}

// buildEndContent preserves the activity's existing content and sets the state
// text to the given reason. Returns a shallow-cloned content map to avoid
// mutating the source data.
func buildEndContent(activity map[string]any, reason string) json.RawMessage {
	orig, _ := activity["content"].(map[string]any)
	contentMap := make(map[string]any, len(orig)+1)
	for k, v := range orig {
		contentMap[k] = v
	}
	if _, ok := contentMap["template"]; !ok {
		contentMap["template"] = defaultTemplate
	}
	contentMap["state"] = reason
	data, _ := json.Marshal(contentMap)
	return data
}

// ---------- get_activity (enhanced) ----------

func handleGetActivity(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	slug, err := req.RequireString("slug")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var includes []string
	if req.GetBool("include_log_backlog", false) {
		includes = append(includes, "log_backlog")
	}

	raw, err := api.GetActivity(ctx, slug, includes...)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// ---------- list_activities (enhanced) ----------

func handleListActivities(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	raw, err := api.ListAllActivities(ctx)
	truncated := errors.Is(err, client.ErrListActivitiesTruncated)
	if err != nil && !truncated {
		return mcp.NewToolResultError(err.Error()), nil
	}

	stateFilter := req.GetString("state", "")
	sourceFilter := req.GetString("source", "")
	summary := req.GetBool("summary", false)
	limit := max(int(req.GetFloat("limit", 0)), 0)

	// No filters and no summary — return raw response (backwards compat)
	if stateFilter == "" && sourceFilter == "" && !summary && limit == 0 {
		return mcp.NewToolResultText(withTruncationNote(string(raw), truncated)), nil
	}

	var activities []map[string]any
	if err := json.Unmarshal(raw, &activities); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parsing activities: %v", err)), nil
	}

	filtered := filterActivities(activities, stateFilter, sourceFilter, "")
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}

	if summary {
		return mcp.NewToolResultText(withTruncationNote(formatSummary(filtered), truncated)), nil
	}

	out, _ := json.MarshalIndent(filtered, "", "  ")
	return mcp.NewToolResultText(withTruncationNote(string(out), truncated)), nil
}

func withTruncationNote(body string, truncated bool) string {
	if truncated {
		return body + truncationNote
	}
	return body
}

func matchesSource(slug, source string) bool {
	prefix := source + "-"
	prefixUnderscore := source + "_"
	return strings.HasPrefix(slug, prefix) || strings.HasPrefix(slug, prefixUnderscore) || slug == source
}

func filterActivities(activities []map[string]any, state, source, slugPrefix string) []map[string]any {
	var result []map[string]any
	for _, a := range activities {
		if state != "" {
			if s, _ := a["state"].(string); s != state {
				continue
			}
		}
		slug, _ := a["slug"].(string)
		if source != "" && !matchesSource(slug, source) {
			continue
		}
		if slugPrefix != "" && !strings.HasPrefix(slug, slugPrefix) {
			continue
		}
		result = append(result, a)
	}
	return result
}

func formatSummary(activities []map[string]any) string {
	if len(activities) == 0 {
		return "No activities found"
	}

	type summaryEntry struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		State    string `json:"state"`
		Priority int    `json:"priority"`
		Age      string `json:"age"`
		Template string `json:"template"`
	}

	entries := make([]summaryEntry, 0, len(activities))
	for _, a := range activities {
		slug, _ := a["slug"].(string)
		name, _ := a["name"].(string)
		state, _ := a["state"].(string)
		priority, _ := a["priority"].(float64)

		tmpl := defaultTemplate
		if content, ok := a["content"].(map[string]any); ok {
			if t, ok := content["template"].(string); ok {
				tmpl = t
			}
		}

		age := "unknown"
		if createdStr, ok := a["created_at"].(string); ok {
			if created, err := time.Parse(time.RFC3339, createdStr); err == nil {
				age = formatDuration(time.Since(created))
			}
		}

		entries = append(entries, summaryEntry{
			Slug:     slug,
			Name:     name,
			State:    state,
			Priority: int(priority),
			Age:      age,
			Template: tmpl,
		})
	}

	out, _ := json.MarshalIndent(entries, "", "  ")
	return string(out)
}

func formatDuration(d time.Duration) string {
	// A future created_at (clock skew between client and server) yields a
	// negative age; clamp it so it renders as "0m" rather than "-1h -3m".
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// ---------- bulk_end_activities ----------

func handleBulkEndActivities(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	stateFilter := req.GetString("state", "")
	sourceFilter := req.GetString("source", "")
	slugPrefix := req.GetString("slug_prefix", "")
	reason := req.GetString("reason", "Ended in bulk")
	confirm := req.GetBool("confirm", false)

	if stateFilter == "" && sourceFilter == "" && slugPrefix == "" {
		return mcp.NewToolResultError("at least one filter (state, source, or slug_prefix) is required"), nil
	}

	// Fetch all activities (walks pagination cursor internally)
	raw, err := api.ListAllActivities(ctx)
	truncated := errors.Is(err, client.ErrListActivitiesTruncated)
	if err != nil && !truncated {
		return mcp.NewToolResultError(fmt.Sprintf("listing activities: %v", err)), nil
	}

	var activities []map[string]any
	if err := json.Unmarshal(raw, &activities); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parsing activities: %v", err)), nil
	}

	// Filter to matching non-ENDED activities
	candidates := filterActivities(activities, stateFilter, sourceFilter, slugPrefix)

	// Only end activities that aren't already ENDED
	var toEnd []map[string]any
	for _, a := range candidates {
		if s, _ := a["state"].(string); s != stateEnded {
			toEnd = append(toEnd, a)
		}
	}

	if len(toEnd) == 0 {
		return mcp.NewToolResultText(withTruncationNote("No matching activities to end", truncated)), nil
	}

	// Dry-run gate: refuse to act unless caller explicitly confirms.
	if !confirm {
		const sampleLimit = 10
		sample := make([]string, 0, sampleLimit)
		for i, a := range toEnd {
			if i >= sampleLimit {
				break
			}
			slug, _ := a["slug"].(string)
			sample = append(sample, slug)
		}
		more := ""
		if len(toEnd) > sampleLimit {
			more = fmt.Sprintf(" (+%d more)", len(toEnd)-sampleLimit)
		}
		body := fmt.Sprintf(
			"Dry run: %d activities match and would be ended.\nSample: %s%s\nRe-call with confirm=true to actually end them.",
			len(toEnd), strings.Join(sample, ", "), more,
		)
		return mcp.NewToolResultText(withTruncationNote(body, truncated)), nil
	}

	var ended []string
	var failed []string
	for _, a := range toEnd {
		// Stop early if the caller cancelled — don't keep firing PATCHes.
		if err := ctx.Err(); err != nil {
			failed = append(failed, fmt.Sprintf("(aborted after %d: %v)", len(ended), err))
			break
		}
		slug, _ := a["slug"].(string)
		content := buildEndContent(a, reason)

		_, err := api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
			State:   stateEnded,
			Content: content,
		})
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", slug, err))
		} else {
			ended = append(ended, slug)
		}
	}

	var parts []string
	if len(ended) > 0 {
		parts = append(parts, fmt.Sprintf("Ended %d activities: %s", len(ended), strings.Join(ended, ", ")))
	}
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("Failed %d: %s", len(failed), strings.Join(failed, "; ")))
	}

	out := withTruncationNote(strings.Join(parts, "\n"), truncated)
	// Nothing ended but failures occurred (every PATCH failed or the caller
	// cancelled before the first success) — surface it as an MCP error so the
	// agent can detect total failure via IsError, mirroring the lifecycle tool,
	// instead of having to parse the text.
	if len(ended) == 0 && len(failed) > 0 {
		return mcp.NewToolResultError(out), nil
	}
	return mcp.NewToolResultText(out), nil
}

// testPayloads contains standard test payloads for each relay provider.
// Derived from relay's internal/selftest fixtures and justfile showcase recipes.
var testPayloads = map[string]any{
	"argocd": map[string]any{
		"app": "mcp-test-app", "event": "sync-running",
		"revision": "abc1234", "repo_url": "https://github.com/test/repo",
	},
	"grafana": map[string]any{
		"alerts": []map[string]any{{
			"status":      "firing",
			"labels":      map[string]string{"alertname": "MCP Test Alert", "severity": "warning"},
			"annotations": map[string]string{"summary": "Test alert from PushWard MCP"},
			"fingerprint": "mcp-test-001",
			"startsAt":    "2024-01-01T00:00:00Z",
		}},
	},
	"radarr": map[string]any{
		"eventType": "Grab",
		"movie":     map[string]any{"title": "MCP Test Movie", "year": 2024},
		"release":   map[string]any{"quality": "Bluray-1080p", "releaseTitle": "MCP.Test.2024.1080p"},
	},
	"sonarr": map[string]any{
		"eventType": "Grab",
		"series":    map[string]any{"title": "MCP Test Show"},
		"episodes":  []map[string]any{{"seasonNumber": 1, "episodeNumber": 1, "title": "Pilot"}},
		"release":   map[string]any{"quality": "HDTV-720p"},
	},
	"prowlarr": map[string]any{
		"eventType":      "Health",
		"level":          "warning",
		"message":        "MCP test health check",
		"type":           "IndexerStatusCheck",
		"wikiUrl":        "https://wiki.servarr.com",
		"applicationUrl": "",
	},
	"bazarr": map[string]any{
		"version": "1", "title": "MCP Test", "message": "Test subtitle download", "type": "info",
	},
	"unmanic": map[string]any{
		"version": "1", "title": "MCP Test", "message": "Test transcoding complete", "type": "info",
	},
	"jellyfin": map[string]any{
		"NotificationType":     "PlaybackStart",
		"Name":                 "MCP Test Movie",
		"DeviceName":           "MCP Test Device",
		"NotificationUsername": "testuser",
	},
	"paperless": map[string]any{
		"event": "document_consumption_started", "title": "MCP Test Document",
		"correspondent": "Test Corp", "document_type": "Invoice",
	},
	"changedetection": map[string]any{
		"url": "https://example.com", "title": "MCP Test Watch",
		"tag": "test", "triggered_text": "Page content changed",
	},
	"proxmox": map[string]any{
		"type": "vzdump", "title": "MCP Test Backup",
		"message": "Backup started for VM 100", "severity": "info", "hostname": "pve-test",
	},
	"overseerr": map[string]any{
		"notification_type": "MEDIA_PENDING", "event": "media.pending",
		"subject": "MCP Test Movie Requested", "message": "A new movie was requested",
		"media":   map[string]any{"media_type": "movie", "tmdbId": "12345", "status": "PENDING"},
		"request": map[string]any{"request_id": "1", "requestedBy_username": "testuser"},
	},
	"uptimekuma": map[string]any{
		"monitor":   map[string]any{"id": 1, "name": "MCP Test Monitor", "url": "https://example.com", "type": "http"},
		"heartbeat": map[string]any{"status": 0, "msg": "Connection failed", "time": "2024-01-01 00:00:00"},
		"msg":       "MCP Test Monitor is DOWN",
	},
	"gatus": map[string]any{
		"endpoint_name": "mcp-test", "endpoint_group": "test",
		"endpoint_url": "https://example.com", "alert_description": "MCP test endpoint down",
		"status": "TRIGGERED", "result_errors": "connection refused",
	},
	"backrest": map[string]any{
		"event": "snapshot_start", "plan": "mcp-test-backup",
		"repo": "local", "snapshot_id": "abc123",
	},
}
