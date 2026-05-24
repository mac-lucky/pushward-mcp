# PushWard Integration Best Practices

A curated guide for writing correct, production-grade code against PushWard.
Pair it with the live reference: call `get_pushward_docs(kind="index")` first to
see what exists, then `kind="full"` (with `section`) for guides, or
`kind="api_openapi"` / `kind="relay_openapi"` for exact request/response schemas.

The H2 headings below are the `topic` values accepted by
`get_pushward_best_practices` — request one to get just that section.

## integration

General rules for any code that talks to the PushWard REST API
(`https://api.pushward.app`).

- **Authentication.** Send `Authorization: Bearer <key>`. User integration keys
  are prefixed `hlk_`. Never hard-code a key — read it from config/env (the
  bridges use the `PUSHWARD_*` env prefix, where env always overrides file
  config).
- **Idempotency via slugs.** An activity is identified by a unique `slug`.
  `POST /activities` creates it; `PATCH /activities/{slug}` updates it. Make the
  slug deterministic from the source event (e.g. `grafana-<fingerprint>`,
  `sonarr-<series>-<episode>`) so retries and re-deliveries converge on the same
  activity instead of creating duplicates.
- **Respect rate limits and 429s.** The API returns RFC 9457 problem responses;
  on `429` it includes `retry_after_ms`. Back off and retry with jitter rather
  than hammering — the relay enforces both per-IP and per-key limits. Bound your
  retries (the reference bridges retry ~5× with exponential backoff).
- **Metadata caps.** Activity/notification metadata is capped at **20 key/value
  pairs**, each value **≤512 characters**. Truncate or summarize before sending;
  oversized payloads are rejected.
- **Lifecycle & cleanup.** Set a sensible `priority` (higher wins when the device
  hits Apple's concurrent-activity ceiling). Let the server clean up ended
  activities via `ended_ttl` (the bridges expose this as `cleanup_delay`) and
  stale ongoing ones via `stale_timeout` — don't rely on your process staying
  alive to delete them.
- **Send only what changed.** `PATCH` is a merge-patch — include the fields you
  are updating plus the `content.template`. Avoid resending unchanged large
  blobs on every tick.

## live-activity

Writing Live Activity content that renders well on the Dynamic Island and Lock
Screen.

- **Pick the right template.** Six templates, each with a distinct layout:
  `generic` (progress for builds/downloads/deploys), `countdown` (server-managed
  timer with automatic warning/completion pushes), `steps` (CI/CD multi-stage
  matrix), `alert` (severity-based monitoring with deep links), `gauge`
  (numeric value within min/max, progress auto-computed server-side), `timeline`
  (real-time sparkline; each push appends a data point and the server keeps the
  history). Always set `content.template`.
- **Two-phase end.** To end an activity with a clean final frame: first `PATCH`
  to `state="ongoing"` with the *final* content (so the last visible frame is
  correct), pause briefly so the user sees it, then `PATCH` to `state="ended"` to
  dismiss. Ending in one step can flash a stale frame before dismissal. The MCP
  `end_activity` tool follows this pattern and preserves the existing template,
  updating only the state text — mirror it.
- **State text.** On end, set a short human reason as the state text
  (e.g. "Completed", "Failed", "Cancelled") rather than leaving the last
  in-progress label.
- **`tap_action` routing.** Set `content.tap_action` to make the activity
  tappable: a foreground HTTPS URL opens in-app, a custom scheme routes
  cross-app, and an HTTP URL with method/headers/body fires a silent webhook.
- **Countdown specifics.** For `countdown`, set it and forget it — the server
  drives the warning and completion pushes from the target time. Use
  `snooze_seconds` to extend rather than recreating the activity.

## relay-provider

Wiring an external service's webhook to PushWard through the relay
(`https://relay.pushward.app`) instead of running your own bridge.

- **Prefer the relay for webhook-style sources.** Point the service's webhook at
  the relay's provider endpoint (e.g. `POST /grafana`, `/sonarr`, `/proxmox`).
  The relay is multi-tenant: it extracts the caller's `hlk_` key from the
  `Authorization` header, so no per-user container or config is needed.
- **Check the exact payload shape.** Each provider has its own request schema —
  pull `get_pushward_docs(kind="relay_openapi")` and read the provider's
  `*Payload` schema before constructing test or production payloads. Some
  providers accept flat top-level fields, others nest under an object.
- **Dedup and grouping are stateful.** The relay persists per-event state (in
  PostgreSQL) to deduplicate repeated webhooks and to group related events (e.g.
  alert fingerprints, media by TMDB/TVDB id) into a single activity. Send a
  stable identifier in the payload so grouping works; don't generate a fresh id
  per delivery.
- **Per-tenant isolation.** The relay pools an API client per tenant key — keep
  one integration key per logical source so activities and limits are scoped
  correctly.
- **Test before shipping.** Use the MCP `test_relay_provider` tool (or
  `relay_<provider>`) to send a representative payload and confirm the response
  before pointing real traffic at it.

## references

- PushWard docs index: <https://pushward.app/llms.txt> (and the full bundle
  <https://pushward.app/llms-full.txt>) — also available offline via
  `get_pushward_docs`.
- API explorer: <https://pushward.app/api>; OpenAPI: <https://api.pushward.app/openapi.yaml>.
- Limits and examples: <https://pushward.app/docs/limits>, <https://pushward.app/docs/examples>.
- Apple ActivityKit (Live Activities): <https://developer.apple.com/documentation/activitykit>.
- APNs sending best practices (collapse ids, priority, expiry):
  <https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns>.
- The `llms.txt` convention (fetch the index, then pull only the pages you need):
  <https://llmstxt.org>.
