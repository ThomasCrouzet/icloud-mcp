# MCP error codes and retry semantics

Each tool error is a JSON object in the MCP error text channel:

```json
{
  "code": "rate_limited",
  "message": "op: iCloud is rate limiting requests...",
  "retryable": true,
  "retry_after_seconds": 5
}
```

These field names are stable: `code`, `message`, `retryable`,
`retry_after_seconds`, `reconciliation`, and optional `details`. Calendar,
Contacts, and Mail use the same public codes.

## Codes and agent policy

| Code | Retryable | Guidance |
|------|-----------|----------|
| `validation` | no | Fix arguments; do not retry unchanged. |
| `authentication` | no | Refresh app-specific password; do not retry. |
| `authorization` | no | Check permission or quota. Do not retry without a change. |
| `not_found` | no | The resource is gone. List resources again if necessary. |
| `conflict` | no | The create UID exists, or the state conflicts. Select a new key or stop. |
| `concurrent_modification` | no | Re-read with `get_*`, then patch with fresh `etag`. |
| `rate_limited` | yes | Wait `retry_after_seconds` (default 5) then retry. |
| `timeout` | no (tool deadline) | The tool deadline is not retryable. After cancellation, the server waits briefly for a real result. A mutation can still finish late. Use `reconciliation`. Read the resource again. Use `client_uid`, `idempotency_key`, or `etag`. |
| `unavailable` | yes | Back off with `retry_after_seconds` (default 2). |
| `partial_failure` | no | Inspect warnings; do not assume full success. |
| `protocol_error` | no | The library or server has a protocol gap. CONDSTORE flags are one example. |
| `payload_too_large` | no | Narrow the query, range, or calendar selection. This includes more than 10,000 materialized events in a multi-calendar search. |
| `outcome_unknown` | no | The mutation might already be complete. Follow `reconciliation`. Use `client_uid` or `idempotency_key` if present. |
| `internal_error` | no | Bug or unexpected failure; report with redacted logs. |

## Examples

### Rate limit

```json
{
  "code": "rate_limited",
  "message": "listing calendars: read rate limit exceeded: retry later",
  "retryable": true,
  "retry_after_seconds": 5
}
```

Agent: wait for `retry_after_seconds`. Then, repeat the same read.

### Authentication

```json
{
  "code": "authentication",
  "message": "getting event: authentication: iCloud authentication refused..."
}
```

Agent: stop. Ask the operator to replace the app-specific password.

### Concurrent modification

```json
{
  "code": "concurrent_modification",
  "message": "updating event: concurrent_modification: the event was modified..."
}
```

Agent: call `get_event`. Apply the intended changes to that result. Then, repeat
the update with the new `etag`.

### Outcome unknown

```json
{
  "code": "outcome_unknown",
  "message": "creating event: outcome_unknown: the Calendar mutation outcome is unknown",
  "reconciliation": "Re-read the target event before retrying; do not repeat the mutation blindly."
}
```

Agent: if the request had `client_uid` or `idempotency_key`, submit it again with
the same key. Create returns conflict if the event is already present. Update
returns cached success while the process-local cache contains the entry.

An update `idempotency_key` stays **in memory** for **15 minutes** in one
process. It does not remain after a restart. Another process cannot use the
entry. Without a key, read the UID again before you decide.

## Internal retries

The server already retries **safe Calendar and Contacts reads** after HTTP 429,
502, 503, or 504. It uses bounded backoff and `Retry-After`. The server never
repeats mutations or Mail send automatically. After its retry limit, obey
`retryable` and `retry_after_seconds` in the final MCP error.
