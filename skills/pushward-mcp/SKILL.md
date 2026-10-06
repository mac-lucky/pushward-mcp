---
name: pushward-mcp
description: Reach the user's iPhone through the PushWard MCP server - push notifications, questions with buttons that wait for their tap, Live Activities on the Lock Screen and Dynamic Island, home screen widgets, scheduled notifications and email. Use when PushWard tools such as create_notification or create_activity are connected and the user says "notify me when done", "ask me before you deploy", "show progress on my lock screen" or "remind me at 6", when they want to connect PushWard to their agent, or when writing code that integrates with the PushWard API.
---

# PushWard MCP

The PushWard MCP server puts the PushWard API behind tools. Everything you send lands on the
user's own devices, so this skill is mostly about what is worth sending and how to clean up
after yourself.

## Connecting

The hosted server is `https://mcp.pushward.app/mcp` (OAuth; the user pastes their `hlk_`
integration key into the consent page once, and the client never sees it):

```sh
claude mcp add --scope user --transport http pushward https://mcp.pushward.app/mcp
```

Skip that if PushWard tools are already there; a second copy of the server doubles every tool.
Other clients: add a remote MCP server or custom connector with that URL. The Claude Code plugin
(`/plugin marketplace add mac-lucky/pushward-mcp`, then `/plugin install pushward@pushward`)
installs the server and this skill together. A local stdio build takes the key from
`PUSHWARD_API_TOKEN` instead (and needs `PUSHWARD_RELAY_TOKEN`, or
`PUSHWARD_MCP_RELAY_ENABLED=false`, to start). Never ask the user to paste a key into the chat.

If this skill is loaded but no PushWard tools are available, the server is added but not
authorized yet. In Claude Code the user runs `/mcp`, picks the pushward server and signs in
once; do not add another server to work around it.

Clients prefix tool names, for example `mcp__pushward__create_notification`, or
`mcp__plugin_pushward_pushward__create_notification` through the plugin. The names below are
the bare ones.

If the user wants the agent on a short leash, they can give it a key of its own in the app
with only the permissions it needs, limited to slugs starting with `agent-`. The examples
below use that prefix.

## Pick the surface

| The user wants | Use |
|---|---|
| to know when something finished or failed | `create_notification` |
| to be told right away that you are blocked | `create_notification` with `level: "time-sensitive"` |
| to approve, pick an option, or type a reply | `create_notification` with url-less `actions`, then `wait_for_answer`; or an approval Live Activity |
| an alert that keeps coming back until they react | `create_notification` with `acknowledge`, then `wait_for_ack` |
| to watch a long task move | `create_activity`, then `update_activity` a few times, then `end_activity` |
| a number to glance at later | `create_widget` once, then `update_widget` |
| a reminder later or on a repeat | `create_scheduled_notification` |
| a report to read at a desk | `send_email` (only to addresses verified in the app) |

One notification at the end beats five along the way. Do not notify about what the user is
watching you do right now; notify when they would otherwise have to come back and check.

## Notifications

```
create_notification {"title": "Tests pass on feat/login", "body": "412 passed in 3m10s"}
create_notification {"title": "Blocked: migration needs a decision", "body": "users.email has 31 duplicates", "level": "time-sensitive"}
```

Titles carry the news, bodies the one detail that matters. `level` is `passive`, `active`
(default), `time-sensitive` or `critical`; keep `time-sensitive` for "I am stuck until you
answer" and do not use `critical` unless asked. `collapse_id` replaces an earlier notification
with the same id instead of stacking a new one; `url` opens a page on tap.

## Asking and waiting

```
create_notification {"title": "Merge PR #42?", "body": "CI green, 3 files changed",
  "actions": [{"id": "merge", "title": "Merge"}, {"id": "hold", "title": "Not yet"},
              {"id": "reply", "title": "Reply", "text_input": true}]}
wait_for_answer {"notification_id": 1234, "timeout_seconds": 600}
```

Actions with no `url` (and no `foreground: true`) are recorded by the server; the create
response then says `answerable: true` and carries the `id` to wait on. Without `answerable`,
nothing will be recorded, so do not wait. The tap comes back as `answer.action_id`, plus
`answer.text` for a `text_input` action. `wait_for_answer` waits at most 600 seconds per call:
call it again to wait longer, and treat a final `answered: false` as "no decision", not as a
no. `get_notification_answer` reads the current state without blocking.

For a question that should sit on the Lock Screen until answered, use an approval card. Give
every question a new slug (add a timestamp): an approval that is started again with the same
buttons keeps the answer from last time.

```
create_activity {"slug": "agent-release-ask-1791150000", "name": "Release", "ended_ttl": 900}
update_activity {"slug": "agent-release-ask-1791150000", "state": "ongoing",
  "content_json": "{\"template\":\"approval\",\"state\":\"Ship 2.4 to production?\",\"source\":\"Agent\",\"options\":[{\"id\":\"ship\",\"title\":\"Ship\"},{\"id\":\"hold\",\"title\":\"Hold\"}]}"}
wait_for_answer {"slug": "agent-release-ask-1791150000", "timeout_seconds": 600}
```

The answer is `answer.option`. If you set a deadline with `end_date` and `on_expire`, check
`answer.by` too: `user` for a real tap, `expired` for the default. The server ends an answered approval card by itself shortly
after the tap. If you give up waiting, end the card yourself so a stale question does not sit
on the Lock Screen.

## Alerts that repeat until acknowledged

Sending `create_notification` with an `acknowledge` object (for example
`{"repeat_seconds": 300, "expire_seconds": 7200}`; every field is optional, but pass at least
one, since an empty object is dropped) makes the push come back every minute by default until
someone taps an action without a url, or until it expires after an hour. Without such an action
the server adds an Acknowledge button. Keep it for problems that must not be missed: a failed
backup, a production outage, a decision blocking a release. A finished task is not one. It
cannot be `passive` or sent with `push: false`.

The create response carries a `receipt`. Wait on it, or stop it once the problem clears:

```
wait_for_ack {"notification_id": 1234, "timeout_seconds": 600}
cancel_notification_receipt {"notification_id": 1234}
cancel_notification_receipts_by_tag {"tag": "nas-1"}
get_notification_receipt {"notification_id": 1234}
```

`wait_for_ack` returns `acknowledged: true` with the receipt (its `action_id` says which action
was tapped), or `acknowledged: false` with a reason when the alert expired, was canceled, or the
wait ran out while it kept repeating. Send it with `tags` to cancel a group by tag later. An
account can have 25 repeating at once, an organization's sends counting against the
organization; a new one with the same `collapse_id` replaces the older one's repeats. With an
integration key, the cancels and reads reach only what that key sent. A callback URL on the
send gets a signed POST when the alert is acknowledged or expires; that is for the user's own
webhook receivers, so only set one when asked.

## Progress on the Lock Screen

`create_activity` only registers the slug: nothing shows until an `update_activity` sets
content and `state: "ongoing"`.

```
create_activity {"slug": "agent-auth-refactor", "name": "Auth refactor", "stale_ttl": 7200, "ended_ttl": 1800}
update_activity {"slug": "agent-auth-refactor", "state": "ongoing",
  "content_json": "{\"template\":\"steps\",\"current_step\":1,\"total_steps\":4,\"step_labels\":[\"Plan\",\"Edit\",\"Test\",\"Commit\"],\"state\":\"Planning\"}"}
update_activity {"slug": "agent-auth-refactor", "content_json": "{\"current_step\":3,\"state\":\"Running 412 tests\"}"}
end_activity {"slug": "agent-auth-refactor",
  "content_json": "{\"current_step\":4,\"progress\":1,\"state\":\"Merged as 3f2c1a9\",\"icon\":\"checkmark.circle.fill\",\"accent_color\":\"green\"}"}
```

On failure:

```
end_activity {"slug": "agent-auth-refactor",
  "content_json": "{\"state\":\"Tests failed: 3 of 412\",\"icon\":\"xmark.circle.fill\",\"accent_color\":\"red\"}"}
```

`end_activity` merges `content_json` onto the stored content and ignores `reason` when
`content_json` is given, so put the final text in its `state`. `reason` alone only replaces the
status text.

- Make the slug from the task (`agent-<repo>-<task>`) so a retry updates the same card instead
  of starting a second one.
- Always end what you start, including when you fail or the user stops you. `stale_ttl` ends
  a forgotten one eventually; do not rely on it.
- Set `ended_ttl` on every `create_activity` (15 to 60 minutes is plenty). An account holds 50
  activities and an ended one counts for 30 days unless `ended_ttl` deletes it sooner; one card
  per task fills that up in a few weeks, and then every integration on the account gets `409`.
- `update_activity` is a merge patch: send only what changed, `null` clears a field, arrays are
  replaced whole. `content_json` is a JSON string, not an object.
- Update on milestones, not on every log line. Each update is a push and free accounts have a
  monthly allowance; `get_me` shows usage.
- `generic` with `progress` (0 to 1) suits one long job; `steps` suits a sequence. The
  description of the `content_json` argument of `update_activity` lists every template's fields, and
  `get_pushward_docs {"kind": "full", "section": "Gauge Template"}` has the details for one
  template.

## Widgets, schedules, email

```
create_widget {"slug": "agent-coverage", "name": "Coverage", "content_json": "{\"template\":\"progress\",\"value\":0.81,\"label\":\"api\"}"}
update_widget {"slug": "agent-coverage", "content_json": "{\"value\":0.84}"}
create_scheduled_notification {"title": "Check the canary", "body": "Error rate after the 14:00 deploy", "send_at": "<RFC 3339 time, in the future>"}
create_scheduled_notification {"title": "Standup", "body": "In 5 minutes", "recurrence": {"cron": "55 8 * * 1-5", "timezone": "Europe/Warsaw"}}
send_email {"to": "ops@example.com", "subject": "Nightly report", "text_body": "..."}
```

Scheduled notifications are held by the server, so nothing has to keep running. Repeats must be
at least 15 minutes apart and an account can have 25 pending; keep the returned `id` for
`cancel_scheduled_notification` (`purge: true` leaves no canceled record).

## Organization keys

With a key that belongs to a PushWard organization (a team account), everything goes to the
members' devices, filtered by the organization's routing rules. `create_notification`,
`create_scheduled_notification`, `create_activity` and `update_activity` take an optional
`target` that narrows it further, by group name, device tag name or member user id:

```
create_notification {"title": "db-1 disk at 95%", "body": "Paging on-call", "level": "time-sensitive", "target": {"groups": ["oncall"]}}
create_activity {"slug": "agent-deploy-api", "name": "Deploy api", "ended_ttl": 1800, "target": {"tags": ["wall"]}}
update_activity {"slug": "agent-deploy-api", "content_json": "{}", "target": {"groups": ["oncall", "sre"]}}
update_activity {"slug": "agent-deploy-api", "content_json": "{}", "clear_target": true}
```

On `update_activity` a new `target` replaces the stored one and `clear_target` sends the
activity back to everyone the rules allow; devices that lose it end it, devices that gain it
start it. A key the admins limited to some groups and tags must stay inside them (403
otherwise, and for `clear_target` too) and only sees activities sent inside them: anything else
answers 404, as if it did not exist. A personal key gets 422 for a target. The API cannot list
group or tag names or member user ids: they live in the organization's console, so ask the
user for them.

## Encrypted notifications

A local stdio or single-user server started with `PUSHWARD_E2E_KEY` encrypts the title,
subtitle, body and url of `create_notification` and `create_scheduled_notification` before they
leave the process; the hosted server never does. The response then has an `encrypted` envelope
and placeholder text, and only the user's devices holding the key can read the real text. Level,
actions, metadata, thread, source and target stay readable to the server and to Apple, so keep
secrets out of those. Encrypted text has room for about 2,200 bytes in total. An organization
key cannot send encrypted (`notification.encryption_unavailable`): tell the user, only they can
unset the key. Never ask for the key itself.

## Writing code against the API

When the task is building an integration rather than notifying the user, ground it first:
`get_pushward_docs {"kind": "index"}` for the map, then `kind: "full"` with a `section`, or
`kind: "api_openapi"` for exact schemas, and `get_pushward_best_practices` (topics
`integration`, `live-activity`, `relay-provider`, `email`). The `test_activity_lifecycle`,
`test_notification` and `test_health` tools exercise the real API in one call. A local stdio
server also has `relay_<provider>` tools and `relay_universal` for replaying webhooks through
the relay; the hosted server does not. For shell scripts and CI, point the user at the
`pushward` CLI instead of hand-written HTTP.

## Be careful with

- `create_integration_key`, `update_integration_key`, `roll_integration_key` and
  `revoke_integration_key` only on an explicit request. A key you create outlives this session
  and the MCP connection; a revoke or roll breaks whatever uses that key.
- `bulk_end_activities` previews by default; show the user the preview before calling it again
  with `confirm: true`.
- `delete_activity` and `delete_widget` remove things from the user's devices. End an activity
  instead unless they asked for a delete.
