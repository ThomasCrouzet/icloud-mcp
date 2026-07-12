# icloud-mcp

[![CI](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A **stdio MCP server** written in Go that exposes the **Apple (iCloud) calendar** over
CalDAV: reading and writing events, with a minimal, auditable scope. A single static
binary, designed to run as a child process of any MCP host or agent gateway, speaking
JSON-RPC over stdin/stdout. No network listener, no extra setup.

**Calendar ONLY**: never mail, contacts, reminders or any other Apple service.

## Stack

- **Go 1.25+**, modules. Only 5 direct dependencies:
  - `github.com/emersion/go-webdav` (+`/caldav`): CalDAV client
  - `github.com/emersion/go-ical`: iCalendar parsing
  - `github.com/mark3labs/mcp-go`: official Go MCP SDK (stdio transport)
  - `github.com/teambition/rrule-go`: recurrence expansion
  - `golang.org/x/time`: rate limiting

No other direct dependency (no prometheus, godotenv, uuid, telemetry).

## MCP tools

| Tool | Type | Description |
|------|------|-------------|
| `list_calendars` | read | Lists calendars (name, path, color, description). |
| `search_events` | read | Events over a date range (recurrences expanded, pagination, hard cap of 400). |
| `create_event` | write | Creates an event (title, start, end, calendar; optional location/notes/alarm). |
| `update_event` | write | Modifies an event by UID (only the supplied fields). |
| `delete_event` | write | Deletes an event by UID; echoes back the title for confirmation. |

**`ICLOUD_MCP_READ_ONLY=1`** removes the 3 write tools from `tools/list` (they are
absent, not merely rejected at execution time). This is the recommended initial
deployment mode.

## Installation

### Install via `go install`

```bash
go install github.com/ThomasCrouzet/icloud-mcp/cmd/icloud-mcp@latest
```

The binary lands in `$(go env GOPATH)/bin`; make sure that directory is on your
`PATH`.

### Configure it in an MCP host

Example configuration, valid for any MCP host that launches stdio servers:

```json
{
  "mcpServers": {
    "icloud-calendar": {
      "command": "icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "you@icloud.com",
        "ICLOUD_PASSWORD": "your-app-specific-password",
        "ICLOUD_MCP_READ_ONLY": "1"
      }
    }
  }
}
```

`ICLOUD_PASSWORD` MUST be an **app-specific password** generated on
[appleid.apple.com](https://appleid.apple.com), never your main Apple ID password.
Start with `ICLOUD_MCP_READ_ONLY=1` and lift it only once you trust your setup.

Most stdio MCP hosts store this block in a JSON config file (the exact path
and the UI to add a server vary by host). Use an absolute `command` path so the
host finds the binary regardless of its working directory:

```json
{
  "mcpServers": {
    "icloud-calendar": {
      "command": "/usr/local/bin/icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "you@icloud.com",
        "ICLOUD_PASSWORD": "your-app-specific-password",
        "ICLOUD_MCP_READ_ONLY": "1"
      }
    }
  }
}
```

After saving, restart the host. Once the server is connected, `list_calendars`
and `search_events` are available immediately (the write tools stay hidden while
`ICLOUD_MCP_READ_ONLY=1`).

#### Secret files (Docker / 12-factor)

The `file://` prefix reads a secret from a file path without putting the value
in the host config or the process environment:

```json
{
  "mcpServers": {
    "icloud-calendar": {
      "command": "/usr/local/bin/icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "file:///run/secrets/icloud-email",
        "ICLOUD_PASSWORD": "file:///run/secrets/icloud-password",
        "ICLOUD_MCP_READ_ONLY": "1"
      }
    }
  }
}
```

This is the recommended mode for containerised deployments (mount the secret
files read-only). It is the only disk read the binary performs, at startup.

## Configuration

Environment variables (see `.env.example`):

| Variable | Role |
|----------|------|
| `ICLOUD_EMAIL` | Apple ID (email). Supports the `file://` prefix. |
| `ICLOUD_PASSWORD` | Apple **app-specific password**. Supports the `file://` prefix. |
| `ICLOUD_MCP_READ_ONLY` | `1`/`true` → read-only mode (write tools not registered). |
| `ICLOUD_MCP_LOG_LEVEL` | `debug`/`info`/`warn`/`error` for the structured JSON logs on stderr (default `info`). |
| `ICLOUD_MCP_DEFAULT_TZ` | IANA timezone (e.g. `Europe/Paris`) used to interpret a `start`/`end` value with no explicit RFC3339 offset. Default `UTC`. See "Dates and timezones" below. |

The `file://` prefix reads the secret from a file (Docker-secret-like pattern):
`ICLOUD_PASSWORD=file:///path/to/app-password`. This is the **only** disk read the
program performs, and only at startup.

The password MUST be an **app-specific password** generated on
[appleid.apple.com](https://appleid.apple.com), never the account's main password.
Optional flag `-health <addr>`: HTTP `/healthz` healthcheck (disabled by default; never
bind to `0.0.0.0`).

### Dates and timezones

`start`/`end` values (`create_event`, `update_event`, `search_events`) accept two forms:

- **RFC3339 with an explicit offset**, e.g. `2026-07-01T14:00:00+02:00` or
  `2026-07-01T14:00:00Z`. Parsed literally: the offset is a deliberate choice by the
  caller and is always honored as-is.
- **A local wall-clock time with no offset**, e.g. `2026-07-01T14:00:00`. Interpreted
  in `ICLOUD_MCP_DEFAULT_TZ` (DST-aware), UTC if that variable is unset.

The no-offset form is the recommended one for "the time the user said": it removes
timezone-offset arithmetic from the calling agent entirely, which is exactly the kind
of computation an LLM tends to get wrong. Set `ICLOUD_MCP_DEFAULT_TZ` to the IANA
timezone of the calendar's owner (e.g. `Europe/Paris`) so a bare local hour resolves
correctly across DST changes without the agent doing any conversion. Reserve the
explicit-offset form for a value that is deliberately in a different, specific
timezone (e.g. a call scheduled in UTC, or in another city's local time).

## Troubleshooting (CalDAV / iCloud)

The server validates credentials against iCloud at boot (`Discover` runs two
PROPFINDs). If startup fails, the error is printed to stderr as JSON and the
process exits non-zero. Set `ICLOUD_MCP_LOG_LEVEL=debug` for the full
discovery trace.

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `authentication_refused` (HTTP 401) at boot | Wrong/revoked app-specific password, or typo in `ICLOUD_EMAIL`. Use the email of the iCloud account whose calendar you want. | Regenerate an app-specific password on [appleid.apple.com](https://appleid.apple.com) → Sign-In and Security → App-Specific Passwords. It is NOT the main Apple ID password. |
| `forbidden` (HTTP 403) | App-specific password revoked, or the calendar holds the maximum of 50,000 events (Apple quota). | Revoke and regenerate the password; or delete events to get back under the quota. |
| Discovery succeeds but a shard (`pXX-caldav.icloud.com`) is unreachable mid-session | Transient Apple shard outage. | The server retries `502`/`503`/`504` with `Retry-After` + backoff; if the shard stays down it surfaces `server_unavailable`. Retry later. |
| `rate_limited` (HTTP 429) | Apple is throttling. The built-in 60 reads/min + 20 writes/min limits usually prevent this, but a burst across many calendars can still trip Apple. | Reduce concurrency / request frequency. The HTTP layer already retries 429 with `Retry-After`. |
| `concurrent_modification` (HTTP 412 on `update_event`) | Another client (Calendar app, another agent) modified the event between your read and your update. | Re-read with `search_events` and re-apply the update. `update_event` sends `If-Match` (ETag) so the conflict is detected rather than silently overwriting. |
| Events appear with wrong times around a DST change | TZID was lost in a manual edit (forcing `.UTC()` on a `Dtstart`). | This server preserves TZID through GET-then-PUT; never edit the raw `text/calendar` outside the server. See `internal/icloud/recurrence.go`. |
| A newly created event is off by 1 to 2 hours from the time the user asked for | The calling agent sent an RFC3339 `start`/`end` with an explicit offset (often `Z`/UTC) instead of the intended local time; the offset is always honored literally. | Have the agent send the local wall-clock time with no offset (e.g. `2026-07-01T14:00:00`) and set `ICLOUD_MCP_DEFAULT_TZ` to the calendar owner's IANA timezone; see "Dates and timezones" above. |
| `search_events` returns no events for a known recurring series | An `EXDATE` excludes the occurrences, or a `RECURRENCE-ID` override replaces them. | Inspect the master object; the override appears as its own occurrence with the moved time. |

### Apple CalDAV limits to keep in mind

- One iCloud account per server instance (single Apple ID).
- 50,000 events per calendar (Apple quota; above that, writes return 403).
- Recurrence expansion is bounded (protection against infinite RRULEs); the
  `search_events` window is capped at 366 days.
- `update_event` modifies the master VEVENT only; `RECURRENCE-ID` overrides
  are left unchanged.
- iCloud rejects server-side `prop-filter` queries (returns 412), so
  `findEventByUID` does a direct GET on `<uid>.ics` then falls back to a
  wide time-range scan for imported events whose file name differs from the UID.

For the security model, threat model, and how to report a vulnerability, see
[SECURITY.md](SECURITY.md).

## Build & test

```bash
make build      # local binary (dev), host toolchain
make test       # go test ./... -race -cover
make lint       # go vet + golangci-lint
make release    # static linux/arm64 binary via a golang:1.25 container
make install    # release + copy to $(HOME)/.local/bin/icloud-mcp
```

The production binary is compiled **inside a `golang:1.25` container**
(`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 -trimpath -ldflags='-s -w'`); no Go toolchain
is assumed present on the host. Expected binary: static, stripped, < 15 MB.

## Threat model

This server runs as a stdio child process inside an agent's process or container,
driven by an LLM. The threat model assumes **the agent driving this server can be
compromised or manipulated** (prompt injection, etc.).

### What the server CAN do
- Connect to **a single** network destination: `https://caldav.icloud.com` and its
  shards `pXX-caldav.icloud.com` (hardcoded allowlist, TLS always verified, every other
  destination rejected, including via HTTP redirection).
- Read the calendars and events of the configured iCloud account.
- In write mode (READ_ONLY disabled): create, modify, delete events.

### What the server CANNOT do
- Access any other Apple service: **never** mail, contacts, reminders, photos, files.
  CalDAV (calendar) only.
- Reach any network destination outside iCloud (strict allowlist, non-standard ports
  rejected).
- Execute commands (zero `os/exec`), write to disk (stateless; the only disk read is
  the `file://` credentials at startup), or emit telemetry.
- Leak the password: it is redacted from every output (logs, errors, MCP responses,
  including the protocol error path on panic).

### If the agent driving the server is compromised
At worst, an attacker can **read and (if READ_ONLY is lifted) modify or delete the
calendar** of the configured account, nothing else. It cannot exfiltrate the
credentials (redacted), nor pivot to another service or network destination.

**Revocation**: `ICLOUD_PASSWORD` is an **app-specific password** (never the main Apple
password). It can be revoked at any time on
[appleid.apple.com](https://appleid.apple.com) → Sign-In and Security → App-Specific
Passwords, with no impact on the account. An app-specific password only grants access
to DAV services, and this server only exercises the CalDAV scope of that access.

## Security model (implementation)

| Mechanism | Detail |
|-----------|--------|
| **Network allowlist** | An `http.RoundTripper` that rejects any host other than `caldav.icloud.com` / `pXX-caldav.icloud.com`, any scheme other than https, and any explicit port, BEFORE DNS resolution; covers every redirect hop (+ `CheckRedirect`). The discovered shard host is re-validated. |
| **TLS** | Always verified, `MinVersion` TLS 1.2, never `InsecureSkipVerify`. |
| **Secret redaction** | The password (+ its Basic-auth base64 form and URL-encoded form) and the email are masked from every output: stderr (slog + audit + transport logger, stdlib `log`), and MCP responses (redacted error helper + redacted recover middleware for the JSON-RPC channel). |
| **Mutation audit** | Every create/update/delete is logged to stderr as structured JSON (timestamp, level, `msg=audit`, tool, calendar, UID, status), **without** title or content (no PII). |
| **Rate limiting** | 60 reads/min, 20 writes/min (`x/time/rate` token bucket), 30s HTTP timeout, 25s per-tool timeout, bounded retries with backoff (idempotent operations only). HTTP 429/502/503/504 from the CalDAV shard are retried with `Retry-After` honoring + exponential backoff/jitter (6 attempts, bounded by the tool timeout). |
| **Input validation** | RFC3339 dates (offset honored literally) or local time resolved via `ICLOUD_MCP_DEFAULT_TZ`, range ≤ 366 days, UID/paths without `..`/NUL, bounded field sizes. |
| **Minimal surface** | Stateless, zero `os/exec`, zero disk writes, zero telemetry. The only in-memory state shared across requests is the (immutable-after-boot) discovery cache (`sync.Once` over the shard base + calendar home-set) and the rate-limiter token buckets (a process-wide throttle, not per-client session state). No request depends on a previous request's mutable state: `update_event` re-reads the event fresh via GET, so it never carries state across calls. |

## Tests

Table-driven tests against a mocked CalDAV server (`httptest`) covering: the full
list/search/create/update/delete cycle, shard discovery, auth errors (401), rate
limiting, RRULE/EXDATE/override expansion (including TZID preservation across a DST
change), the network allowlist, and **end-to-end redaction** (the sentinel password
never appears in any output, with a positive control). A real integration test
(`//go:build integration`) hits actual iCloud, skipped by default, never run in CI.

```bash
go test ./... -race -cover
```

## Attribution

This project is inspired by the tool structure, the rate-limit/retry decorator
pattern, the cached shard discovery (`sync.Once`), and the pointer-based
`EventUpdate` pattern of
[`github.com/roygabriel/mcp-icloud-calendar`](https://github.com/roygabriel/mcp-icloud-calendar)
(MIT license, © 2026 Gabe). The code is **rewritten, not copied**. Not carried over:
multi-account support, godotenv, prometheus/metrics, google/uuid, mTLS, request-id
middleware.

Notable deviations and fixes: single account, **hard network allowlist** (absent from
the reference), secret redaction, custom shard discovery via PROPFIND (go-webdav
v0.7.0 loses the shard host), correct EXDATE + RECURRENCE-ID override expansion with
TZID preservation, handling of a missing DTEND, hard cap of 400 results.

## Known limitations
- **Update relies on an opportunistic ETag**: `update_event` re-reads the full
  object via GET (which returns an `ETag`) and sends an `If-Match` header on the
  PUT, so a concurrent modification is rejected with a stable
  `concurrent_modification` error rather than silently overwriting. go-webdav
  v0.7.0 `PutCalendarObject` does not support `If-Match`, so the conditional PUT
  is hand-rolled. When the server returns no `ETag` (rare, or the wide-scan
  fallback path for imported events), the PUT degrades to unconditional,
  last-writer-wins, never worse than before.
- **Single iCloud account** per instance.
- Recurrence expansion is bounded (protection against infinite RRULEs).

## Contributing

Before opening a PR, please run `gofmt`, `make test` (`go test -race`), and `make lint`
(`go vet` + `golangci-lint`). Keep the dependency list minimal; any new direct
dependency needs justification. PRs welcome.

### MCP SDK (`mark3labs/mcp-go`) watching

This server pins `mark3labs/mcp-go` and uses only stable APIs: `NewMCPServer`,
`WithToolCapabilities`/`WithRecovery`/`WithInstructions`/`ToolHandlerMiddleware`,
`AddTool`, `ServeStdio`, `WithErrorLogger`. It does **not** rely on the
`initialize` handshake or on any MCP-level authorization (the only
authentication is Basic Auth at the CalDAV HTTP layer). Before upgrading
`mcp-go` to a new minor/major, check the changelog for removal of
`initialize` or a new authorization model, and run `make test`. Dependabot
opens upgrade PRs; review them against the pinned-dependency policy above.
The server MUST stay 100% stateless (no per-request mutable state carried
across calls): see the "Minimal surface" row in the security model table.

## License

MIT. See the [LICENSE](LICENSE) file.
