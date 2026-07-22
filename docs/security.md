# Security

Companion to [SECURITY.md](../SECURITY.md) with implementation detail for V2.

## Network allowlist

- Scheme: HTTPS only.
- Hosts: `caldav.icloud.com` or `p\d{1,3}-caldav.icloud.com` (case-sensitive).
- Port: empty or 443 only.
- Validation runs before DNS (`RoundTripper`) and on every redirect hop.
- Production destinations are not configurable. Tests inject `httptest` via `NewClient`.

## Credentials and PII

Redacted material:

- App-specific password (raw)
- Apple ID email
- Basic auth Base64 (`email:password`)
- URL-escaped password

Insertion points:

1. `RedactingWriter` on stderr (slog + stdlib `log` + audit)
2. `errResult` on every tool error
3. `RecoverRedactMiddleware` on panics (stdout JSON-RPC is not covered by stderr redaction alone)

Calendar titles, notes, and locations are never written to audit logs.

## Read-only mode

`ICLOUD_MCP_READ_ONLY=1` removes `create_event`, `update_event`, and `delete_event`
from `tools/list`. Local tools (`validate_event`, `calendar_capabilities`) and
read tools remain available.

## Concurrency (ETag)

- `get_event` returns `etag` when known.
- `update_event` / `delete_event` accept optional `etag` (`If-Match`).
- HTTP 412 maps to structured `concurrent_modification` and is **never** auto-retried.

## Free slots privacy

`find_free_slots` returns only free intervals. Busy event titles, notes, UIDs, and
locations are never included in the response.

## Structured errors

Payload shape:

```json
{"code":"concurrent_modification","message":"...","retryable":false}
```

Codes include: `validation`, `authentication`, `authorization`, `not_found`,
`conflict`, `concurrent_modification`, `rate_limited`, `timeout`, `unavailable`,
`partial_failure`, `protocol_error`, `internal_error`. Messages never embed raw
HTTP/XML bodies.
