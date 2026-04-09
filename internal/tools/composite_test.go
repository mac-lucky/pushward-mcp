package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// resultText extracts the text from a CallToolResult, joining all TextContent items.
func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func newReq(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// ---------- buildTestContent ----------

func TestBuildTestContent_Generic(t *testing.T) {
	raw := buildTestContent("generic", 0.5, "Testing...")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"template", "progress", "state", "icon", "accent_color"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	if m["template"] != "generic" {
		t.Errorf("template = %v, want generic", m["template"])
	}
	if m["state"] != "Testing..." {
		t.Errorf("state = %v, want Testing...", m["state"])
	}
}

func TestBuildTestContent_Steps(t *testing.T) {
	raw := buildTestContent("steps", 0.3, "Step 1")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"template", "progress", "state", "icon", "accent_color", "current_step", "total_steps"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	if m["current_step"] != float64(1) {
		t.Errorf("current_step = %v, want 1", m["current_step"])
	}
	if m["total_steps"] != float64(2) {
		t.Errorf("total_steps = %v, want 2", m["total_steps"])
	}
}

func TestBuildTestContent_Alert(t *testing.T) {
	raw := buildTestContent("alert", 0.0, "Firing")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := m["severity"]; !ok {
		t.Error("missing key \"severity\"")
	}
	if m["severity"] != "info" {
		t.Errorf("severity = %v, want info", m["severity"])
	}
}

func TestBuildTestContent_Gauge(t *testing.T) {
	raw := buildTestContent("gauge", 0.75, "Running")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"value", "min_value", "max_value", "unit"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	if m["value"] != float64(75) {
		t.Errorf("value = %v, want 75", m["value"])
	}
	if m["unit"] != "%" {
		t.Errorf("unit = %v, want %%", m["unit"])
	}
}

// ---------- handleTestHealth ----------

func TestHandleTestHealth_BothHealthy(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer apiSrv.Close()

	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer relaySrv.Close()

	api := client.NewAPIClient(apiSrv.URL, "test-token")
	relay := client.NewRelayClient(relaySrv.URL, "test-token")

	result, err := handleTestHealth(context.Background(), api, relay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "API:") {
		t.Error("output missing API: prefix")
	}
	if !strings.Contains(text, "Relay:") {
		t.Error("output missing Relay: prefix")
	}
	if strings.Contains(text, "FAIL") {
		t.Errorf("unexpected FAIL in output: %s", text)
	}
}

func TestHandleTestHealth_APIDown(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer apiSrv.Close()

	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer relaySrv.Close()

	api := client.NewAPIClient(apiSrv.URL, "test-token")
	relay := client.NewRelayClient(relaySrv.URL, "test-token")

	result, err := handleTestHealth(context.Background(), api, relay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "API: FAIL") {
		t.Errorf("expected API: FAIL, got: %s", text)
	}
	if strings.Contains(text, "Relay: FAIL") {
		t.Errorf("relay should be healthy, got: %s", text)
	}
}

func TestHandleTestHealth_RelayDown(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer apiSrv.Close()

	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("relay down"))
	}))
	defer relaySrv.Close()

	api := client.NewAPIClient(apiSrv.URL, "test-token")
	relay := client.NewRelayClient(relaySrv.URL, "test-token")

	result, err := handleTestHealth(context.Background(), api, relay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if strings.Contains(text, "API: FAIL") {
		t.Errorf("API should be healthy, got: %s", text)
	}
	if !strings.Contains(text, "Relay: FAIL") {
		t.Errorf("expected Relay: FAIL, got: %s", text)
	}
}

// ---------- handleTestActivityLifecycle ----------

func TestHandleTestActivityLifecycle_HappyPath(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/activities":
			// Step 1: create
			w.Write([]byte(`{"slug":"test-slug","name":"Test"}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/activity/"):
			// Steps 2, 4: update
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/activities/"):
			// Steps 3, 5: get - return state based on request order
			if n <= 4 {
				w.Write([]byte(`{"slug":"test-slug","state":"ONGOING"}`))
			} else {
				w.Write([]byte(`{"slug":"test-slug","state":"ENDED"}`))
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/activities/"):
			// Step 6: delete
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-slug", "name": "Test"})

	result, err := handleTestActivityLifecycle(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text)
	}

	okCount := strings.Count(text, "OK")
	if okCount != 6 {
		t.Errorf("expected 6 OK lines, got %d in:\n%s", okCount, text)
	}
}

func TestHandleTestActivityLifecycle_CreateFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-slug", "name": "Test"})

	result, err := handleTestActivityLifecycle(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !result.IsError {
		t.Errorf("expected IsError=true for create failure, got: %s", text)
	}
	if !strings.Contains(text, "Step 1") {
		t.Errorf("expected mention of Step 1 in error, got: %s", text)
	}
}

func TestHandleTestActivityLifecycle_UpdateOngoingFails(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/activities":
			w.Write([]byte(`{"slug":"test-slug","name":"Test"}`))
		case r.Method == http.MethodPatch:
			// First PATCH (step 2 ONGOING) fails
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"update failed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-slug", "name": "Test"})

	result, err := handleTestActivityLifecycle(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "ONGOING: FAIL") {
		t.Errorf("expected step 2 FAIL, got: %s", text)
	}
	// When ONGOING update fails, the function returns early (only steps 1 and 2)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines (early return after ONGOING fail), got %d:\n%s", len(lines), text)
	}
}

func TestHandleTestActivityLifecycle_CleanupSkipped(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/activities":
			w.Write([]byte(`{"slug":"test-slug","name":"Test"}`))
		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/activities/"):
			if n <= 4 {
				w.Write([]byte(`{"slug":"test-slug","state":"ONGOING"}`))
			} else {
				w.Write([]byte(`{"slug":"test-slug","state":"ENDED"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-slug", "name": "Test", "cleanup": false})

	result, err := handleTestActivityLifecycle(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "skipped") {
		t.Errorf("expected cleanup skipped, got: %s", text)
	}
}

// ---------- handleTestNotification ----------

func TestHandleTestNotification_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/notifications" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"id":1,"pushed":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"title": "Test Title", "body": "Test Body"})

	result, err := handleTestNotification(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, `"id":1`) {
		t.Errorf("expected id:1 in response, got: %s", text)
	}
	if !strings.Contains(text, `"pushed":false`) {
		t.Errorf("expected pushed:false in response, got: %s", text)
	}
}

func TestHandleTestNotification_PushFalseInBody(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.Write([]byte(`{"id":2,"pushed":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"title": "Test", "body": "Body", "push": false})

	_, err := handleTestNotification(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedBody == nil {
		t.Fatal("server did not receive a body")
	}
	push, ok := receivedBody["push"]
	if !ok {
		t.Fatal("request body missing 'push' field")
	}
	if push != false {
		t.Errorf("expected push=false in request body, got %v", push)
	}
}

// ---------- handleTestRelayProvider ----------

func TestHandleTestRelayProvider_ValidProvider(t *testing.T) {
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	relay := client.NewRelayClient(srv.URL, "test-token")
	req := newReq(map[string]any{"provider": "argocd"})

	result, err := handleTestRelayProvider(context.Background(), req, relay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedPath != "/argocd" {
		t.Errorf("expected path /argocd, got %s", receivedPath)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "relay argocd:") {
		t.Errorf("expected 'relay argocd:' prefix, got: %s", text)
	}
	if result.IsError {
		t.Errorf("expected success, got error: %s", text)
	}
}

// ---------- testPayloads completeness ----------

func TestTestPayloads_AllProvidersPresent(t *testing.T) {
	expected := []string{
		"argocd", "backrest", "bazarr", "changedetection", "gatus",
		"grafana", "jellyfin", "overseerr", "paperless", "prowlarr",
		"proxmox", "radarr", "sonarr", "unmanic", "uptimekuma",
	}

	if len(testPayloads) != len(expected) {
		t.Errorf("testPayloads has %d entries, want %d", len(testPayloads), len(expected))
	}

	for _, provider := range expected {
		payload, ok := testPayloads[provider]
		if !ok {
			t.Errorf("testPayloads missing provider %q", provider)
			continue
		}
		if payload == nil {
			t.Errorf("testPayloads[%q] is nil", provider)
		}
	}
}

func TestTestPayloads_AllMarshalable(t *testing.T) {
	for provider, payload := range testPayloads {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("testPayloads[%q] failed to marshal: %v", provider, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("testPayloads[%q] marshaled to empty", provider)
		}
	}
}
