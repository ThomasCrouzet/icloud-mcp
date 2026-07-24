# icloud-mcp

[![CI](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A **stdio MCP server** in Go that exposes the **Apple iCloud calendar** over CalDAV.
Single static binary, JSON-RPC on stdin/stdout, no network listener. Designed to run
as a child process of any MCP host.

**Calendar only**: never mail, contacts, reminders, or any other Apple service.

## Stack

Go 1.25+, modules. Five direct dependencies only:

| Dependency | Role |
|------------|------|
| `github.com/emersion/go-webdav` (+`/caldav`) | CalDAV client |
| `github.com/emersion/go-ical` | iCalendar parse/encode |
| `github.com/mark3labs/mcp-go` | MCP stdio SDK |
| `github.com/teambition/rrule-go` | Recurrence expansion |
| `golang.org/x/time` | Rate limiting |

No prometheus, godotenv, uuid, or telemetry. Any new direct dependency needs a
written justification in this README.

## MCP tools

| Tool | Mode | Description |
|------|------|-------------|
| `list_calendars` | read | Calendars (name, path, color, description). |
| `search_events` | read | Range search with expanded recurrences (`recurrenceId`, `etag`, filters, pagination, hard cap 400). |
| `get_event` | read | One event by calendar path + UID (`etag`, alarms, status, `overrides[]`). |
| `find_free_slots` | read | Free intervals only; never returns busy titles. |
| `validate_event` | read | Local create-shaped validation (no network). |
| `calendar_capabilities` | read | Version, limits, features (no secrets, no network). |
| `create_event` | write | Create (timed/all-day, alarms, RRULE or structured recurrence, status/transp/URL, `client_uid`). |
| `update_event` | write | Patch by UID (`scope` series/occurrence, optional `etag` If-Match; `etag=*` rejected). |
| `delete_event` | write | Delete by UID (`scope`/`recurrence_id`, optional `etag`, `dry_run`); may echo `deletedTitle` on MCP success (never in audit). |

`ICLOUD_MCP_READ_ONLY=1` removes the three write tools from `tools/list` (absent,
not rejected at call time). Recommended for first deploy.

Further docs: [architecture](docs/architecture.md), [security](docs/security.md),
[CalDAV compatibility](docs/caldav-compatibility.md), [testing](docs/testing.md).
Vulnerability reporting and threat model: [SECURITY.md](SECURITY.md).

### Recommended agent flow

1. `calendar_capabilities` once per session.
2. `list_calendars` for paths.
3. `search_events` / `get_event` / `find_free_slots` for reads.
4. `validate_event` before risky writes.
5. Mutations with human confirmation for deletes (`dry_run` first).
6. On `concurrent_modification`, re-`get_event` and retry with a fresh `etag`.

## Install

```bash
go install github.com/ThomasCrouzet/icloud-mcp/cmd/icloud-mcp@latest
```

Binary lands in `$(go env GOPATH)/bin`. Or build from source: `make build`
(dev) / `make release` (static linux/arm64 in a pinned `golang:1.25` container).

### MCP host config

```json
{
  "mcpServers": {
    "icloud-calendar": {
      "command": "icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "you@icloud.com",
        "ICLOUD_PASSWORD": "your-app-specific-password",
        "ICLOUD_MCP_READ_ONLY": "1",
        "ICLOUD_MCP_DEFAULT_TZ": "Europe/Paris"
      }
    }
  }
}
```

Prefer an absolute `command` path if the host has a sparse `PATH`. Restart the
host after saving.

`ICLOUD_PASSWORD` **must** be an [app-specific password](https://appleid.apple.com),
never the main Apple ID password. Start with `ICLOUD_MCP_READ_ONLY=1`.

#### `file://` secrets (containers)

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

Only disk read the binary performs, and only at startup. Mount secrets read-only.

## Configuration

| Variable | Role |
|----------|------|
| `ICLOUD_EMAIL` | Apple ID email. Supports `file://`. |
| `ICLOUD_PASSWORD` | App-specific password. Supports `file://`. |
| `ICLOUD_MCP_READ_ONLY` | `1`/`true`: write tools not registered. |
| `ICLOUD_MCP_LOG_LEVEL` | `debug`/`info`/`warn`/`error` (stderr JSON, default `info`). |
| `ICLOUD_MCP_DEFAULT_TZ` | IANA TZ for bare local `start`/`end` (no offset). Default `UTC`. |

Optional flag `-health <addr>`: loopback-only HTTP `/healthz` (off by default;
`0.0.0.0` and bare `:port` rejected). `-version` prints the build version.

See `.env.example`.

### Dates and timezones

`start`/`end` accept:

- **RFC3339 with offset** (`2026-07-01T14:00:00+02:00` or `...Z`): honored literally.
- **Local wall clock without offset** (`2026-07-01T14:00:00`): interpreted in
  `ICLOUD_MCP_DEFAULT_TZ` (DST-aware), else UTC.

Prefer the no-offset form for "the time the user said". Set
`ICLOUD_MCP_DEFAULT_TZ` to the calendar owner's zone.

**Storage:** non-recurring timed creates default to UTC `Z`. Timed recurring
creates (or any create with an explicit `timezone`) write **TZID + generated
VTIMEZONE** so wall-clock RRULEs survive DST. All-day uses `VALUE=DATE`.
`ICLOUD_MCP_DEFAULT_TZ` always governs input parsing; it also backs recurring
writes when `timezone` is omitted.

## Troubleshooting

Boot runs CalDAV discovery (two PROPFINDs). Failure prints JSON on stderr and
exits non-zero. Use `ICLOUD_MCP_LOG_LEVEL=debug` for the discovery trace.

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `authentication_refused` (401) | Bad/revoked app password or wrong email | Regenerate app-specific password on appleid.apple.com |
| `forbidden` (403) | Revoked password, or calendar at Apple's ~50k event quota | Regenerate password or free quota |
| Shard unreachable mid-session | Transient Apple outage | Retries 502/503/504; surfaces `server_unavailable` if persistent |
| `rate_limited` (429) | Apple or local throttle (60 reads / 20 writes per min) | Slow down; HTTP layer honors `Retry-After` |
| `concurrent_modification` (412) | Another client won the race | Re-read (`get_event`) and retry with fresh `etag` |
| Wrong times around DST | TZID stripped in a manual edit | Server preserves TZID on GET-then-PUT; do not hand-edit ICS |
| Create off by 1-2h | Agent sent `Z`/wrong offset instead of local wall clock | Send bare local time + set `ICLOUD_MCP_DEFAULT_TZ` |
| Missing recurring occurrence | `EXDATE` or `RECURRENCE-ID` override | Inspect master; override is its own row with moved time |

Apple limits: one Apple ID per process; ~50k events/calendar; search window
capped at 366 days; expansion capped at 2000 occurrences/series. iCloud rejects
server-side `prop-filter` (412), so UID lookup is GET `<uid>.ics` then a
±10 year REPORT fallback.

## Build and test

```bash
make build        # local binary (dev), host toolchain
make test         # go test ./... -race -cover
make lint         # go vet + golangci-lint (pinned)
make release      # static linux/arm64 via pinned golang:1.25 container
make release-all  # cross-compile linux/amd64, linux/arm64, darwin/arm64 (host Go)
make install      # release + copy to $(HOME)/.local/bin
```

Production deliverable: `make release` (`CGO_ENABLED=0`, `-trimpath`, stripped).
No host Go required for that path. CI also builds multi-arch and enforces a
20 MiB size budget (typical arm64 binary is well under).

## Threat model (summary)

The server is a stdio child driven by an LLM-powered host. Assume **the agent
can be compromised** (prompt injection). Blast radius is calendar-only for one
Apple ID:

- Hard allowlist: `https://caldav.icloud.com` and `pXX-caldav.icloud.com` only
  (HTTPS, TLS verified, non-443 ports rejected, every redirect hop).
- Password and email redacted from logs, errors, and MCP success/error paths
  (including panic recover on JSON-RPC).
- No `os/exec`, no disk writes, no telemetry. Sole disk read: boot `file://`
  secrets.
- Mutations audited on stderr without title/location/notes.
- App-specific password is revocable on appleid.apple.com without touching the
  main account.

Details: [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md).

## Security model (implementation)

| Mechanism | Detail |
|-----------|--------|
| Network allowlist | `RoundTripper` + redirect checks before DNS; shard revalidated after discovery; `Proxy: nil`. |
| TLS | Verified, min TLS 1.2, never `InsecureSkipVerify`. |
| Redaction | Password, email, Basic-auth Base64 (Std/RawStd/URL/RawURL), query/path-escaped forms; stderr writer, tool errors, success JSON, panic middleware. |
| Mutation audit | stderr JSON (`tool`, calendar, UID, status); no PII. `deletedTitle` may appear on MCP success only. |
| Rate limits | 60 reads/min, 20 writes/min; HTTP 30s; tool 25s; retries bounded (429/502/503/504 with `Retry-After`). |
| Input validation | Dates, range ≤ 366d, path/UID hardening, field bounds; re-checked on Client. |
| Concurrency | Mutations fail closed without ETag (`If-Match`); create uses `If-None-Match: *`. |
| Surface | Stateless after boot (discovery cache + rate buckets only). |

## Known limitations

- **If-Match required** for update/delete (series and occurrence). Missing ETag
  fails closed (`concurrent_modification`); pass `etag` from `get_event` /
  `search_events`. Conditional PUT/DELETE is hand-rolled (go-webdav v0.7.0 has
  no If-Match API).
- **UID fallback** scans ±10 years around now when `<uid>.ics` is missing.
- **Occurrence scope** needs a recurring master (RRULE). EXDATE/RECURRENCE-ID
  match the master DATE/TZID/Z form.
- **Expansion** capped at 2000/series (`truncatedByExpansion`); results hard-capped
  at 400 (`truncated`). Multi-calendar search queries every calendar then caps
  fairly after sort (`multiCalendarCapped`).
- **RDATE** not expanded (RRULE + EXDATE + RECURRENCE-ID only).
- **`this-and-future`** and attendees/invitations are not implemented.
- **Single iCloud account** per process.
- Tool errors: `{"code","message",...}` when classified; plain validation may
  omit `code`.

## Tests

Table-driven unit tests, mocked CalDAV (`httptest`), MCP in-process E2E,
native fuzz targets, redaction end-to-end (sentinel password never appears).
Real iCloud: `//go:build integration`, never CI. See [docs/testing.md](docs/testing.md).

```bash
go test ./... -race -cover
```

## Attribution

Inspired by the tool shape, rate-limit/retry decorator, cached discovery
(`sync.Once`), and pointer-based `EventUpdate` pattern of
[`github.com/roygabriel/mcp-icloud-calendar`](https://github.com/roygabriel/mcp-icloud-calendar)
(MIT, © 2026 Gabe). Code is rewritten, not copied. Not carried over: multi-account,
godotenv, prometheus, google/uuid, mTLS, request-id middleware.

Notable differences: hard network allowlist, secret redaction, custom PROPFIND
discovery (go-webdav v0.7.0 loses the shard host), EXDATE/RECURRENCE-ID with
TZID preservation, missing-DTEND handling, result caps, series/occurrence scope.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Run `gofmt`, `make test`, and `make lint`
before opening a PR. Keep the five direct dependencies unless justified here.

## License

MIT. See [LICENSE](LICENSE).
