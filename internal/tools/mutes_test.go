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

// The mute tools send what the caller gave and nothing else, so the server
// defaults (mode silent, no end) apply to what was left out, and the delete
// path carries the id escaped.
func TestMuteTools_Requests(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" "+string(body))
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"m-1"}`)
		default:
			io.WriteString(w, `{"mutes":[]}`)
		}
	}))
	defer srv.Close()
	api := client.NewAPIClient(srv.URL, "tok")

	for _, call := range []struct {
		handler func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error)
		args    map[string]any
		want    string
	}{
		{handleListMutes, nil, `{"mutes":[]}`},
		{handleCreateMute, map[string]any{"target": "*", "applies_to": "both", "until": "2030-01-01T10:00:00Z"}, `"m-1"`},
		// Clients that fill every optional param send empty values; they
		// must not reach the server as a blank mode or an empty schedule.
		{handleCreateMute, map[string]any{"target": "sonarr", "applies_to": "notifications", "mode": "", "label": nil, "until": "", "schedule": map[string]any{}}, `"m-1"`},
		{handleCreateMute, map[string]any{
			"target": "deploy-prod", "applies_to": "activities", "mode": "off", "label": "Deploys",
			"schedule": map[string]any{"start_minute": float64(1320), "end_minute": float64(0), "time_zone": "Europe/Warsaw"},
		}, `"m-1"`},
		{handleDeleteMute, map[string]any{"mute_id": "11111111-1111-4111-8111-111111111111"}, "deleted"},
		{handleDeleteMute, map[string]any{"mute_id": "../widgets/x"}, "deleted"},
	} {
		result, err := call.handler(context.Background(), newReq(call.args), api)
		if err != nil || result.IsError || !strings.Contains(resultText(t, result), call.want) {
			t.Errorf("args %v: %v %s", call.args, err, resultText(t, result))
		}
	}
	want := []string{
		"GET /app/mutes ",
		`POST /app/mutes {"target":"*","applies_to":"both","until":"2030-01-01T10:00:00Z"}`,
		`POST /app/mutes {"target":"sonarr","applies_to":"notifications"}`,
		// end_minute 0 (midnight) is sent, not dropped as unset.
		`POST /app/mutes {"target":"deploy-prod","applies_to":"activities","label":"Deploys","mode":"off","schedule":{"end_minute":0,"start_minute":1320,"time_zone":"Europe/Warsaw"}}`,
		"DELETE /app/mutes/11111111-1111-4111-8111-111111111111 ",
		"DELETE /app/mutes/..%2Fwidgets%2Fx ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMuteTools_MissingArgsNeverSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	api := client.NewAPIClient(srv.URL, "tok")
	for name, c := range map[string]struct {
		handler func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error)
		args    map[string]any
	}{
		"create without target":     {handleCreateMute, map[string]any{"applies_to": "both"}},
		"create without applies_to": {handleCreateMute, map[string]any{"target": "*"}},
		// Dropped, a unix-time until would leave a mute that never ends.
		"create with a numeric until": {handleCreateMute, map[string]any{"target": "*", "applies_to": "both", "until": float64(1764072000)}},
		"create with a boolean mode":  {handleCreateMute, map[string]any{"target": "*", "applies_to": "both", "mode": true}},
		"delete without id":           {handleDeleteMute, nil},
		"delete with empty id":        {handleDeleteMute, map[string]any{"mute_id": ""}},
	} {
		if r, err := c.handler(context.Background(), newReq(c.args), api); err != nil || !r.IsError {
			t.Errorf("%s: want a tool error, got %v", name, err)
		}
	}
}

// The hosted server serves the mute tools like every API tool, with the
// user's own key; a key without the account permission gets the refusal
// code, which survives the hosted mode's redaction so the agent can tell the
// user what to grant.
func TestMuteTools_Hosted(t *testing.T) {
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)
	val := func(b *bool) bool { return b != nil && *b }
	for name, want := range map[string]struct{ readOnly, destructive, idempotent bool }{
		"list_mutes":  {true, false, false},
		"create_mute": {false, false, false},
		"delete_mute": {false, true, true},
	} {
		st := s.GetTool(name)
		if st == nil {
			t.Errorf("%s not registered without a relay client", name)
			continue
		}
		a := st.Tool.Annotations
		if val(a.ReadOnlyHint) != want.readOnly || (!want.readOnly && val(a.DestructiveHint) != want.destructive) || val(a.IdempotentHint) != want.idempotent {
			t.Errorf("%s annotations = %+v, want %+v", name, a, want)
		}
		if !strings.Contains(st.Tool.Description, "account permission") {
			t.Errorf("%s description does not name the permission it needs", name)
		}
	}

	client.SetRemoteMode(true)
	t.Cleanup(func() { client.SetRemoteMode(false) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"status":403,"code":"integration_key.permission_denied","detail":"insufficient permissions: requires account:manage"}`)
	}))
	defer srv.Close()
	r, _ := handleCreateMute(context.Background(), newReq(map[string]any{"target": "*", "applies_to": "both"}), client.NewAPIClient(srv.URL, "tok"))
	if text := resultText(t, r); !r.IsError || !strings.HasSuffix(text, "returned 403: [integration_key.permission_denied]") {
		t.Errorf("result %q, want the refusal code without the detail", text)
	}
}
