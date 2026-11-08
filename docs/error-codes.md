# MCP error codes and retry semantics

Domain errors use a JSON object inside an MCP text content item.
The MCP result sets `isError` to `true`:

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
| `conflict` | no | The create UID exists or the resource state conflicts. Key conflicts and a full cache also use this code. See the recovery rules below. |
| `concurrent_modification` | no | Re-read with `get_*`, then patch with fresh `etag`. |
| `rate_limited` | yes | For reads, wait `retry_after_seconds` (default 5) before retry. For keyed updates, use the recovery rules below. |
| `timeout` | no (tool deadline) | A mutation can still finish after the caller times out. Read the resource again. A waiting duplicate does not release the original claim. |
| `unavailable` | yes | For reads, wait `retry_after_seconds` (default 2) before retry. For keyed updates, use the recovery rules below. |
| `partial_failure` | no | Inspect warnings; do not assume full success. |
| `protocol_error` | no | The library or server has a protocol gap. CONDSTORE flags are one example. |
| `payload_too_large` | no | Narrow read queries, ranges, or calendar selections. For a write result that exceeds the limit, read the resource before another update. |
| `outcome_unknown` | no | The mutation might already have succeeded. Follow `reconciliation`. Read the resource before choosing a new update key. |
| `internal_error` | no | Report the failure with redacted logs. If a mutation was possible, read the resource before another update. |

## Mail doctor diagnostics (unreleased)

Mail doctor adds optional `stage` and `reason` fields without changing public
error codes. These fields are not added to MCP tool errors or health output.
For example, `errorCode: unavailable`, `stage: tls`, and `reason: eof` identify
a TLS failure before IMAP authentication, not a rejected password.

Stages are `connect`, `tls`, or `authentication`. Reasons are fixed and validated
per stage. No raw network or protocol details are exposed. Unclassified causes
at observed connection or TLS boundaries use `unknown`; unobserved stages are omitted.
The fields describe the final failed attempt. They do not change retry policy.
See [the diagnostic pair table](mail-network-troubleshooting.md#unreleased-mail-failure-fields)
for supported values and safe next actions. Mail doctor remains authenticated,
read-only IMAP. It does not probe SMTP or provide a credential-free mode.

## Update idempotency and recovery

Only `update_event` and `update_contact` use the process-local result cache
through `idempotency_key`. In the same tool namespace, the same key and parameters
return the saved result, including its error flag. The domain applies redaction
and result limits again. A cached error remains an error.

Success and definitive domain errors stay for 15 minutes after the request
completes. Unknown outcomes, unclassified errors, and internal errors stay until
process exit. Response serialization or size errors after a successful write
also stay until process exit. Pending claims never expire. See the
[cache contract](architecture.md#update-idempotency) for entry and payload limits.

### Recover an ambiguous update

Use this procedure after `outcome_unknown`, a mutation timeout, or an error
after a possible write:

1. Keep the original key and update parameters.
2. Read the resource with `get_event` or `get_contact`.
3. Compare the current fields and ETag with the intended change.
4. If the desired state exists, stop.
5. If another update is necessary, use a new key with the fresh strong `etag`.

A new key starts a new operation. It does not prove that the previous write
failed. Process restart and cache expiry also give no such proof. A read does
not change a cached error into success.

While an entry exists, the same keyed call retrieves its result or waits for
the owner. It does not send another mutation. After expiry or restart, the same
call can send a new mutation. If a duplicate caller times out or cancels, the
original claim stays active.

### Resolve a key conflict

- **Different parameters:** read the resource before deciding on a new operation.
  A changed `etag` also counts as different parameters. Both update tools return
  `conflict`; a Contacts key conflict is not `validation_error`.
- **Full cache:** keep the key. Wait for capacity. The failed claim sends no
  mutation. Pending and uncertain entries do not expire to make space.
  Existing keys can still return their saved results.
- **Cached definitive error:** the same key returns that error during its
  15 minute lifetime. Retry metadata does not invalidate the cached result.
  Before a new operation, read the resource. Obey any `retry_after_seconds`
  interval.

Before restarting a process with pending or uncertain entries, reconcile the
affected resources. Restart removes these entries without resolving their
outcomes.

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
the update with the new `etag`. For a keyed update, select a new key after this
read because the parameters changed.

### Outcome unknown

```json
{
  "code": "outcome_unknown",
  "message": "creating event: outcome_unknown: the Calendar mutation outcome is unknown",
  "reconciliation": "Re-read the target event before retrying; do not repeat the mutation blindly."
}
```

Agent: call `get_event` with the create UID before another create attempt.
For create tools, `client_uid` and its `idempotency_key` alias identify the
resource. If that UID exists, another create returns `conflict`.

For an update, use the recovery procedure above. The same key can return a
cached `outcome_unknown`, even when a read shows that the write succeeded.

## Send archival and recovery

Synthetic tool timeouts for `send_message` and `reply_message` include SMTP-specific
reconciliation guidance. They are not evidence that submission failed. Never resend
automatically. Check Sent and confirm with recipients before deciding on another send.
An absent Sent copy does not prove failure, especially when archival was not requested.

For keyed calls, keep the same `idempotency_key` and unchanged parameters in the same
process during reconciliation. The send cache can share pending work or return a retained
outcome without another SMTP submission. Pending and `outcome_unknown` claims never
expire; they stay until process exit. Accepted and definitive rejected outcomes are
retained for 15 minutes after completion. A full cache returns `conflict` before SMTP
and gives reconciliation guidance without advising a restart or a different key as proof
of failure. However, `reply_message` first re-reads and validates its source, then derives
the send request before reaching that cache. Source deletion, changed UIDVALIDITY, or
changed derived fields can prevent retrieval of the earlier outcome or cause a conflict.
Such an error does not prove that the original SMTP submission failed. Repeated replies
are not a guaranteed status-query API. Keep the key and parameters, and reconcile before
deciding on another send.

After the 15-minute window or a process restart, an old key is a new operation. It can
submit again with identical or different content, and no conflict is guaranteed. Neither
expiry nor restart proves that the earlier operation failed. A send outside the window
needs reconciliation and a fresh sending decision, not automatic retry. Unkeyed calls
have no deduplication guarantee.

The tool deadline stays at 25 seconds with a two-second cancellation grace period.
A definitive handler result within that grace period takes precedence over a synthetic
timeout. Otherwise, the handler can still finish and retains its slot until it exits.
Do not treat a synthetic timeout as a definitive SMTP rejection or acceptance.

`send_message` validates `archive_sent` as a strict boolean, default false.
Disabled archival returns `validation`. A forbidden Sent scope returns `authorization`.
Missing, ambiguous, or invalid Sent discovery returns `protocol_error`.
These archival preflight failures occur before SMTP access and report `status: rejected`.

After SMTP acceptance, archival failure is not a rejected send or a tool error.
The response keeps `status: accepted` and reports `archivalStatus: failed` or `unknown`.
It also includes a bounded `archivalError`, not raw server text.
Inspect Sent before recovery. Never resend or blindly repeat APPEND to repair archival.

Accepted and definitive rejected submission and archival results share the same
15-minute completion-based retention. Pending, ambiguous, and unclassified state
stays pinned until process exit. Changing `archive_sent` with the same key returns
`conflict` while the key is retained, without another submission. Omission and false
have the same preference. After expiry or restart the key is a new operation; it has
no conflict guarantee and does not prove the earlier outcome failed. Restarting does
not resolve an uncertain outcome.

## Internal retries

The server already retries **safe Calendar and Contacts reads** after HTTP 429,
502, 503, or 504. It uses bounded backoff and `Retry-After`. The server never
repeats mutations or Mail send automatically. After its retry limit, obey
`retryable` and `retry_after_seconds` in the final MCP error.
