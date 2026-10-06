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
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

func TestReceiptTools_Requests(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		if strings.HasSuffix(r.URL.Path, "/receipts/cancel") {
			io.WriteString(w, `{"canceled":2}`)
			return
		}
		io.WriteString(w, `{"notification_id":42,"status":"canceled","cancel_reason":"api"}`)
	}))
	defer srv.Close()
	api := client.NewAPIClient(srv.URL, "tok")
	ctx := context.Background()

	for _, call := range []struct {
		handler func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error)
		args    map[string]any
		want    string
	}{
		{handleGetNotificationReceipt, map[string]any{"notification_id": float64(42)}, `"status":"canceled"`},
		{handleGetNotificationReceipt, map[string]any{"notification_id": float64(42), "wait_seconds": float64(25)}, `"status":"canceled"`},
		{handleCancelNotificationReceipt, map[string]any{"notification_id": float64(42)}, `"cancel_reason":"api"`},
		{handleCancelNotificationReceiptsByTag, map[string]any{"tag": "nas-1"}, `{"canceled":2}`},
	} {
		result, err := call.handler(ctx, newReq(call.args), api)
		if err != nil || result.IsError || !strings.Contains(resultText(t, result), call.want) {
			t.Errorf("args %v: %v %s", call.args, err, resultText(t, result))
		}
	}
	want := []string{
		"GET /notifications/receipts/42 ",
		// The hold is capped below the server's 25 s, as for answers.
		"GET /notifications/receipts/42?wait=20 ",
		"POST /notifications/receipts/42/cancel ",
		`POST /notifications/receipts/cancel {"tag":"nas-1"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestReceiptTools_BadArguments(t *testing.T) {
	api := client.NewAPIClient("http://127.0.0.1:1", "tok")
	for name, call := range map[string]struct {
		handler func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error)
		args    map[string]any
	}{
		"get without id":    {handleGetNotificationReceipt, map[string]any{}},
		"get fractional id": {handleGetNotificationReceipt, map[string]any{"notification_id": 1.5}},
		"cancel id string":  {handleCancelNotificationReceipt, map[string]any{"notification_id": "42"}},
		"tag missing":       {handleCancelNotificationReceiptsByTag, map[string]any{}},
		"tag empty":         {handleCancelNotificationReceiptsByTag, map[string]any{"tag": ""}},
		"wait without id":   {handleWaitForAck, map[string]any{}},
	} {
		result, err := call.handler(context.Background(), newReq(call.args), api)
		if err != nil || !result.IsError {
			t.Errorf("%s: want a tool error, got %v / %s", name, err, resultText(t, result))
		}
	}
}

// The cancels stop an alert, so they must not be annotated as safe.
func TestReceiptTools_Annotations(t *testing.T) {
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)
	for name, destructive := range map[string]bool{
		"get_notification_receipt":            false,
		"wait_for_ack":                        false,
		"cancel_notification_receipt":         true,
		"cancel_notification_receipts_by_tag": true,
	} {
		tool := s.GetTool(name)
		if tool == nil {
			t.Errorf("%s is not registered on the hosted server", name)
			continue
		}
		a := tool.Tool.Annotations
		if destructive && (a.DestructiveHint == nil || !*a.DestructiveHint) {
			t.Errorf("%s: destructive hint %v", name, a.DestructiveHint)
		}
		if !destructive && (a.ReadOnlyHint == nil || !*a.ReadOnlyHint) {
			t.Errorf("%s: read-only hint %v", name, a.ReadOnlyHint)
		}
	}
}

func waitForAckOutcome(t *testing.T, api *client.APIClient, args map[string]any) ackOutcome {
	t.Helper()
	result, err := handleWaitForAck(context.Background(), newReq(args), api)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("expected a regular result, got tool error: %s", text)
	}
	var out ackOutcome
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON (%v): %s", err, text)
	}
	return out
}

func TestHandleWaitForAck_Acknowledged(t *testing.T) {
	shortAnswerPoll(t)
	var calls atomic.Int64
	var sawWait atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notifications/receipts/42" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("wait") != "" {
			sawWait.Store(true)
		}
		if calls.Add(1) < 3 {
			io.WriteString(w, `{"notification_id":42,"status":"active","repeats_sent":1}`)
			return
		}
		io.WriteString(w, `{"notification_id":42,"status":"acknowledged","action_id":"pw_ack","acknowledged_by_device":"iPhone"}`)
	}))
	defer srv.Close()

	out := waitForAckOutcome(t, client.NewAPIClient(srv.URL, "tok"), map[string]any{"notification_id": float64(42)})
	if !out.Acknowledged || out.Status != "acknowledged" || out.Reason != "" || !strings.Contains(string(out.Receipt), `"pw_ack"`) {
		t.Errorf("unexpected outcome: %+v", out)
	}
	if !sawWait.Load() {
		t.Error("polls did not ask the server to hold (?wait=)")
	}
}

// An expiry or a cancel ends the wait at once: waiting on cannot change it.
func TestHandleWaitForAck_FinishedUnacknowledged(t *testing.T) {
	for status, reason := range map[string]string{
		`"status":"expired"`:                         "expired with nobody",
		`"status":"canceled","cancel_reason":"tag"`:  "canceled (tag)",
		`"status":"canceled"`:                        "it was canceled",
		`"status":"superseded_by_something_unknown"`: "finished as superseded_by_something_unknown",
	} {
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			io.WriteString(w, `{"notification_id":7,`+status+`}`)
		}))
		out := waitForAckOutcome(t, client.NewAPIClient(srv.URL, "tok"), map[string]any{"notification_id": float64(7)})
		srv.Close()
		if out.Acknowledged || !strings.Contains(out.Reason, reason) || len(out.Receipt) == 0 {
			t.Errorf("%s: unexpected outcome %+v", status, out)
		}
		if calls.Load() != 1 {
			t.Errorf("%s: %d polls, want 1", status, calls.Load())
		}
	}
}

func TestHandleWaitForAck_TimeoutIsResult(t *testing.T) {
	shortAnswerPoll(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"notification_id":7,"status":"active","repeats_sent":3}`)
	}))
	defer srv.Close()

	out := waitForAckOutcome(t, client.NewAPIClient(srv.URL, "tok"), map[string]any{"notification_id": float64(7), "timeout_seconds": float64(1)})
	if out.Acknowledged || out.Status != "active" || !strings.Contains(out.Reason, "not acknowledged within 1s") || !strings.Contains(string(out.Receipt), `"repeats_sent":3`) {
		t.Errorf("unexpected outcome: %+v", out)
	}
}

// A receipt that is not there (another key's send, or no acknowledge) is a
// hard miss: the wait stops on the first 404.
func TestHandleWaitForAck_NotFoundAborts(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"title":"Not Found","status":404,"code":"notification_receipt.not_found"}`)
	}))
	defer srv.Close()
	result, err := handleWaitForAck(context.Background(), newReq(map[string]any{"notification_id": float64(9)}), client.NewAPIClient(srv.URL, "tok"))
	if err != nil || !result.IsError || !strings.Contains(resultText(t, result), "notification_receipt.not_found") {
		t.Errorf("want a tool error naming the code, got %v / %s", err, resultText(t, result))
	}
	if calls.Load() != 1 {
		t.Errorf("%d polls, want 1", calls.Load())
	}
}
