package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
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

	tmpl := req.GetString("template", "generic")
	cleanup := true // default
	if v := req.GetBool("cleanup", true); !v {
		cleanup = false
	}

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
		State:   "ONGOING",
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
		if state == "ONGOING" {
			steps = append(steps, "3. Verified state=ONGOING: OK")
		} else {
			steps = append(steps, fmt.Sprintf("3. Verify ONGOING: expected ONGOING, got %s", state))
		}
	}

	// Step 4: Update to ENDED
	endedContent := buildTestContent(tmpl, 1.0, "Done")
	_, err = api.UpdateActivity(ctx, slug, client.UpdateActivityInput{
		State:   "ENDED",
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
		if state == "ENDED" {
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
