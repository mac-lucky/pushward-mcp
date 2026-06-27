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
