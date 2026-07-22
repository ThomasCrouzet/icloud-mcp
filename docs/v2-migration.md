# V2 migration guide

## Compatibility matrix

| Area | Previous | V2 | Compatible | Action |
|------|----------|----|------------|--------|
| `list_calendars` | unchanged | unchanged | yes | none |
| `search_events` | query, limit, offset, calendar | + calendars, uid, status, all_day, compact, busy_only, include_cancelled | yes | optional new params |
| `create_event` | title/start/end/calendar/... | + status, transparency, url, client_uid, idempotency_key | yes | optional |
| `update_event` | field patch | + scope, recurrence_id, etag, status, transparency, url | yes | use etag when concurrent edits |
| `delete_event` | uid+calendar | + scope, recurrence_id, etag, dry_run | yes | use dry_run to preview |
| New tools | n/a | get_event, find_free_slots, validate_event, calendar_capabilities | additive | call as needed |
| Read-only | 2 tools | 6 tools (mutations still hidden) | yes | none |
| Errors | text / some codes | structured code + retryable | yes | prefer matching `code` |
| DEFAULT_TZ | UTC if unset | UTC if unset (unchanged) | yes | set `Europe/Paris` if desired |
| Update concurrency | opportunistic If-Match | same + delete If-Match + get etag | yes | re-read on `concurrent_modification` |

## Breaking changes

None intentional for the five original tools. New optional parameters only.

## Recommended agent flow

1. `calendar_capabilities` once per session.
2. `list_calendars` for paths.
3. `search_events` or `get_event` for reads.
4. `validate_event` before risky writes.
5. `create_event` / `update_event` / `delete_event` with human confirmation for deletes.
6. On `concurrent_modification`, re-`get_event` and retry with fresh `etag`.

## Not shipped

- `this-and-future` recurrence scope (not proven correct against iCloud).
- Attendees / invitations (must not contact third parties).
