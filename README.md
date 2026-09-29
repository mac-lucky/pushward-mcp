# pushward-mcp

An MCP server that lets a coding agent drive [PushWard](https://pushward.app) directly:
start and update Live Activities, send push notifications, manage widgets, and replay
external-service webhooks, all without a device or a pile of hand-written `curl`. It is the
fastest way to exercise the push API while you build an integration.

The tools wrap two HTTP surfaces:

- `api.pushward.app` for activities, notifications, and widgets (the REST API).
- `relay.pushward.app` for simulating webhooks from services like Grafana, Sonarr, Proxmox,
  and a dozen others (`relay_<provider>` tools).

On top of those there are a few composite `test_` tools for common flows (a full activity
lifecycle, a notification round-trip, a health check) and `get_pushward_docs` /
`get_pushward_best_practices`, which load the API reference and integration notes into the
agent's context.

## Tools

| Area | Tools |
|---|---|
| Live Activities | `create_activity`, `update_activity`, `end_activity`, `get_activity`, `list_activities`, `delete_activity`, `bulk_end_activities` |
| Notifications | `create_notification`, `create_scheduled_notification` (one-off `send_at` up to 365 days ahead, or a cron `recurrence`), `list_scheduled_notifications`, `get_scheduled_notification`, `cancel_scheduled_notification` (`purge` deletes instead of leaving a 24-hour canceled record) |
| Answers | `get_notification_answer` (can hold the request until the answer arrives), `wait_for_answer` (longer waits, for a notification or an approval Live Activity) |
| Widgets | `create_widget`, `update_widget`, `get_widget`, `list_widgets`, `delete_widget` |
| Account and health | `get_me`, `get_health`, `get_ready` |
| Email | `send_email`, `test_email` |
| Composite tests | `test_activity_lifecycle`, `test_notification`, `test_health`, `test_relay_provider` |
| Docs | `get_pushward_docs`, `get_pushward_best_practices` |
| Relay (stdio only) | `relay_<provider>` for ArgoCD, Backrest, Bazarr, Changedetection.io, Forgejo, Gatus, Gitea, Grafana, Jellyfin, Komodo, Overseerr, Paperless-ngx, Prowlarr, Proxmox VE, Radarr, Sonarr, Unmanic and Uptime Kuma |

## Using the hosted server

The easiest path is the hosted remote endpoint. Point an OAuth-capable MCP client at:

```
https://mcp.pushward.app/mcp
```

You authenticate on first connect: the consent screen asks for your PushWard integration key
(`hlk_...`) once, stores it encrypted server-side, and the MCP client only ever holds a
short-lived token. The key never reaches the client. The hosted endpoint exposes the API
tools only, not the relay tools (a multi-tenant endpoint can't share one relay credential).

## Running it locally (stdio)

For local development the server talks to a single client over stdio with one identity from
the environment:

```bash
go build -o pushward-mcp .
PUSHWARD_API_TOKEN=hlk_your_key ./pushward-mcp
```

Then register it in your client's MCP config:

```json
{
  "mcpServers": {
    "pushward": {
      "command": "/path/to/pushward-mcp",
      "env": { "PUSHWARD_API_TOKEN": "hlk_your_key" }
    }
  }
}
```

Environment variables (stdio mode):

- `PUSHWARD_API_TOKEN` - required, your `hlk_` integration key.
- `PUSHWARD_RELAY_TOKEN` - required only when relay tools are enabled (they are, by default,
  in stdio mode).
- `PUSHWARD_API_URL` / `PUSHWARD_RELAY_URL` - default to the production hosts; override to
  point at a staging server. Non-loopback hosts must be https.

The `http` transport (OAuth, multi-tenant) is what backs the hosted endpoint above; it needs
a signing key and a few more variables and is meant to run behind a proxy. See
`internal/config` and `internal/oauth` if you want to host your own.

## Single-user http (for a headless agent)

An agent that runs as a service can't click through an OAuth consent screen. For that case
the `http` transport can skip caller auth and act as one key:

```bash
PUSHWARD_MCP_TRANSPORT=http \
PUSHWARD_MCP_HTTP_AUTH=none \
PUSHWARD_API_TOKEN=hlk_your_key \
./pushward-mcp
```

It serves `/mcp`, `/health` and `/ready` on `127.0.0.1:8080`. Every request acts as
`PUSHWARD_API_TOKEN`, and any `Authorization` header the caller sends is ignored. Nothing
checks who is calling, so only the agent should be able to reach the port. To serve it to
a container or pod, set `PUSHWARD_MCP_LISTEN_ADDR=:8080` and put a network policy in front,
never the open internet. Requests with an `Origin` header get a 403, so a web page in your
browser can't use the key. Relay tools stay off unless `PUSHWARD_MCP_RELAY_ENABLED=true`,
which also needs `PUSHWARD_RELAY_TOKEN`.

Give the agent a dedicated integration key with only what it needs, for example
notifications on and activity slugs limited to a prefix like `agent-*`.

## Generated code

`internal/tools/api_gen.go` and `internal/tools/relay_gen.go` are generated from the OpenAPI
specs by `cmd/generate`, so don't edit them by hand. Regenerate from the committed specs
without hitting the network:

```bash
PUSHWARD_USE_LOCAL_SPEC=1 go run ./cmd/generate
```

Drop `PUSHWARD_USE_LOCAL_SPEC` to refresh the specs and embedded docs from the live API first.

## License

MIT, see [LICENSE](LICENSE).
