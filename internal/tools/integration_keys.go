package tools

import (
	"context"
	"fmt"
	"slices"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// Integration key management. Only the account's default key may call these
// endpoints; the server answers every other key with
// integration_key.default_key_required, so the tools are registered in every
// mode and the server stays the one place that decides.
//
// Hand-written rather than generated (see skipOperations): activity_slugs is a
// string array the generator drops, roll is a POST without a body, and omitted
// vs [] activity_slugs on update mean different things.

var integrationKeyScopes = []string{"activity:update", "activity:manage"}

func registerIntegrationKeyTools(s *mcpserver.MCPServer, api *client.APIClient) {
	s.AddTool(
		mcp.NewTool("create_integration_key",
			mcp.WithDescription("Create an integration key (hlk_...) for this account. The secret comes back once, in `key`, and cannot be read again, so hand it straight to whatever needs it. Only the account's default key can do this (other keys get integration_key.default_key_required), and the new key cannot have a higher scope or a capability the default key lacks (integration_key.grant_exceeded). The new key does not depend on the default key: it keeps working if the default key is revoked or rolled, so revoke it when it is no longer needed."),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("name", mcp.Required(), mcp.Description("Name shown in the app, e.g. the integration it is for (1-256 characters)")),
			mcp.WithString("scope", mcp.Enum(integrationKeyScopes...), mcp.Description("activity:update (default) can list, read and update activities; activity:manage can also create and delete them")),
			mcp.WithArray("activity_slugs", mcp.WithStringItems(), mcp.Description("Restrict the key to these activity slugs or trailing-* patterns, e.g. [\"ci-*\"]. Omit for all activities; an empty list is refused, since the server would read it as all activities too.")),
			mcp.WithBoolean("notifications", mcp.Description("Allow sending notifications (default false)")),
			mcp.WithBoolean("widgets", mcp.Description("Allow managing widgets (default false)")),
			mcp.WithBoolean("emails", mcp.Description("Allow sending transactional emails (default false)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCreateIntegrationKey(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("list_integration_keys",
			mcp.WithDescription("List this account's active integration keys, newest first, without their secrets: id, name, scope, notifications/widgets/emails flags, activity_slugs, created_at, last_used_at. The one with is_default=true is the default key. Only the account's default key can do this."),
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
			mcp.WithDescription("Change an integration key's scope, activity_slugs restriction or notifications/widgets/emails flags. Omitted fields stay as they are; activity_slugs=[] removes the restriction. Pass at least one field. A default key cannot be changed here (integration_key.default_target), and the result cannot have a higher scope or a capability the default key lacks (integration_key.grant_exceeded)."),
			// Changing a live credential's permissions can break the integration
			// using it, so this is not an additive update.
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("key_id", mcp.Required(), mcp.Description("Key id (UUID) from list_integration_keys")),
			mcp.WithString("scope", mcp.Enum(integrationKeyScopes...), mcp.Description("New scope")),
			mcp.WithArray("activity_slugs", mcp.WithStringItems(), mcp.Description("New slug restriction (slugs or trailing-* patterns); [] removes it")),
			mcp.WithBoolean("notifications", mcp.Description("Allow sending notifications")),
			mcp.WithBoolean("widgets", mcp.Description("Allow managing widgets")),
			mcp.WithBoolean("emails", mcp.Description("Allow sending transactional emails")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleUpdateIntegrationKey(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("roll_integration_key",
			mcp.WithDescription("Replace an integration key's secret and return the new one once, in `key`. The old secret stops working at once, so whatever still uses it fails until it gets the new one. A default key cannot be rolled here (integration_key.default_target), nor a key with a higher scope or a capability the default key lacks (integration_key.grant_exceeded)."),
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
	// likely meant "no activities", which the API cannot express.
	if fields.ActivitySlugs != nil && len(*fields.ActivitySlugs) == 0 {
		return mcp.NewToolResultError("activity_slugs=[] would give the key every activity; omit activity_slugs for that, or list the slugs it may use"), nil
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
	if fields == (client.IntegrationKeyFields{}) {
		return mcp.NewToolResultError("nothing to update: pass scope, activity_slugs, notifications, widgets or emails"), nil
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

// integrationKeyFields reads the fields create and update share.
func integrationKeyFields(req mcp.CallToolRequest) (f client.IntegrationKeyFields, err error) {
	if f.Scope, err = optionalScope(req); err != nil {
		return f, err
	}
	if f.ActivitySlugs, err = optionalSlugs(req); err != nil {
		return f, err
	}
	f.Notifications, f.Widgets, f.Emails, err = optionalCapabilities(req)
	return f, err
}

// optionalScope returns nil when scope is omitted and rejects a null or a value
// outside the enum before it reaches the server.
func optionalScope(req mcp.CallToolRequest) (*string, error) {
	v, ok := req.GetArguments()["scope"]
	if !ok {
		return nil, nil
	}
	s, isString := v.(string)
	if !isString {
		return nil, fmt.Errorf("scope must be one of %v", integrationKeyScopes)
	}
	if slices.Contains(integrationKeyScopes, s) {
		return &s, nil
	}
	return nil, fmt.Errorf("scope must be one of %v, got %q", integrationKeyScopes, s)
}

// optionalSlugs returns nil when activity_slugs is omitted and a pointer to
// the list (possibly empty) otherwise. It is strict on purpose: a lenient
// parse that dropped non-string items would turn [123] into [], which the
// server reads as "no restriction".
func optionalSlugs(req mcp.CallToolRequest) (*[]string, error) {
	v, ok := req.GetArguments()["activity_slugs"]
	if !ok {
		return nil, nil
	}
	if v == nil {
		return nil, fmt.Errorf("activity_slugs cannot be null: omit it, or pass [] to remove the restriction")
	}
	slugs, err := req.RequireStringSlice("activity_slugs")
	if err != nil {
		return nil, err
	}
	if slugs == nil {
		slugs = []string{}
	}
	return &slugs, nil
}

// optionalCapabilities reads the three capability flags, nil when omitted.
// Only JSON booleans are accepted, so a stray 1 or "yes" cannot switch one on.
func optionalCapabilities(req mcp.CallToolRequest) (notifications, widgets, emails *bool, err error) {
	read := func(name string) (*bool, error) {
		v, ok := req.GetArguments()[name]
		if !ok {
			return nil, nil
		}
		b, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf("%s must be true or false", name)
		}
		return &b, nil
	}
	if notifications, err = read("notifications"); err != nil {
		return nil, nil, nil, err
	}
	if widgets, err = read("widgets"); err != nil {
		return nil, nil, nil, err
	}
	if emails, err = read("emails"); err != nil {
		return nil, nil, nil, err
	}
	return notifications, widgets, emails, nil
}
