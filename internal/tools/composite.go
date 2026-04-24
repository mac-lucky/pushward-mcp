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
	stateOngoing   = "ONGOING"
	stateEnded     = "ENDED"
	defaultTemplate = "generic"
)

func registerCompositeTools(s *mcpserver.MCPServer, api *client.APIClient, relay *client.RelayClient) {
	// test_health
	s.AddTool(
		mcp.NewTool("test_health",
			mcp.WithDescription("Check health of both PushWard API and Relay endpoints"),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleTestHealth(ctx, api, relay)
		},
	)

	// test_activity_lifecycle
	s.AddTool(
		mcp.NewTool("test_activity_lifecycle",
			mcp.WithDescription("Run a full activity lifecycle: create -> start (ONGOING) -> end (ENDED) -> verify -> optionally delete"),
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
				mcp.Enum("generic", "countdown", "steps", "alert", "gauge", "timeline"),
			),
			mcp.WithBoolean("cleanup",
				mcp.Description("Delete the activity after test (default: true)"),
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

	// test_relay_provider
	s.AddTool(
		mcp.NewTool("test_relay_provider",
			mcp.WithDescription("Send a standard test payload to a relay provider and verify the response"),
			mcp.WithString("provider",
				mcp.Required(),
				mcp.Description("Relay provider name"),
				mcp.Enum(
					"argocd", "backrest", "bazarr", "changedetection", "gatus",
					"grafana", "jellyfin", "overseerr", "paperless", "prowlarr",
					"proxmox", "radarr", "sonarr", "unmanic", "uptimekuma",
				),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleTestRelayProvider(ctx, req, relay)
		},
	)

	// end_activity
	s.AddTool(
		mcp.NewTool("end_activity",
			mcp.WithDescription("End an activity by slug. Fetches the current content to preserve the template, then transitions to ENDED state."),
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

	// list_activities (enhanced, replaces generated version)
	s.AddTool(
		mcp.NewTool("list_activities",
			mcp.WithDescription("List activities with optional filtering and summary mode. Walks the server's cursor pagination automatically and returns the aggregated result (capped at ~2000 activities). Without parameters, returns the full JSON for every page concatenated."),
			mcp.WithString("state",
				mcp.Description("Filter by activity state"),
				mcp.Enum("ONGOING", "ENDED", "PREEMPTED"),
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
			mcp.WithString("state",
				mcp.Description("Filter by current state (e.g. \"ONGOING\")"),
				mcp.Enum("ONGOING", "ENDED", "PREEMPTED"),
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

	relayRaw, _, err := relay.Base.DoJSON(ctx, "GET", "/health", nil)
	if err != nil {
		results = append(results, fmt.Sprintf("Relay: FAIL (%v)", err))
	} else {
		results = append(results, fmt.Sprintf("Relay: %s", string(relayRaw)))
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

	var steps []string

	// Step 1: Create activity
	_, err = api.CreateActivity(ctx, client.CreateActivityInput{
		Slug: slug,
		Name: name,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Step 1 (create): %v", err)), nil
	}
	steps = append(steps, "1. Created activity: OK")

	// Step 2: Update to ONGOING
	ongoingContent := buildTestContent(tmpl, 0.5, "Testing...")
	_, err = api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
		State:   stateOngoing,
		Content: ongoingContent,
	})
	if err != nil {
		steps = append(steps, fmt.Sprintf("2. Update ONGOING: FAIL (%v)", err))
		return mcp.NewToolResultText(strings.Join(steps, "\n")), nil
	}
	steps = append(steps, "2. Updated to ONGOING: OK")

	// Step 3: Verify ONGOING
	raw, err := api.GetActivity(ctx, slug)
	if err != nil {
		steps = append(steps, fmt.Sprintf("3. Verify ONGOING: FAIL (%v)", err))
	} else {
		var activity map[string]any
		_ = json.Unmarshal(raw, &activity)
		state, _ := activity["state"].(string)
		if state == stateOngoing {
			steps = append(steps, "3. Verified state=ONGOING: OK")
		} else {
			steps = append(steps, fmt.Sprintf("3. Verify ONGOING: expected ONGOING, got %s", state))
		}
	}

	// Step 4: Update to ENDED
	endedContent := buildTestContent(tmpl, 1.0, "Done")
	_, err = api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
		State:   stateEnded,
		Content: endedContent,
	})
	if err != nil {
		steps = append(steps, fmt.Sprintf("4. Update ENDED: FAIL (%v)", err))
		return mcp.NewToolResultText(strings.Join(steps, "\n")), nil
	}
	steps = append(steps, "4. Updated to ENDED: OK")

	// Step 5: Verify ENDED
	raw, err = api.GetActivity(ctx, slug)
	if err != nil {
		steps = append(steps, fmt.Sprintf("5. Verify ENDED: FAIL (%v)", err))
	} else {
		var activity map[string]any
		_ = json.Unmarshal(raw, &activity)
		state, _ := activity["state"].(string)
		if state == stateEnded {
			steps = append(steps, "5. Verified state=ENDED: OK")
		} else {
			steps = append(steps, fmt.Sprintf("5. Verify ENDED: expected ENDED, got %s", state))
		}
	}

	// Step 6: Cleanup
	if cleanup {
		err = api.DeleteActivity(ctx, slug)
		if err != nil {
			steps = append(steps, fmt.Sprintf("6. Cleanup: FAIL (%v)", err))
		} else {
			steps = append(steps, "6. Cleanup (deleted): OK")
		}
	} else {
		steps = append(steps, "6. Cleanup: skipped (cleanup=false)")
	}

	return mcp.NewToolResultText(strings.Join(steps, "\n")), nil
}

func buildTestContent(tmpl string, progress float64, state string) json.RawMessage {
	content := map[string]any{
		"template":    tmpl,
		"progress":    progress,
		"state":       state,
		"icon":        "checkmark.circle",
		"accent_color": "blue",
	}

	switch tmpl {
	case "steps":
		content["current_step"] = 1
		content["total_steps"] = 2
	case "alert":
		content["severity"] = "info"
	case "gauge":
		content["value"] = progress * 100
		content["min_value"] = 0
		content["max_value"] = 100
		content["unit"] = "%"
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
		Push:   push,
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
		return mcp.NewToolResultText(fmt.Sprintf("Activity %q is already ENDED", slug)), nil
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

	return mcp.NewToolResultText(withTruncationNote(strings.Join(parts, "\n"), truncated)), nil
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
		"eventType":       "Health",
		"level":           "warning",
		"message":         "MCP test health check",
		"type":            "IndexerStatusCheck",
		"wikiUrl":         "https://wiki.servarr.com",
		"applicationUrl":  "",
	},
	"bazarr": map[string]any{
		"version": "1", "title": "MCP Test", "message": "Test subtitle download", "type": "info",
	},
	"unmanic": map[string]any{
		"version": "1", "title": "MCP Test", "message": "Test transcoding complete", "type": "info",
	},
	"jellyfin": map[string]any{
		"NotificationType": "PlaybackStart",
		"Name":             "MCP Test Movie",
		"DeviceName":       "MCP Test Device",
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
		"media": map[string]any{"media_type": "movie", "tmdbId": "12345", "status": "PENDING"},
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
