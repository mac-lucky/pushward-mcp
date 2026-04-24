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
	"time"

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
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/activities/"):
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
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"about:blank","title":"Bad Request","status":400,"detail":"bad request","code":"validation.failed"}`))
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
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"type":"about:blank","title":"Internal Server Error","status":500,"detail":"update failed","code":"server.error"}`))
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

// ---------- handleEndActivity ----------

func TestHandleEndActivity_HappyPath(t *testing.T) {
	var patchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/activities/"):
			w.Write([]byte(`{"slug":"test-alert","state":"ONGOING","content":{"template":"alert","severity":"warning","state":"Firing"}}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/activities/"):
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &patchBody)
			w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-alert"})

	result, err := handleEndActivity(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected success, got error: %s", text)
	}
	if !strings.Contains(text, "Ended activity") {
		t.Errorf("expected 'Ended activity' in output, got: %s", text)
	}

	// Verify PATCH body preserved template and updated state
	if patchBody["state"] != "ENDED" {
		t.Errorf("expected state=ENDED in PATCH, got %v", patchBody["state"])
	}
	content, _ := patchBody["content"].(map[string]any)
	if content == nil {
		t.Fatal("missing content in PATCH body")
	}
	if content["template"] != "alert" {
		t.Errorf("expected template preserved as 'alert', got %v", content["template"])
	}
	if content["state"] != "Ended manually" {
		t.Errorf("expected state='Ended manually', got %v", content["state"])
	}
	if content["severity"] != "warning" {
		t.Errorf("expected severity preserved as 'warning', got %v", content["severity"])
	}
}

func TestHandleEndActivity_AlreadyEnded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"slug":"test-done","state":"ENDED","content":{"template":"generic","state":"Done"}}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-done"})

	result, err := handleEndActivity(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "already ENDED") {
		t.Errorf("expected 'already ENDED', got: %s", text)
	}
}

func TestHandleEndActivity_CustomReason(t *testing.T) {
	var patchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"slug":"test-x","state":"ONGOING","content":{"template":"generic","state":"Running"}}`))
		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &patchBody)
			w.Write(body)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"slug": "test-x", "reason": "Cancelled by operator"})

	result, err := handleEndActivity(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Cancelled by operator") {
		t.Errorf("expected custom reason in output, got: %s", text)
	}
	content, _ := patchBody["content"].(map[string]any)
	if content["state"] != "Cancelled by operator" {
		t.Errorf("expected state='Cancelled by operator', got %v", content["state"])
	}
}

func TestHandleEndActivity_ContentOverride(t *testing.T) {
	var patchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"slug":"test-x","state":"ONGOING","content":{"template":"alert","state":"Firing"}}`))
		case r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &patchBody)
			w.Write(body)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	override := `{"template":"generic","state":"Custom end"}`
	req := newReq(map[string]any{"slug": "test-x", "content_json": override})

	result, err := handleEndActivity(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", resultText(t, result))
	}
	content, _ := patchBody["content"].(map[string]any)
	if content["template"] != "generic" {
		t.Errorf("expected overridden template 'generic', got %v", content["template"])
	}
}

// ---------- handleListActivities (enhanced) ----------

func TestHandleListActivities_NoFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"a","state":"ONGOING"},{"slug":"b","state":"ENDED"}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	// Should return raw JSON (backwards compat)
	if !strings.Contains(text, `"slug":"a"`) || !strings.Contains(text, `"slug":"b"`) {
		t.Errorf("expected both activities in raw output, got: %s", text)
	}
}

func TestHandleListActivities_StateFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"a","state":"ONGOING"},{"slug":"b","state":"ENDED"},{"slug":"c","state":"ONGOING"}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"state": "ONGOING"})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	var activities []map[string]any
	if err := json.Unmarshal([]byte(text), &activities); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(activities) != 2 {
		t.Errorf("expected 2 ONGOING activities, got %d", len(activities))
	}
	for _, a := range activities {
		if a["state"] != "ONGOING" {
			t.Errorf("expected state=ONGOING, got %v", a["state"])
		}
	}
}

func TestHandleListActivities_SourceFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"grafana-cpu","state":"ONGOING"},{"slug":"grafana_disk","state":"ONGOING"},{"slug":"argocd-deploy","state":"ONGOING"}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"source": "grafana"})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	var activities []map[string]any
	if err := json.Unmarshal([]byte(text), &activities); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(activities) != 2 {
		t.Errorf("expected 2 grafana activities, got %d", len(activities))
	}
}

func TestHandleListActivities_Summary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"test-a","name":"Test A","state":"ONGOING","priority":5,"content":{"template":"alert"},"created_at":"2026-04-10T10:00:00Z"}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"summary": true})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	var summaries []map[string]any
	if err := json.Unmarshal([]byte(text), &summaries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	s := summaries[0]
	if s["slug"] != "test-a" {
		t.Errorf("expected slug=test-a, got %v", s["slug"])
	}
	if s["template"] != "alert" {
		t.Errorf("expected template=alert, got %v", s["template"])
	}
	if _, ok := s["age"]; !ok {
		t.Error("missing age field in summary")
	}
	// Summary should NOT contain content (it's projected out)
	if _, ok := s["content"]; ok {
		t.Error("summary should not contain content field")
	}
}

func TestHandleListActivities_Limit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"a","state":"ONGOING"},{"slug":"b","state":"ONGOING"},{"slug":"c","state":"ONGOING"}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"state": "ONGOING", "limit": float64(2)})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	var activities []map[string]any
	if err := json.Unmarshal([]byte(text), &activities); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(activities) != 2 {
		t.Errorf("expected 2 activities (limited), got %d", len(activities))
	}
}

func TestHandleListActivities_CombinedFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[
			{"slug":"grafana-cpu","state":"ONGOING"},
			{"slug":"grafana-disk","state":"ENDED"},
			{"slug":"argocd-deploy","state":"ONGOING"},
			{"slug":"grafana-mem","state":"ONGOING"}
		],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"state": "ONGOING", "source": "grafana", "limit": float64(1)})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	var activities []map[string]any
	if err := json.Unmarshal([]byte(text), &activities); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(activities) != 1 {
		t.Errorf("expected 1 activity (ONGOING + grafana + limit 1), got %d", len(activities))
	}
}

func TestHandleListActivities_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"a","state":"ENDED"}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"state": "ONGOING", "summary": true})

	result, err := handleListActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "No activities found") {
		t.Errorf("expected 'No activities found', got: %s", text)
	}
}

// ---------- handleBulkEndActivities ----------

func TestHandleBulkEndActivities_NoFilter(t *testing.T) {
	api := client.NewAPIClient("http://unused", "test-token")
	req := newReq(map[string]any{})

	result, err := handleBulkEndActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no filter provided")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "at least one filter") {
		t.Errorf("expected filter error, got: %s", text)
	}
}

func TestHandleBulkEndActivities_HappyPath(t *testing.T) {
	var patchCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/activities":
			w.Write([]byte(`{"items":[
				{"slug":"grafana-cpu","state":"ONGOING","content":{"template":"alert","state":"Firing"}},
				{"slug":"grafana-disk","state":"ONGOING","content":{"template":"alert","state":"Firing"}},
				{"slug":"argocd-deploy","state":"ONGOING","content":{"template":"steps"}}
			],"has_more":false}`))
		case r.Method == http.MethodPatch:
			patchCount++
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"source": "grafana", "confirm": true})

	result, err := handleBulkEndActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if patchCount != 2 {
		t.Errorf("expected 2 PATCH calls (grafana only), got %d", patchCount)
	}
	if !strings.Contains(text, "Ended 2 activities") {
		t.Errorf("expected 'Ended 2 activities', got: %s", text)
	}
	if !strings.Contains(text, "grafana-cpu") || !strings.Contains(text, "grafana-disk") {
		t.Errorf("expected both grafana slugs in output, got: %s", text)
	}
}

func TestHandleBulkEndActivities_DryRunByDefault(t *testing.T) {
	var patchCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/activities":
			w.Write([]byte(`{"items":[
				{"slug":"grafana-cpu","state":"ONGOING","content":{"template":"alert"}},
				{"slug":"grafana-disk","state":"ONGOING","content":{"template":"alert"}}
			],"has_more":false}`))
		case r.Method == http.MethodPatch:
			patchCount++
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"source": "grafana"}) // confirm omitted → dry-run

	result, err := handleBulkEndActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if patchCount != 0 {
		t.Errorf("dry-run must not PATCH, got %d calls", patchCount)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Dry run: 2 activities match") {
		t.Errorf("expected dry-run preview, got: %s", text)
	}
	if !strings.Contains(text, "confirm=true") {
		t.Errorf("expected hint about confirm=true, got: %s", text)
	}
}

func TestHandleBulkEndActivities_NoMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"slug":"argocd-deploy","state":"ONGOING","content":{"template":"steps"}}],"has_more":false}`))
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"source": "grafana"})

	result, err := handleBulkEndActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "No matching activities") {
		t.Errorf("expected 'No matching activities', got: %s", text)
	}
}

func TestHandleBulkEndActivities_SkipsAlreadyEnded(t *testing.T) {
	var patchCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/activities":
			w.Write([]byte(`{"items":[
				{"slug":"grafana-cpu","state":"ONGOING","content":{"template":"alert"}},
				{"slug":"grafana-disk","state":"ENDED","content":{"template":"alert"}}
			],"has_more":false}`))
		case r.Method == http.MethodPatch:
			patchCount++
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "test-token")
	req := newReq(map[string]any{"source": "grafana", "confirm": true})

	result, err := handleBulkEndActivities(context.Background(), req, api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if patchCount != 1 {
		t.Errorf("expected 1 PATCH (skip ENDED), got %d", patchCount)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Ended 1 activities") {
		t.Errorf("expected 'Ended 1 activities', got: %s", text)
	}
}

// ---------- matchesSource ----------

func TestMatchesSource(t *testing.T) {
	tests := []struct {
		slug, source string
		want         bool
	}{
		{"grafana-cpu", "grafana", true},
		{"grafana_disk", "grafana", true},
		{"grafana", "grafana", true},
		{"argocd-deploy", "grafana", false},
		{"my-grafana-thing", "grafana", false},
		{"grafana-", "grafana", true},
	}
	for _, tt := range tests {
		if got := matchesSource(tt.slug, tt.source); got != tt.want {
			t.Errorf("matchesSource(%q, %q) = %v, want %v", tt.slug, tt.source, got, tt.want)
		}
	}
}

// ---------- formatDuration ----------

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{25 * time.Hour, "1d 1h"},
		{48*time.Hour + 30*time.Minute, "2d 0h"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.d); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// ---------- testPayloads ----------

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
