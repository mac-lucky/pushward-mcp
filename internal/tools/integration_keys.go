package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// Integration key management. Only the account's default key may call these
// endpoints; the server answers every other key with
// integration_key.default_key_required, so the tools are registered in every
// mode and the server stays the one place that decides.
//
// Hand-written rather than generated (see skipOperations): the slug lists are
// string arrays the generator drops, roll is a POST without a body, omitted vs
// [] slug lists on update mean different things, and expires_at: null on
// update removes the expiry.
//
// The tools take the permissions object, not the legacy scope and
// notifications/widgets/emails booleans the API still accepts; the server
// refuses a body that mixes the two.

// keyResource is one resource of a key's permissions and its levels, weakest
// first.
type keyResource struct {
	resource string
	levels   []string
	doc      string
}

var integrationKeyLevels = []keyResource{
	{"activities", []string{"none", "read", "update", "manage"}, "read: list and get activities; update: also PATCH them; manage: also create and delete them"},
	{"notifications", []string{"none", "send", "schedule"}, "send: POST /notifications and read answers; schedule: also scheduled notifications"},
	{"widgets", []string{"none", "read", "write"}, "read: list and get widgets; write: also create, update and delete them"},
	{"emails", []string{"none", "send"}, "send: send transactional emails to verified recipients"},
}

func permissionsParam(doc string) mcp.ToolOption {
	props := map[string]any{}
	for _, l := range integrationKeyLevels {
		props[l.resource] = map[string]any{"type": "string", "enum": l.levels, "description": l.doc}
	}
	return mcp.WithObject("permissions", mcp.Description(doc), mcp.Properties(props))
}

func registerIntegrationKeyTools(s *mcpserver.MCPServer, api *client.APIClient) {
	s.AddTool(
		mcp.NewTool("create_integration_key",
			mcp.WithDescription("Create an integration key (hlk_...) for this account. The secret comes back once, in `key`, and cannot be read again, so hand it straight to whatever needs it. Give the key only what its integration needs: e.g. permissions {\"notifications\": \"send\"} for an alerting bridge. Only the account's default key can do this (other keys get integration_key.default_key_required), and the new key cannot have a permission above the default key's own (integration_key.grant_exceeded). The new key does not depend on the default key: it keeps working if the default key is revoked or rolled, so revoke it when it is no longer needed."),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("name", mcp.Required(), mcp.Description("Name shown in the app, e.g. the integration it is for (1-256 characters)")),
			permissionsParam("The key's level per resource. A resource left out gets none. Omit permissions entirely for the old default: activities update, nothing else."),
			mcp.WithArray("activity_slugs", mcp.WithStringItems(), mcp.Description("Restrict the key to these activity slugs or trailing-* patterns, e.g. [\"ci-*\"]. Omit for all activities; an empty list is refused, since the server would read it as all activities too.")),
			mcp.WithArray("widget_slugs", mcp.WithStringItems(), mcp.Description("Restrict the key to these widget slugs or trailing-* patterns. Omit for all widgets; an empty list is refused for the same reason.")),
			mcp.WithString("expires_at", mcp.Description("When the key stops working, RFC 3339 (e.g. 2026-12-31T00:00:00Z), in the future. Omit for a key that never expires.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCreateIntegrationKey(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("list_integration_keys",
			mcp.WithDescription("List this account's active integration keys, newest first, without their secrets: id, name, permissions (one level per resource), activity_slugs, widget_slugs, expires_at, created_at, last_used_at. The one with is_default=true is the default key. Only the account's default key can do this."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleListIntegrationKeys(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("revoke_integration_key",
			mcp.WithDescription("Revoke an integration key by id (from list_integration_keys). Requests made with it fail from then on, and its pending scheduled notifications are dropped. A default key cannot be revoked here (integration_key.default_target), including the one this connection uses; that is done in the PushWard app."),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("key_id", mcp.Required(), mcp.Description("Key id (UUID) from list_integration_keys")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleRevokeIntegrationKey(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("update_integration_key",
			mcp.WithDescription("Change an integration key's permissions, slug restrictions or expiry. Omitted fields stay as they are; activity_slugs=[] or widget_slugs=[] removes that restriction and expires_at=\"never\" removes the expiry. Pass at least one field. A default key cannot be changed here (integration_key.default_target), and the result cannot have a permission above the default key's own (integration_key.grant_exceeded)."),
			// Changing a live credential's permissions can break the integration
			// using it, so this is not an additive update.
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("key_id", mcp.Required(), mcp.Description("Key id (UUID) from list_integration_keys")),
			permissionsParam("Levels to change; resources left out keep their level."),
			mcp.WithArray("activity_slugs", mcp.WithStringItems(), mcp.Description("New activity slug restriction (slugs or trailing-* patterns); [] removes it")),
			mcp.WithArray("widget_slugs", mcp.WithStringItems(), mcp.Description("New widget slug restriction (slugs or trailing-* patterns); [] removes it")),
			mcp.WithString("expires_at", mcp.Description("New expiry, RFC 3339 and in the future, or \"never\" to remove the expiry")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleUpdateIntegrationKey(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("roll_integration_key",
			mcp.WithDescription("Replace an integration key's secret and return the new one once, in `key`. The old secret stops working at once, so whatever still uses it fails until it gets the new one. A default key cannot be rolled here (integration_key.default_target), nor a key with a permission above the default key's own (integration_key.grant_exceeded)."),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("key_id", mcp.Required(), mcp.Description("Key id (UUID) from list_integration_keys")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleRollIntegrationKey(ctx, req, api)
		},
	)
}

func handleCreateIntegrationKey(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	fields, err := integrationKeyFields(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// The server stores [] as "no restriction"; an agent passing [] most
	// likely meant "none", which the API cannot express.
	for name, slugs := range map[string]*[]string{"activity_slugs": fields.ActivitySlugs, "widget_slugs": fields.WidgetSlugs} {
		if slugs != nil && len(*slugs) == 0 {
			return mcp.NewToolResultError(name + "=[] would not restrict the key at all; omit " + name + " for that, or list the slugs it may use"), nil
		}
	}
	if string(fields.ExpiresAt) == "null" {
		return mcp.NewToolResultError(`expires_at="never" only applies to update_integration_key; omit expires_at for a key that never expires`), nil
	}
	raw, err := api.CreateIntegrationKey(ctx, client.CreateIntegrationKeyInput{Name: name, IntegrationKeyFields: fields})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleListIntegrationKeys(ctx context.Context, _ mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	raw, err := api.ListIntegrationKeys(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleRevokeIntegrationKey(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("key_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := api.RevokeIntegrationKey(ctx, id); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("revoked"), nil
}

func handleUpdateIntegrationKey(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("key_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	fields, err := integrationKeyFields(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if fields.IsZero() {
		return mcp.NewToolResultError("nothing to update: pass permissions, activity_slugs, widget_slugs or expires_at"), nil
	}
	raw, err := api.UpdateIntegrationKey(ctx, id, fields)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleRollIntegrationKey(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("key_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	raw, err := api.RollIntegrationKey(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// legacyKeyArgs are the parameters these tools took before permission levels.
// An agent working from an older tool list may still send them; dropping them
// silently would create or leave a key with other permissions than it asked
// for, so they are refused with the new spelling.
var legacyKeyArgs = []string{"scope", "notifications", "widgets", "emails"}

// integrationKeyFields reads the fields create and update share.
func integrationKeyFields(req mcp.CallToolRequest) (f client.IntegrationKeyFields, err error) {
	for _, name := range legacyKeyArgs {
		if _, sent := req.GetArguments()[name]; sent {
			return f, fmt.Errorf("%s is no longer a parameter: pass permissions, e.g. {\"activities\": \"update\", \"notifications\": \"send\"}", name)
		}
	}
	if f.Permissions, err = optionalPermissions(req); err != nil {
		return f, err
	}
	if f.ActivitySlugs, err = optionalSlugs(req, "activity_slugs"); err != nil {
		return f, err
	}
	if f.WidgetSlugs, err = optionalSlugs(req, "widget_slugs"); err != nil {
		return f, err
	}
	f.ExpiresAt, err = optionalExpiry(req)
	return f, err
}

// optionalPermissions returns nil when permissions is omitted and checks every
// level against its resource's enum before it reaches the server. An empty
// object is refused: on create it would silently mean "no access at all".
func optionalPermissions(req mcp.CallToolRequest) (*client.IntegrationKeyPermissions, error) {
	v, ok := req.GetArguments()["permissions"]
	if !ok {
		return nil, nil
	}
	m, isObject := v.(map[string]any)
	if !isObject {
		return nil, fmt.Errorf("permissions must be an object like {\"notifications\": \"send\"}")
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("permissions is empty: name at least one of activities, notifications, widgets or emails")
	}
	p := &client.IntegrationKeyPermissions{}
	targets := map[string]**string{"activities": &p.Activities, "notifications": &p.Notifications, "widgets": &p.Widgets, "emails": &p.Emails}
	for _, r := range integrationKeyLevels {
		raw, set := m[r.resource]
		if !set {
			continue
		}
		level, isString := raw.(string)
		if !isString || !slices.Contains(r.levels, level) {
			return nil, fmt.Errorf("permissions.%s must be one of %v, got %v", r.resource, r.levels, raw)
		}
		*targets[r.resource] = &level
	}
	for key := range m {
		if _, known := targets[key]; !known {
			return nil, fmt.Errorf("permissions.%s is not a resource: use activities, notifications, widgets or emails", key)
		}
	}
	return p, nil
}

// optionalSlugs returns nil when the list is omitted and a pointer to the
// list (possibly empty) otherwise. It is strict on purpose: a lenient parse
// that dropped non-string items would turn [123] into [], which the server
// reads as "no restriction".
func optionalSlugs(req mcp.CallToolRequest, name string) (*[]string, error) {
	v, ok := req.GetArguments()[name]
	if !ok {
		return nil, nil
	}
	if v == nil {
		return nil, fmt.Errorf("%s cannot be null: omit it, or pass [] to remove the restriction", name)
	}
	slugs, err := req.RequireStringSlice(name)
	if err != nil {
		return nil, err
	}
	if slugs == nil {
		slugs = []string{}
	}
	return &slugs, nil
}

// optionalExpiry returns the expires_at to send: nil when omitted, JSON null
// for "never", or the RFC 3339 time, which must be in the future.
func optionalExpiry(req mcp.CallToolRequest) (json.RawMessage, error) {
	v, ok := req.GetArguments()["expires_at"]
	if !ok {
		return nil, nil
	}
	s, isString := v.(string)
	if !isString {
		return nil, fmt.Errorf(`expires_at must be an RFC 3339 time or "never"`)
	}
	if s == "never" {
		return json.RawMessage("null"), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf(`expires_at must be an RFC 3339 time like 2026-12-31T00:00:00Z, or "never"; got %q`, s)
	}
	if !t.After(time.Now()) {
		return nil, fmt.Errorf("expires_at must be in the future")
	}
	return json.Marshal(t.UTC().Format(time.RFC3339))
}
