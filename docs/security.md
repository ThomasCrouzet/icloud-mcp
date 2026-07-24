# Security

Companion to [SECURITY.md](../SECURITY.md) with implementation detail for V2.

## Network allowlist

- Scheme: HTTPS only.
- Hosts: `caldav.icloud.com` or `p\d{1,3}-caldav.icloud.com` (case-sensitive).
- Port: empty or 443 only.
- Validation runs before DNS (`RoundTripper`) and on every redirect hop.
- Discovery also revalidates principal and home-set URLs (scheme + host). For
  production iCloud hostnames, port empty-or-443 is enforced at discovery too
  (`security.PortAllowed`). httptest fixtures use non-iCloud hosts with random
  ports and are unaffected.
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

Config and credential load errors never embed the email or password values.
Boot failures are logged before the production Redactor is installed, so
`config.Validate` and `loadCredential` keep error strings free of account
identity and secrets.

Calendar titles, notes, and locations are never written to audit logs.

## `file://` secrets (operator trust)

`ICLOUD_EMAIL` / `ICLOUD_PASSWORD` may use a `file://` prefix (Docker-style
secrets). The process reads that path once at startup. There is no chroot or
directory allowlist: **whoever can set the process environment is already
trusted** to choose which file is loaded. A path segment equal to `..` is
rejected as a footgun guard only (substring names like `app..pwd` are fine).
Missing or unreadable files fail with a stable reason code (`not_found`,
`permission_denied`, or `unreadable`) and never echo the path. Do not point
`file://` at shared or world-readable locations; mount secrets read-only for
the service user.

## Retry budget

Two layers may retry, both bounded by the per-tool context (25s in production):

1. **HTTP `retryClassifier`**: up to 6 attempts on 429/502/503/504 only,
   honoring `Retry-After` (capped at 10s) with backoff + jitter. Safe for
   writes because those statuses mean the server did not apply the request.
   Transport/network errors are not retried (a PUT may have landed).
   Exhausted retryable statuses become a typed `*icloud.Error`.
2. **`GuardedService`**: additional retries on **idempotent reads** (and
   series delete) for non-classified transient errors only (e.g. short
   connection blips). Create/update and occurrence-scoped deletes are never
   retried at this layer. Typed `*icloud.Error` values, including exhausted
   HTTP 503s, are returned immediately and are not retried again here.

Prolonged 503s are therefore capped by the HTTP layer alone (6 attempts).
The outer layer only multiplies attempts for unclassified transport failures
on reads / series delete, still within the tool timeout.

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
