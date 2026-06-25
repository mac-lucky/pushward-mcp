package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
