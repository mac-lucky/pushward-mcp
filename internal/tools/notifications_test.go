package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

func TestNotificationTools_Requests(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		if r.URL.Path == "/notifications/retract" {
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"devices":3}`)
			return
		}
		io.WriteString(w, `{"items":[],"cursor":"57"}`)
	}))
	defer srv.Close()
	api := client.NewAPIClient(srv.URL, "tok")

	for _, call := range []struct {
		handler func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error)
		args    map[string]any
		want    string
	}{
		{handleListNotificationAnswers, map[string]any{}, `"cursor":"57"`},
		{handleListNotificationAnswers, map[string]any{"after": "57", "wait_seconds": float64(25), "limit": float64(10)}, `"cursor":"57"`},
		// An unquoted cursor is still that cursor, not a fresh start.
		{handleListNotificationAnswers, map[string]any{"after": float64(57)}, `"cursor":"57"`},
		{handleListNotificationAnswers, map[string]any{"after": "57", "limit": float64(500)}, `"cursor":"57"`},
		{handleListNotificationAnswers, map[string]any{"after": "57", "limit": float64(-3)}, `"cursor":"57"`},
		{handleRetractNotifications, map[string]any{"collapse_id": "garage-door"}, `{"devices":3}`},
	} {
		result, err := call.handler(context.Background(), newReq(call.args), api)
		if err != nil || result.IsError || !strings.Contains(resultText(t, result), call.want) {
			t.Errorf("args %v: %v %s", call.args, err, resultText(t, result))
		}
	}
	want := []string{
		"GET /notifications/answers ",
		// The hold is capped below the server's 25 s, as for the single answer.
		"GET /notifications/answers?after=57&limit=10&wait=20 ",
		"GET /notifications/answers?after=57 ",
		"GET /notifications/answers?after=57&limit=100 ",
		"GET /notifications/answers?after=57&limit=1 ",
		`POST /notifications/retract {"collapse_id":"garage-door"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A full wait slot is a tool error that names the code, so the model knows
// to retry rather than give up on the stream.
func TestListNotificationAnswers_WaitLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"title":"Too Many Requests","status":429,"code":"answer_wait.limit_exceeded","retry_after_ms":2000}`)
	}))
	defer srv.Close()
	args := map[string]any{"after": "57", "wait_seconds": float64(20)}
	result, err := handleListNotificationAnswers(context.Background(), newReq(args), client.NewAPIClient(srv.URL, "tok"))
	if err != nil || !result.IsError || !strings.Contains(resultText(t, result), "answer_wait.limit_exceeded") {
		t.Errorf("want a tool error naming the code, got %v / %s", err, resultText(t, result))
	}
}

// A cursor that is neither a string nor a whole number cannot be sent as it
// was meant, so the call fails before reaching the server.
func TestListNotificationAnswers_BadAfter(t *testing.T) {
	api := client.NewAPIClient("http://127.0.0.1:1", "tok")
	for name, after := range map[string]any{
		"fraction": float64(57.5),
		"too big":  float64(1 << 60),
		"bool":     true,
		"object":   map[string]any{"cursor": "57"},
	} {
		result, err := handleListNotificationAnswers(context.Background(), newReq(map[string]any{"after": after}), api)
		if err != nil || !result.IsError || !strings.Contains(resultText(t, result), "after must be the cursor string") {
			t.Errorf("%s: want a tool error about after, got %v / %s", name, err, resultText(t, result))
		}
	}
}

// The hosted server withholds upstream detail, so what is left of an error
// (the code, and the retry hint of a 429) has to be enough to act on.
func TestNotificationTools_RemoteModeErrors(t *testing.T) {
	client.SetRemoteMode(true)
	t.Cleanup(func() { client.SetRemoteMode(false) })
	type call func(*client.APIClient) (*mcp.CallToolResult, error)
	list := func(api *client.APIClient) (*mcp.CallToolResult, error) {
		return handleListNotificationAnswers(context.Background(), newReq(map[string]any{"after": "57", "wait_seconds": float64(20)}), api)
	}
	retract := func(api *client.APIClient) (*mcp.CallToolResult, error) {
		return handleRetractNotifications(context.Background(), newReq(map[string]any{"collapse_id": "garage-door"}), api)
	}
	for _, tc := range []struct {
		name   string
		call   call
		status int
		body   string
		want   string
	}{
		{"feed 429", list, http.StatusTooManyRequests,
			`{"status":429,"code":"answer_wait.limit_exceeded","detail":"two waits per key","retry_after_ms":2000}`,
			"returned 429: [answer_wait.limit_exceeded] (retry_after_ms=2000)"},
		{"feed 403", list, http.StatusForbidden,
			`{"status":403,"code":"integration_key.permission_denied","detail":"key k-17 lacks notifications:send"}`,
			"returned 403: [integration_key.permission_denied]"},
		{"feed 400", list, http.StatusBadRequest,
			`{"status":400,"code":"cursor.invalid","detail":"malformed cursor"}`,
			"returned 400: [cursor.invalid]"},
		{"retract 429", retract, http.StatusTooManyRequests,
			`{"status":429,"code":"rate_limit.exceeded","detail":"retract bucket of user u-1 empty","retry_after_ms":6000}`,
			"returned 429: [rate_limit.exceeded] (retry_after_ms=6000)"},
		{"retract 403", retract, http.StatusForbidden,
			`{"status":403,"code":"integration_key.permission_denied","detail":"key k-17 lacks notifications:send"}`,
			"returned 403: [integration_key.permission_denied]"},
		// Retract has no 400; a bad body is a 422 from validation.
		{"retract 422", retract, http.StatusUnprocessableEntity,
			`{"status":422,"title":"Unprocessable Entity","detail":"validation failed","errors":[{"location":"body.collapse_id"}]}`,
			"returned 422: upstream error (detail withheld)"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		result, err := tc.call(client.NewAPIClient(srv.URL, "tok"))
		srv.Close()
		text := resultText(t, result)
		if err != nil || !result.IsError || !strings.HasSuffix(text, tc.want) {
			t.Errorf("%s: got %v / %q, want a tool error ending %q", tc.name, err, text, tc.want)
		}
		for _, leak := range []string{"k-17", "u-1", "two waits", "malformed", "validation failed", "collapse_id"} {
			if strings.Contains(text, leak) {
				t.Errorf("%s: upstream detail %q reached the caller: %q", tc.name, leak, text)
			}
		}
	}
}

func TestRetractNotifications_NeedsCollapseID(t *testing.T) {
	api := client.NewAPIClient("http://127.0.0.1:1", "tok")
	for name, args := range map[string]map[string]any{
		"missing":    {},
		"empty":      {"collapse_id": ""},
		"not string": {"collapse_id": float64(7)},
	} {
		result, err := handleRetractNotifications(context.Background(), newReq(args), api)
		if err != nil || !result.IsError {
			t.Errorf("%s: want a tool error, got %v / %s", name, err, resultText(t, result))
		}
	}
}

// Retract takes notifications off the user's devices, so it must not be
// annotated as safe; the stream is a read.
func TestNotificationTools_Annotations(t *testing.T) {
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)
	list := s.GetTool("list_notification_answers")
	retract := s.GetTool("retract_notifications")
	if list == nil || retract == nil {
		t.Fatal("the answer stream and retract tools are not registered on the hosted server")
	}
	if a := list.Tool.Annotations; a.ReadOnlyHint == nil || !*a.ReadOnlyHint {
		t.Errorf("list_notification_answers: read-only hint %v", a.ReadOnlyHint)
	}
	a := retract.Tool.Annotations
	if a.DestructiveHint == nil || !*a.DestructiveHint || a.IdempotentHint == nil || !*a.IdempotentHint {
		t.Errorf("retract_notifications: destructive %v, idempotent %v", a.DestructiveHint, a.IdempotentHint)
	}
}
