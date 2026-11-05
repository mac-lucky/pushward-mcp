package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// TestHandleCreateNotification_PushPresence exercises the generated handler's
// boolean presence logic (req.RequireBool -> *bool). This is the behavior the
// *bool/omitempty change exists for, and it is NOT covered by the client-level
// marshaling test: omitting push must drop the key entirely (so the server
// default of true applies), while an explicit false must be sent as false. A
// regression to the old `GetBool(..., false)` codegen - which forced push=false
// on omission, silently disabling APNs push - would fail this test.
func TestHandleCreateNotification_PushPresence(t *testing.T) {
	cases := []struct {
		name        string
		args        map[string]any
		wantPresent bool
		wantValue   bool
	}{
		{"omitted", map[string]any{"title": "T", "body": "B"}, false, false},
		{"explicit false", map[string]any{"title": "T", "body": "B", "push": false}, true, false},
		{"explicit true", map[string]any{"title": "T", "body": "B", "push": true}, true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Fatalf("unmarshal request body: %v", err)
				}
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"id":"notif-1"}`))
			}))
			defer srv.Close()

			api := client.NewAPIClient(srv.URL, "tok")
			result, err := handleCreateNotification(context.Background(), newReq(c.args), api)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.IsError {
				t.Fatalf("unexpected tool error: %s", resultText(t, result))
			}

			pushRaw, present := raw["push"]
			if present != c.wantPresent {
				t.Fatalf("push present=%v, want %v (body keys=%v)", present, c.wantPresent, raw)
			}
			if present {
				var got bool
				if err := json.Unmarshal(pushRaw, &got); err != nil {
					t.Fatalf("push is not a bool: %s", pushRaw)
				}
				if got != c.wantValue {
					t.Errorf("push=%v, want %v", got, c.wantValue)
				}
			}
		})
	}
}

// TestHandleCreateNotification_MissingRequiredIsError covers the generated
// RequireString error path: omitting a required string returns an MCP error
// result (not a Go error), rather than the old error-swallowing inline closure
// that sent an empty string to the server.
func TestHandleCreateNotification_MissingRequiredIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server must not be called when required args are missing")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	api := client.NewAPIClient(srv.URL, "tok")
	// body present, title missing.
	result, err := handleCreateNotification(context.Background(), newReq(map[string]any{"body": "B"}), api)
	if err != nil {
		t.Fatalf("expected a nil Go error, got: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true when a required string is missing, got: %s", resultText(t, result))
	}
}

// Snooze minutes reach the body as integers, on both send tools; a
// fractional minute is refused here instead of being rounded into another
// duration.
func TestHandleCreateNotification_Snooze(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()
	api := client.NewAPIClient(srv.URL, "tok")
	ctx := context.Background()

	args := map[string]any{"title": "Garage open", "body": "Since 21:40", "snooze": []any{float64(15), float64(60)}}
	if result, err := handleCreateNotification(ctx, newReq(args), api); err != nil || result.IsError {
		t.Fatalf("create_notification: %v %s", err, resultText(t, result))
	}
	args["send_at"] = "2030-01-01T09:00:00Z"
	if result, err := handleCreateScheduledNotification(ctx, newReq(args), api); err != nil || result.IsError {
		t.Fatalf("create_scheduled_notification: %v %s", err, resultText(t, result))
	}
	for i, b := range bodies {
		var got struct {
			Snooze json.RawMessage `json:"snooze"`
		}
		if err := json.Unmarshal([]byte(b), &got); err != nil || string(got.Snooze) != "[15,60]" {
			t.Errorf("request %d: snooze %s, want [15,60] (%v)", i, got.Snooze, err)
		}
	}

	bad := map[string]any{"title": "T", "body": "B", "snooze": []any{7.5}}
	result, err := handleCreateNotification(ctx, newReq(bad), api)
	if err != nil || !result.IsError {
		t.Errorf("a fractional minute: want a tool error, got %v / %s", err, resultText(t, result))
	}
	if len(bodies) != 2 {
		t.Errorf("%d requests, want 2: the fractional one must not be sent", len(bodies))
	}
}

// An approval option can ask for a device unlock before it fires. content_json
// is forwarded as raw JSON, so the key has to be advertised in the tool
// description and arrive in the PATCH body untouched.
func TestHandleUpdateActivity_ApprovalAuthenticationRequired(t *testing.T) {
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)
	st := s.GetTool("update_activity")
	if st == nil {
		t.Fatal("update_activity not registered")
	}
	prop, _ := st.Tool.InputSchema.Properties["content_json"].(map[string]any)
	if desc, _ := prop["description"].(string); !strings.Contains(desc, "authentication_required") {
		t.Errorf("content_json description does not offer authentication_required on approval options: %s", desc)
	}

	var got struct {
		Content struct {
			Options []map[string]json.RawMessage `json:"options"`
		} `json:"content"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}
		w.Write([]byte(`{"slug":"deploy"}`))
	}))
	defer srv.Close()

	content := `{"template":"approval","state":"Deploy 2.4?","options":[{"id":"deploy","title":"Deploy","authentication_required":true},{"id":"hold","title":"Hold"}]}`
	result, err := handleUpdateActivity(context.Background(), newReq(map[string]any{"slug": "deploy", "content_json": content}), client.NewAPIClient(srv.URL, "tok"))
	if err != nil || result.IsError {
		t.Fatalf("update_activity: %v %s", err, resultText(t, result))
	}
	if len(got.Content.Options) != 2 {
		t.Fatalf("sent %d options, want 2", len(got.Content.Options))
	}
	if v := string(got.Content.Options[0]["authentication_required"]); v != "true" {
		t.Errorf("first option authentication_required = %q, want true", v)
	}
	if _, ok := got.Content.Options[1]["authentication_required"]; ok {
		t.Error("second option gained an authentication_required it was not sent with")
	}
}
