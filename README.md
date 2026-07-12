# icloud-mcp

[![CI](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ThomasCrouzet/icloud-mcp)](https://goreportcard.com/report/github.com/ThomasCrouzet/icloud-mcp)
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

## Configuration

Environment variables (see `.env.example`):

| Variable | Role |
|----------|------|
| `ICLOUD_EMAIL` | Apple ID (email). Supports the `file://` prefix. |
| `ICLOUD_PASSWORD` | Apple **app-specific password**. Supports the `file://` prefix. |
| `ICLOUD_MCP_READ_ONLY` | `1`/`true` → read-only mode (write tools not registered). |

The `file://` prefix reads the secret from a file (Docker-secret-like pattern):
`ICLOUD_PASSWORD=file:///path/to/app-password`. This is the **only** disk read the
program performs, and only at startup.

The password MUST be an **app-specific password** generated on
[appleid.apple.com](https://appleid.apple.com), never the account's main password.
Optional flag `-health <addr>`: HTTP `/healthz` healthcheck (disabled by default; never
bind to `0.0.0.0`).

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
| **Mutation audit** | Every create/update/delete is logged to stderr: timestamp, tool, calendar, UID, status, **without** title or content (no PII). |
| **Rate limiting** | 60 reads/min, 20 writes/min (`x/time/rate` token bucket), 30s HTTP timeout, 25s per-tool timeout, bounded retries with backoff (idempotent operations only). |
| **Input validation** | RFC3339 dates, range ≤ 366 days, UID/paths without `..`/NUL, bounded field sizes. |
| **Minimal surface** | Stateless, zero `os/exec`, zero disk writes, zero telemetry. |

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
- **Update is last-writer-wins**: `PutCalendarObject` (go-webdav v0.7.0) does not
  support `If-Match`/ETag; a concurrent modification can be overwritten.
- **Single iCloud account** per instance.
- Recurrence expansion is bounded (protection against infinite RRULEs).

## Contributing

Before opening a PR, please run `gofmt`, `make test` (`go test -race`), and `make lint`
(`go vet` + `golangci-lint`). Keep the dependency list minimal; any new direct
dependency needs justification. PRs welcome.

## License

MIT. See the [LICENSE](LICENSE) file.
