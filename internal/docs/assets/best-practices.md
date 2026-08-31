# PushWard Integration Best Practices

Rules for writing PushWard integrations that hold up in production. This is the
short list of things that actually bite bridges, not a feature tour. Pair it with
the live reference: call `get_pushward_docs(kind="index")` first to see what
exists, then `kind="full"` (with `section`) for guides, or `kind="api_openapi"` /
`kind="relay_openapi"` for exact request/response schemas.

The H2 headings below are the `topic` values accepted by
`get_pushward_best_practices`. Request one to get just that section.

## integration

General rules for any code that talks to the PushWard REST API
(`https://api.pushward.app`).

- **Authentication.** Send `Authorization: Bearer <key>`. User integration keys
  are prefixed `hlk_`. Never hard-code a key; read it from config/env (the
  bridges use the `PUSHWARD_*` env prefix, where env always overrides file
  config).
- **Idempotency via slugs.** An activity is identified by a unique `slug`.
  `POST /activities` creates it; `PATCH /activities/{slug}` updates it. Make the
  slug deterministic from the source event (e.g. `grafana-<fingerprint>`,
  `sonarr-<series>-<episode>`) so retries and re-deliveries converge on the same
  activity instead of creating duplicates.
- **Respect rate limits and 429s.** The API returns RFC 9457 problem responses;
  on `429` it includes `retry_after_ms`. Back off and retry with jitter rather
  than hammering. The relay enforces both per-IP and per-key limits, so bound
  your retries (the reference bridges retry about 5 times with exponential
  backoff).
- **Metadata caps.** Activity/notification metadata is capped at **20 key/value
  pairs**, each value **512 characters max**. Truncate or summarize before
  sending; oversized payloads are rejected.
- **Lifecycle & cleanup.** Set a sensible `priority` (higher wins when the device
  hits Apple's concurrent-activity ceiling). Let the server clean up ended
  activities via `ended_ttl` (the bridges expose this as `cleanup_delay`) and
  stale ongoing ones via `stale_timeout`. Don't rely on your process staying
  alive to delete them.
- **Send only what changed.** `PATCH` is a merge-patch, so include the fields you
  are updating plus the `content.template`. Avoid resending unchanged large
  blobs on every tick.

## live-activity

Writing Live Activity content that renders well on the Dynamic Island and Lock
Screen.

- **Pick the right template.** Ten templates, each with a distinct layout:
  `generic` (progress for builds/downloads/deploys), `countdown` (server-managed
  timer with automatic warning/completion pushes), `steps` (CI/CD multi-stage
  matrix), `alert` (severity-based monitoring with deep links), `gauge`
  (numeric value within min/max, progress auto-computed server-side), `timeline`
  (real-time sparkline; each push appends a data point and the server keeps the
  history), `board` (a grid of 1-4 labeled status tiles such as room sensors or
  service health, `tiles` replaced wholesale per update), `log` (a scrolling feed
  of 1-20 newest-first `lines`, replaced wholesale per update; the server also
  keeps a rolling backlog readable via `GET /activities/{slug}?include=log_backlog`),
  `media` (a remote player card: cover art, `media_title` over `subtitle`, a
  scrubber that ticks on device while playing, and transport buttons that fire
  your webhooks), `approval` (a question card with 2-4 answer buttons; the
  server can record the tapped option itself and end the card). Always set
  `content.template`.
- **Images are fetched by the device, not the server.** `content.image_url` is
  accepted on `generic`, `steps` and `media` only (anything else is a `422`), and the
  phone downloads it itself - a LAN or Tailscale host renders nothing at all.
  Send `image_thumbhash` alongside it: the blurred ThumbHash is what shows until
  the download lands, and the only thing that shows when it never does.
  `image_shape` picks the frame (`poster`, `square`, `circle`). Every viewer's
  device requests the URL itself and activities can be shared, so host the image
  somewhere you are happy to expose to whoever holds the link.
- **Two-phase end.** To end an activity with a clean final frame: first `PATCH`
  to `state="ongoing"` with the *final* content (so the last visible frame is
  correct), pause briefly so the user sees it, then `PATCH` to `state="ended"` to
  dismiss. Ending in one step can flash a stale frame before dismissal. The MCP
  `end_activity` tool follows this pattern and preserves the existing template,
  updating only the state text. Mirror it.
- **State text.** On end, set a short human reason as the state text
  (e.g. "Completed", "Failed", "Cancelled") rather than leaving the last
  in-progress label.
- **`tap_action` routing.** Set `content.tap_action` to make the activity
  tappable: a foreground HTTPS URL opens in-app, a custom scheme routes
  cross-app, and an HTTP URL with method/headers/body fires a silent webhook.
- **Countdown specifics.** For `countdown`, set it and forget it: the server
  drives the warning and completion pushes from the target time. Use
  `snooze_seconds` to extend rather than recreating the activity.
- **Media specifics.** The device ticks the scrubber itself from
  `position_seconds` and `position_at`, so re-send `position_seconds` on every
  play/pause/seek transition (leave `position_at` out; it defaults to the
  server's receive time) and on a slow timer while playing - a position-only
  patch is a low-priority, coalescable push. `controls` are silent webhooks:
  an http(s) URL is `POST` by default and `foreground` is rejected, a custom
  scheme opens that app. Prefer the `play_pause` toggle unless the player has
  separate play and pause endpoints. Keep control headers and bodies small:
  when the payload runs over the APNs budget the server drops whole optional
  buttons, never their headers, in the order `extra` -> volume -> `favorite`
  -> `stop` -> image -> `previous`/`next`. iOS builds older than 1.9.0 render
  the card as `generic` with a static progress bar and no buttons.
- **Approval specifics.** The question rides `state`; keep it short and
  interrogative. Prefer url-less options: the server signs an answer URL into
  each one, writes the first tap to the read-only `answer` field, pushes that
  to every device and ends the activity a few seconds later (`dismissal_ttl`
  decides how long the answered card lingers). Read the outcome with the
  `wait_for_answer` tool, or poll `get_activity`, rather than hosting a webhook
  of your own. An option carrying your own `url` needs a stable endpoint and
  records no `answer`. Set `end_date` with `on_expire` so an ignored question
  resolves itself; two options render as labeled buttons, three or four as icon
  tiles (icons required there). Older iOS builds render the card as `generic`
  with the first two options as working buttons.

## relay-provider

Wiring an external service's webhook to PushWard through the relay
(`https://relay.pushward.app`) instead of running your own bridge.

- **Prefer the relay for webhook-style sources.** Point the service's webhook at
  the relay's provider endpoint (e.g. `POST /grafana`, `/sonarr`, `/proxmox`).
  The relay is multi-tenant: it extracts the caller's `hlk_` key from the
  `Authorization` header, so no per-user container or config is needed.
- **Check the exact payload shape.** Each provider has its own request schema, so
  pull `get_pushward_docs(kind="relay_openapi")` and read the provider's
  `*Payload` schema before constructing test or production payloads. Some
  providers accept flat top-level fields, others nest under an object.
- **Dedup and grouping are stateful.** The relay persists per-event state (in
  PostgreSQL) to deduplicate repeated webhooks and to group related events (e.g.
  alert fingerprints, media by TMDB/TVDB id) into a single activity. Send a
  stable identifier in the payload so grouping works; don't generate a fresh id
  per delivery.
- **Per-tenant isolation.** The relay pools an API client per tenant key, so keep
  one integration key per logical source and activities and limits stay scoped
  correctly.
- **Test before shipping.** Use the MCP `test_relay_provider` tool (or
  `relay_<provider>`) to send a representative payload and confirm the response
  before pointing real traffic at it.

## email

Sending transactional email through `POST /emails`.

- **Recipients must be verified.** Email only delivers to an address that is
  registered **and** double-opt-in verified on the account, and not
  unsubscribed. Registering/verifying recipients is an account-owner (`hla_`) /
  dashboard operation; it is **not** reachable with an `hlk_` integration key,
  so a bridge (or this MCP) can send to an existing verified recipient but
  cannot add one.
- **Provide a body.** `subject` and `to` are required; send `html_body`,
  `text_body`, or both.
- **Read the send outcome, don't trust the HTTP status.** The response is an
  email-log record: `status` is `sent` / `bounced` / `complained` / `failed` /
  `suppressed`, and `delivery` is `all` or `none`. When `delivery` is `none`,
  `reason` explains it (`suppressed` = recipient not verified or unsubscribed;
  `send_failed` = provider error). A `2xx` does not guarantee delivery, so always
  inspect `status`.
- **Test it.** Use the MCP `test_email` tool (or `send_email`) with a known
  verified recipient and confirm a `sent` status before pointing real traffic
  at it.

## references

- PushWard docs index: <https://pushward.app/llms.txt> (and the full bundle
  <https://pushward.app/llms-full.txt>), also available offline via
  `get_pushward_docs`.
- API explorer: <https://pushward.app/api>; OpenAPI: <https://api.pushward.app/openapi.yaml>.
- Limits and examples: <https://pushward.app/docs/limits>, <https://pushward.app/docs/examples>.
- Apple ActivityKit (Live Activities): <https://developer.apple.com/documentation/activitykit>.
- APNs sending best practices (collapse ids, priority, expiry):
  <https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns>.
- The `llms.txt` convention (fetch the index, then pull only the pages you need):
  <https://llmstxt.org>.
