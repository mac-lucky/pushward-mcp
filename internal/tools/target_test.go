package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// TestHandleTarget_Forwarded: an organization key's target reaches the API
// body as sent, on every tool that takes one. Omitted stays omitted; an
// explicit null clears the target on update_activity (merge patch) and is
// dropped on the creates, where it means nothing.
func TestHandleTarget_Forwarded(t *testing.T) {
	sendAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tools := []struct {
		name     string
		handler  func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error)
		args     map[string]any
		nullable bool
	}{
		{"create_activity", handleCreateActivity, map[string]any{"slug": "deploy", "name": "Deploy"}, false},
		{"create_notification", handleCreateNotification, map[string]any{"title": "T", "body": "B"}, false},
		{"create_scheduled_notification", handleCreateScheduledNotification, map[string]any{"title": "T", "body": "B", "send_at": sendAt}, false},
		{"update_activity", handleUpdateActivity, map[string]any{"slug": "deploy", "content_json": `{"progress":0.5}`}, true},
	}
	target := map[string]any{"groups": []any{"oncall"}, "tags": []any{"wall"}}
	for _, tool := range tools {
		for _, c := range []struct {
			name  string
			value any
			set   bool
			want  string // "" = absent
		}{
			{"omitted", nil, false, ""},
			{"object", target, true, `{"groups":["oncall"],"tags":["wall"]}`},
			{"null", nil, true, map[bool]string{true: "null", false: ""}[tool.nullable]},
		} {
			t.Run(tool.name+"/"+c.name, func(t *testing.T) {
				var raw map[string]json.RawMessage
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(body, &raw); err != nil {
						t.Fatalf("unmarshal request body: %v", err)
					}
					w.WriteHeader(http.StatusCreated)
					w.Write([]byte(`{"id":1}`))
				}))
				defer srv.Close()

				args := map[string]any{}
				for k, v := range tool.args {
					args[k] = v
				}
				if c.set {
					args["target"] = c.value
				}
				result, err := tool.handler(context.Background(), newReq(args), client.NewAPIClient(srv.URL, "tok"))
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.IsError {
					t.Fatalf("unexpected tool error: %s", resultText(t, result))
				}
				got, present := raw["target"]
				if c.want == "" {
					if present {
						t.Errorf("target = %s, want it omitted", got)
					}
					return
				}
				if string(got) != c.want {
					t.Errorf("target = %s, want %s", got, c.want)
				}
				if tool.name == "update_activity" {
					if content := string(raw["content"]); content != `{"progress":0.5}` {
						t.Errorf("content = %s, want it forwarded alongside the target", content)
					}
				}
			})
		}
	}
}

// TestTargetSchemaProperties: the target param shows the model its fields
// instead of a bare object.
func TestTargetSchemaProperties(t *testing.T) {
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)
	for _, name := range []string{"create_activity", "update_activity", "create_notification", "create_scheduled_notification"} {
		st := s.GetTool(name)
		if st == nil {
			t.Fatalf("%s not registered", name)
		}
		target, ok := st.Tool.InputSchema.Properties["target"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no target param", name)
		}
		if _, ok := st.Tool.InputSchema.Properties["clear_target"]; ok != (name == "update_activity") {
			t.Errorf("%s: clear_target registered = %v, want it on update_activity only", name, ok)
		}
		props, _ := target["properties"].(map[string]any)
		for _, field := range []string{"groups", "tags", "members"} {
			p, _ := props[field].(map[string]any)
			items, _ := p["items"].(map[string]any)
			if p["type"] != "array" || items["type"] != "string" {
				t.Errorf("%s: target.%s = %v, want an array of strings", name, field, props[field])
			}
		}
	}
}

// TestHandleUpdateActivity_ClearTarget: clear_target sends the null that
// clears the target, for clients whose schemas cannot carry a null; it does
// not combine with a target. An empty target object is skipped, on every tool.
func TestHandleUpdateActivity_ClearTarget(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    map[string]any
		want    string // "" = absent
		wantErr bool
	}{
		{"clear", map[string]any{"clear_target": true}, "null", false},
		{"clear false", map[string]any{"clear_target": false}, "", false},
		{"clear with empty object", map[string]any{"clear_target": true, "target": map[string]any{}}, "null", false},
		{"clear with target", map[string]any{"clear_target": true, "target": map[string]any{"groups": []any{"oncall"}}}, "", true},
		{"empty object", map[string]any{"target": map[string]any{}}, "", false},
		{"clear not a boolean", map[string]any{"clear_target": "please"}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &raw)
				w.Write([]byte(`{"slug":"deploy"}`))
			}))
			defer srv.Close()
			args := map[string]any{"slug": "deploy", "content_json": "{}"}
			for k, v := range c.args {
				args[k] = v
			}
			result, err := handleUpdateActivity(context.Background(), newReq(args), client.NewAPIClient(srv.URL, "tok"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.IsError != c.wantErr {
				t.Fatalf("tool error = %v (%s), want %v", result.IsError, resultText(t, result), c.wantErr)
			}
			if c.wantErr {
				if calls != 0 {
					t.Errorf("%d requests sent after a refused call", calls)
				}
				return
			}
			got, present := raw["target"]
			if c.want == "" && present {
				t.Errorf("target = %s, want it omitted", got)
			}
			if c.want != "" && string(got) != c.want {
				t.Errorf("target = %s, want %s", got, c.want)
			}
		})
	}
	// Creates skip an empty target too.
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()
	result, err := handleCreateNotification(context.Background(), newReq(map[string]any{"title": "T", "body": "B", "target": map[string]any{}}), client.NewAPIClient(srv.URL, "tok"))
	if err != nil || result.IsError {
		t.Fatalf("create_notification with an empty target: %v %v", err, result)
	}
	if got, ok := raw["target"]; ok {
		t.Errorf("empty target forwarded as %s", got)
	}
}
