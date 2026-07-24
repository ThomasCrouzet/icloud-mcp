# Security (implementation)

Companion to [SECURITY.md](../SECURITY.md) with implementation detail.

## Network allowlist

- Scheme: HTTPS only.
- Hosts: `caldav.icloud.com` or `p\d{1,3}-caldav.icloud.com` (case-sensitive).
- Port: empty or 443 only.
- Validation runs before DNS (`RoundTripper`) and on every redirect hop.
- Discovery revalidates principal and home-set URLs (scheme + host). Production
  iCloud hostnames also enforce empty-or-443 port (`security.PortAllowed`).
  httptest fixtures use non-iCloud hosts and are unaffected.
- Production destinations are not configurable. Tests inject `httptest` via
  `NewClient`.
- Production HTTP transport sets `Proxy: nil` (never honors `HTTP(S)_PROXY`).

## Credentials and PII

Redacted material:

- App-specific password (raw)
- Apple ID email
- Basic auth Base64 of `email:password` in Std, RawStd, URL, and RawURL encodings
- Password-only Base64 in the same four encodings
- Query-escaped and path-escaped password

Insertion points:

1. `RedactingWriter` on stderr (slog + stdlib `log` + audit)
2. `errResult` on every tool error
3. `writeJSON` on every tool success payload
4. `RecoverRedactMiddleware` on panics (stdout JSON-RPC is not covered by
   stderr redaction alone)

Config and credential load errors never embed email or password values. Boot
failures log before the production Redactor is installed, so `config.Validate`
and `loadCredential` keep error strings free of account identity and secrets.

Calendar titles, notes, and locations are never written to audit logs.

`delete_event` may return `deletedTitle` on the MCP success channel so a human
(or host) can confirm the target. That title is **not** written to the mutation
audit trail. Prefer `dry_run=true` first when the host can show a confirmation UI.

## `file://` secrets (operator trust)

`ICLOUD_EMAIL` / `ICLOUD_PASSWORD` may use a `file://` prefix. The process reads
that path once at startup. There is no chroot or directory allowlist: **whoever
can set the process environment is already trusted** to choose which file is
loaded. A path segment equal to `..` is rejected as a footgun guard only
(substring names like `app..pwd` are fine). Missing or unreadable files fail
with a stable reason code (`not_found`, `permission_denied`, or `unreadable`)
and never echo the path. Mount secrets read-only for the service user.

## Retry budget

Two layers may retry, both bounded by the per-tool context (25s in production):

1. **HTTP `retryClassifier`**: up to 6 attempts on 429/502/503/504 only,
   honoring `Retry-After` (capped at 10s) with backoff + jitter. Safe for
   writes because those statuses mean the server did not apply the request.
   Transport/network errors are not retried (a PUT may have landed).
   Exhausted retryable statuses become a typed `*icloud.Error`. Each attempt
   rewinds `req.Body` via `GetBody` so PUT/REPORT bodies are not sent empty
   after a prior retry.
2. **`GuardedService`**: additional retries on **idempotent reads** (and
   series delete) for non-classified transient errors only. Create/update and
   occurrence-scoped deletes are never retried here. Typed `*icloud.Error`
   values, including exhausted HTTP 503s, return immediately. Local rate limit
   waits are capped at 2s; longer delays fail fast with `rate_limited`.

Prolonged 503s are capped by the HTTP layer alone (6 attempts).

## Read-only mode

`ICLOUD_MCP_READ_ONLY=1` removes `create_event`, `update_event`, and
`delete_event` from `tools/list`. Local tools (`validate_event`,
`calendar_capabilities`) and read tools remain available.

## Concurrency (ETag)

- `get_event` / `search_events` return `etag` when known.
- `update_event`, series `delete_event`, and occurrence delete always require
  an ETag (`If-Match`); if the server omits one, the mutation fails closed
  (`concurrent_modification`) instead of last-writer-wins. Pass `etag` from
  `get_event` when needed. Client-supplied `etag=*` is rejected.
- Create sends `If-None-Match: *`; HTTP 412 maps to `conflict`.
- HTTP 412 on update/delete maps to `concurrent_modification` and is **never**
  auto-retried.

## Calendar path hardening

Agent-supplied calendar paths must be path-absolute (`/…`). Scheme-relative
forms (`//host/…`), query/fragment markers, backslashes, and percent-encoded
`..` are rejected. Paths are bound to the discovered home-set. PUT/REPORT/DELETE
also resolve paths with a same-host check against the discovered shard.

## Free slots privacy

`find_free_slots` returns only free intervals. Busy event titles, notes, UIDs,
and locations are never included in the response.

## Structured errors

```json
{"code":"concurrent_modification","message":"...","retryable":false}
```

Codes include: `validation`, `authentication`, `authorization`, `not_found`,
`conflict`, `concurrent_modification`, `rate_limited`, `timeout`, `unavailable`,
`partial_failure`, `protocol_error`, `internal_error`. Messages never embed raw
HTTP/XML bodies.
