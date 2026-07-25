# MCP error codes and retry semantics

Every tool error is a JSON object in the MCP error text channel:

```json
{
  "code": "rate_limited",
  "message": "op: iCloud is rate limiting requests...",
  "retryable": true,
  "retry_after_seconds": 5
}
```

Field names are stable: `code`, `message`, `retryable`, `retry_after_seconds`,
`reconciliation`, and optional `details`. Calendar, Contacts, and Mail share the
same public code vocabulary.

## Codes and agent policy

| Code | Retryable | Guidance |
|------|-----------|----------|
| `validation` | no | Fix arguments; do not retry unchanged. |
| `authentication` | no | Refresh app-specific password; do not retry. |
| `authorization` | no | Permission or quota; do not retry blindly. |
| `not_found` | no | Resource gone; re-list if needed. |
| `conflict` | no | Create UID exists or state conflict; choose a new key or abort. |
| `concurrent_modification` | no | Re-read with `get_*`, then patch with fresh `etag`. |
| `rate_limited` | yes | Wait `retry_after_seconds` (default 5) then retry. |
| `timeout` | no (tool deadline) | Tool middleware deadline is non-retryable. After cancel it waits a short grace for a real result. Mutation tools include `reconciliation` because a late server apply is still possible. Prefer re-read; use `client_uid` / `idempotency_key` / `etag`. |
| `unavailable` | yes | Back off with `retry_after_seconds` (default 2). |
| `partial_failure` | no | Inspect warnings; do not assume full success. |
| `protocol_error` | no | Library/server protocol gap (e.g. CONDSTORE flags). |
| `payload_too_large` | no | Narrow the query, range, or calendar selection (includes multi-calendar search above the 10,000-event materialization budget). |
| `outcome_unknown` | no | Mutation may have applied. Follow `reconciliation`; use `client_uid` / `idempotency_key` if present. |
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

Agent: sleep `retry_after_seconds`, then retry the same read.

### Authentication

```json
{
  "code": "authentication",
  "message": "getting event: authentication: iCloud authentication refused..."
}
```

Agent: stop. Ask the operator to rotate the app-specific password.

### Concurrent modification

```json
{
  "code": "concurrent_modification",
  "message": "updating event: concurrent_modification: the event was modified..."
}
```

Agent: call `get_event`, merge intent, retry update with the new `etag`.

### Outcome unknown

```json
{
  "code": "outcome_unknown",
  "message": "creating event: outcome_unknown: the Calendar mutation outcome is unknown",
  "reconciliation": "Re-read the target event before retrying; do not repeat the mutation blindly."
}
```

Agent: if `client_uid` / `idempotency_key` was supplied, re-submit with the same
key (create returns conflict if already present; update returns the cached
success when the process-local cache still holds the entry). Update
`idempotency_key` is **process-local**, in-memory, **15 minute TTL**, and does
not survive restart or another process. Otherwise re-read by UID before
deciding.

## Internal retries

The server already retries **safe Calendar and Contacts reads** on HTTP 429/502/
503/504 with bounded backoff and `Retry-After`. Mutations and Mail send are
never auto-replayed. Agents should still honor `retryable` and
`retry_after_seconds` on the final MCP error after the server exhausted its own
budget.
