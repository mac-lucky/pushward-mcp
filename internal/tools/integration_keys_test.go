package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"

	"github.com/mac-lucky/pushward-mcp/internal/client"
	"github.com/mac-lucky/pushward-mcp/internal/docs"
)

const testKeyID = "22222222-2222-4222-8222-222222222222"

type keyCall struct {
	method, path, contentType string
	body                      map[string]json.RawMessage
	rawBody                   []byte
}

// keyServer records the one request a tool makes and answers with status/body.
func keyServer(t *testing.T, status int, resp string) (*client.APIClient, *[]keyCall) {
	t.Helper()
	var calls []keyCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := keyCall{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), rawBody: raw}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &c.body); err != nil {
				t.Errorf("request body is not a JSON object: %s", raw)
			}
		}
		calls = append(calls, c)
		if status >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return client.NewAPIClient(srv.URL, "hlk_test"), &calls
}

func TestCreateIntegrationKey_ForwardsFields(t *testing.T) {
	api, calls := keyServer(t, http.StatusCreated, `{"id":"`+testKeyID+`","key":"hlk_new"}`)
	result, err := handleCreateIntegrationKey(context.Background(), newReq(map[string]any{
		"name": "ci", "activity_slugs": []any{"ci-*", "deploy"}, "widget_slugs": []any{"cpu"},
		"permissions": map[string]any{"activities": "manage", "notifications": "send", "emails": "none"},
		"expires_at":  "2099-01-02T03:04:05Z",
	}), api)
	if err != nil || result.IsError {
		t.Fatalf("unexpected result: %v %s", err, resultText(t, result))
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/integrations/keys" {
		t.Fatalf("request = %s %s", c.method, c.path)
	}
	want := map[string]string{
		"name": `"ci"`, "activity_slugs": `["ci-*","deploy"]`, "widget_slugs": `["cpu"]`,
		"permissions": `{"activities":"manage","notifications":"send","emails":"none"}`,
		"expires_at":  `"2099-01-02T03:04:05Z"`,
	}
	if len(c.body) != len(want) {
		t.Errorf("body keys = %v, want %v", c.body, want)
	}
	for k, v := range want {
		if string(c.body[k]) != v {
			t.Errorf("%s = %s, want %s", k, c.body[k], v)
		}
	}
	if !strings.Contains(resultText(t, result), "hlk_new") {
		t.Errorf("secret not returned to the caller: %s", resultText(t, result))
	}
}

func TestCreateIntegrationKey_EmptySlugsRefused(t *testing.T) {
	api, calls := keyServer(t, http.StatusCreated, `{}`)
	r, _ := handleCreateIntegrationKey(context.Background(), newReq(map[string]any{"name": "ci", "activity_slugs": []any{}}), api)
	if !r.IsError || len(*calls) != 0 {
		t.Fatalf("IsError=%v calls=%d, want a tool error and no request", r.IsError, len(*calls))
	}
}

func TestCreateIntegrationKey_OmitsUnsetFields(t *testing.T) {
	api, calls := keyServer(t, http.StatusCreated, `{}`)
	if result, _ := handleCreateIntegrationKey(context.Background(), newReq(map[string]any{"name": "ci"}), api); result.IsError {
		t.Fatalf("tool error: %s", resultText(t, result))
	}
	if body := (*calls)[0].body; len(body) != 1 || string(body["name"]) != `"ci"` {
		t.Errorf("body = %v, want only name", body)
	}
}

// A lenient parse would turn [123] into [], which the server reads as "no
// restriction"; every malformed value must stop before the request.
func TestIntegrationKeyTools_RejectMalformedArgs(t *testing.T) {
	bad := []struct {
		name string
		args map[string]any
	}{
		{"number slug", map[string]any{"activity_slugs": []any{123}}},
		{"mixed slugs", map[string]any{"activity_slugs": []any{"a", 1}}},
		{"string slugs", map[string]any{"activity_slugs": "a"}},
		{"null slugs", map[string]any{"activity_slugs": nil}},
		{"number widget slug", map[string]any{"widget_slugs": []any{1}}},
		{"null widget slugs", map[string]any{"widget_slugs": nil}},
		// The pre-levels parameters: dropping them silently would change what the key can do.
		{"legacy bool", map[string]any{"notifications": true}},
		{"legacy scope", map[string]any{"scope": "activity:manage"}},
		{"permissions not an object", map[string]any{"permissions": "send"}},
		{"empty permissions", map[string]any{"permissions": map[string]any{}}},
		{"unknown level", map[string]any{"permissions": map[string]any{"widgets": "admin"}}},
		{"level of another resource", map[string]any{"permissions": map[string]any{"emails": "schedule"}}},
		{"unknown resource", map[string]any{"permissions": map[string]any{"billing": "read"}}},
		{"numeric level", map[string]any{"permissions": map[string]any{"activities": 1}}},
		{"bad expiry", map[string]any{"expires_at": "next week"}},
		{"past expiry", map[string]any{"expires_at": "2001-01-01T00:00:00Z"}},
		{"numeric expiry", map[string]any{"expires_at": 1893456000}},
	}
	for _, tc := range bad {
		t.Run("create/"+tc.name, func(t *testing.T) {
			api, calls := keyServer(t, http.StatusCreated, `{}`)
			args := map[string]any{"name": "ci"}
			for k, v := range tc.args {
				args[k] = v
			}
			result, _ := handleCreateIntegrationKey(context.Background(), newReq(args), api)
			if !result.IsError || len(*calls) != 0 {
				t.Fatalf("IsError=%v calls=%d, want a tool error and no request", result.IsError, len(*calls))
			}
		})
		t.Run("update/"+tc.name, func(t *testing.T) {
			api, calls := keyServer(t, http.StatusOK, `{}`)
			args := map[string]any{"key_id": testKeyID}
			for k, v := range tc.args {
				args[k] = v
			}
			result, _ := handleUpdateIntegrationKey(context.Background(), newReq(args), api)
			if !result.IsError || len(*calls) != 0 {
				t.Fatalf("IsError=%v calls=%d, want a tool error and no request", result.IsError, len(*calls))
			}
		})
	}
}

func TestUpdateIntegrationKey_SlugsOmittedVsEmpty(t *testing.T) {
	api, calls := keyServer(t, http.StatusOK, `{}`)
	ctx := context.Background()
	if r, _ := handleUpdateIntegrationKey(ctx, newReq(map[string]any{"key_id": testKeyID, "permissions": map[string]any{"emails": "none"}}), api); r.IsError {
		t.Fatalf("tool error: %s", resultText(t, r))
	}
	if r, _ := handleUpdateIntegrationKey(ctx, newReq(map[string]any{"key_id": testKeyID, "activity_slugs": []any{}}), api); r.IsError {
		t.Fatalf("tool error: %s", resultText(t, r))
	}
	if _, present := (*calls)[0].body["activity_slugs"]; present {
		t.Errorf("omitted activity_slugs was sent: %s", (*calls)[0].rawBody)
	}
	if got := string((*calls)[1].body["activity_slugs"]); got != "[]" {
		t.Errorf("activity_slugs=[] sent as %q, want []", got)
	}
	if c := (*calls)[1]; c.method != http.MethodPatch || c.path != "/integrations/keys/"+testKeyID {
		t.Errorf("request = %s %s", c.method, c.path)
	}
}

// expires_at "never" becomes JSON null on update (removing the expiry) and is
// refused on create, where omitting it already means never.
// widget_slugs behaves like activity_slugs: [] is refused on create (it would
// not restrict anything) and sent as [] on update, where it clears the list.
func TestIntegrationKey_WidgetSlugsEmpty(t *testing.T) {
	api, calls := keyServer(t, http.StatusOK, `{}`)
	if r, _ := handleCreateIntegrationKey(context.Background(), newReq(map[string]any{"name": "ci", "widget_slugs": []any{}}), api); !r.IsError || len(*calls) != 0 {
		t.Fatalf("create with []: IsError=%v calls=%d, want a tool error and no request", r.IsError, len(*calls))
	}
	if r, _ := handleUpdateIntegrationKey(context.Background(), newReq(map[string]any{"key_id": testKeyID, "widget_slugs": []any{}}), api); r.IsError {
		t.Fatalf("update with []: %s", resultText(t, r))
	}
	if got := string((*calls)[0].body["widget_slugs"]); got != "[]" {
		t.Errorf("widget_slugs=[] sent as %q, want []", got)
	}
}

func TestIntegrationKey_ExpiryNever(t *testing.T) {
	api, calls := keyServer(t, http.StatusOK, `{}`)
	if r, _ := handleUpdateIntegrationKey(context.Background(), newReq(map[string]any{"key_id": testKeyID, "expires_at": "never"}), api); r.IsError {
		t.Fatalf("tool error: %s", resultText(t, r))
	}
	if got := string((*calls)[0].body["expires_at"]); got != "null" {
		t.Errorf("expires_at never sent as %q, want null", got)
	}
	if r, _ := handleCreateIntegrationKey(context.Background(), newReq(map[string]any{"name": "ci", "expires_at": "never"}), api); !r.IsError || len(*calls) != 1 {
		t.Errorf("create with never: IsError=%v calls=%d, want a tool error and no request", r.IsError, len(*calls))
	}
}

func TestUpdateIntegrationKey_NothingToUpdate(t *testing.T) {
	api, calls := keyServer(t, http.StatusOK, `{}`)
	r, _ := handleUpdateIntegrationKey(context.Background(), newReq(map[string]any{"key_id": testKeyID}), api)
	if !r.IsError || len(*calls) != 0 {
		t.Fatalf("IsError=%v calls=%d, want a tool error and no request", r.IsError, len(*calls))
	}
}

func TestRevokeAndRollIntegrationKey(t *testing.T) {
	api, calls := keyServer(t, http.StatusNoContent, "")
	if r, _ := handleRevokeIntegrationKey(context.Background(), newReq(map[string]any{"key_id": testKeyID}), api); r.IsError || resultText(t, r) != "revoked" {
		t.Fatalf("revoke: %s", resultText(t, r))
	}
	if c := (*calls)[0]; c.method != http.MethodDelete || c.path != "/integrations/keys/"+testKeyID {
		t.Errorf("revoke request = %s %s", c.method, c.path)
	}

	api, calls = keyServer(t, http.StatusOK, `{"key":"hlk_rolled"}`)
	r, _ := handleRollIntegrationKey(context.Background(), newReq(map[string]any{"key_id": testKeyID}), api)
	if r.IsError || !strings.Contains(resultText(t, r), "hlk_rolled") {
		t.Fatalf("roll: %s", resultText(t, r))
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/integrations/keys/"+testKeyID+"/roll" {
		t.Errorf("roll request = %s %s", c.method, c.path)
	}
	if len(c.rawBody) != 0 || c.contentType != "" {
		t.Errorf("roll sent a body (%q, Content-Type %q), want none", c.rawBody, c.contentType)
	}
}

func TestIntegrationKeyTools_BadKeyIDNeverSent(t *testing.T) {
	for _, id := range []string{"", "../widgets", "not-a-uuid", testKeyID + "/roll"} {
		api, calls := keyServer(t, http.StatusOK, `{}`)
		ctx := context.Background()
		args := map[string]any{"key_id": id, "emails": true}
		for name, h := range map[string]func(context.Context, mcp.CallToolRequest, *client.APIClient) (*mcp.CallToolResult, error){
			"revoke": handleRevokeIntegrationKey, "update": handleUpdateIntegrationKey, "roll": handleRollIntegrationKey,
		} {
			if r, _ := h(ctx, newReq(args), api); !r.IsError {
				t.Errorf("%s with key_id %q: want a tool error", name, id)
			}
		}
		if len(*calls) != 0 {
			t.Errorf("key_id %q reached the server %d times", id, len(*calls))
		}
	}
}

// Hosted mode redacts upstream detail; the refusal code must still reach the
// agent so it can tell "use the default key" from other failures.
func TestIntegrationKeyTools_RefusalCodeSurvivesRemoteMode(t *testing.T) {
	t.Cleanup(func() { client.SetRemoteMode(false) })
	for _, remote := range []bool{false, true} {
		client.SetRemoteMode(remote)
		api, _ := keyServer(t, http.StatusForbidden, `{"code":"integration_key.default_key_required","detail":"only the account's default integration key can manage integration keys","status":403}`)
		r, _ := handleListIntegrationKeys(context.Background(), newReq(nil), api)
		if !r.IsError || !strings.Contains(resultText(t, r), "integration_key.default_key_required") {
			t.Errorf("remote=%v: result %q, want the refusal code", remote, resultText(t, r))
		}
	}
}

func TestIntegrationKeyTools_RegisteredWithHints(t *testing.T) {
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)

	type hints struct{ readOnly, destructive, idempotent bool }
	want := map[string]hints{
		"create_integration_key": {false, false, false},
		"list_integration_keys":  {true, false, false},
		"revoke_integration_key": {false, true, true},
		"update_integration_key": {false, true, true},
		"roll_integration_key":   {false, true, false},
	}
	val := func(b *bool) bool { return b != nil && *b }
	for name, w := range want {
		st := s.GetTool(name)
		if st == nil {
			t.Errorf("%s not registered without a relay client", name)
			continue
		}
		a := st.Tool.Annotations
		got := hints{val(a.ReadOnlyHint), val(a.DestructiveHint), val(a.IdempotentHint)}
		if !w.readOnly && got.readOnly {
			t.Errorf("%s is marked read-only", name)
		}
		if w.readOnly && !got.readOnly {
			t.Errorf("%s: readOnlyHint not set", name)
		}
		if !w.readOnly && got.destructive != w.destructive {
			t.Errorf("%s: destructiveHint = %v, want %v", name, got.destructive, w.destructive)
		}
		if got.idempotent != w.idempotent {
			t.Errorf("%s: idempotentHint = %v, want %v", name, got.idempotent, w.idempotent)
		}
		if !val(a.OpenWorldHint) {
			t.Errorf("%s: openWorldHint not set", name)
		}
	}
}

// Every field the server accepts on create/update is a tool parameter, so a
// field added to the spec shows up here instead of being unreachable.
func TestIntegrationKeyTools_SpecParity(t *testing.T) {
	data, err := docs.Doc(docs.KindAPIOpenAPI)
	if err != nil {
		t.Fatalf("read the embedded API spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(data), &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	s := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	RegisterAll(s, client.NewAPIClient("http://127.0.0.1:1", "tok"), nil)

	for schema, tool := range map[string]string{
		"CreateIntegrationKeyInputBody": "create_integration_key",
		"UpdateIntegrationKeyInputBody": "update_integration_key",
	} {
		props := spec.Components.Schemas[schema].Properties
		if len(props) == 0 {
			t.Fatalf("%s missing from openapi.yaml", schema)
		}
		st := s.GetTool(tool)
		if st == nil {
			t.Fatalf("%s not registered", tool)
		}
		params := st.Tool.InputSchema.Properties
		for name := range props {
			// The legacy fields are refused by the tools on purpose; see legacyKeyArgs.
			if name == "$schema" || slices.Contains(legacyKeyArgs, name) {
				continue
			}
			if _, ok := params[name]; !ok {
				t.Errorf("%s accepts %q but %s has no such parameter", schema, name, tool)
			}
		}
	}
}
