# Architecture

## Overview

`icloud-mcp` is a stdio MCP server that exposes a minimal, calendar-only surface
over Apple iCloud CalDAV. It runs as a child process of an MCP host and speaks
JSON-RPC on stdin/stdout.

## Boot flow

1. `config.Load` reads env and optional `file://` secrets (errors omit identity).
2. `security.Redactor` is built from password, email, Basic-auth encodings, and
   URL-escaped password forms; stderr and stdlib `log` are redirected through it.
3. `security.NewICloudHTTPClient` enforces HTTPS, TLS 1.2+, host allowlist,
   `Proxy: nil`, and redirect revalidation.
4. Basic auth wraps the client; `icloud.NewRetryClassifier` retries
   429/502/503/504 and classifies errors.
5. `Client.Discover` PROPFINDs current-user-principal and calendar-home-set,
   then revalidates the shard host (and port on production iCloud hosts).
6. `GuardedService` applies rate limits (60 reads/min, 20 writes/min) and
   retries only idempotent ops (reads + series delete).
7. MCP tools register; write tools omitted when `ICLOUD_MCP_READ_ONLY=1`.
8. Optional loopback-only `-health`; then `ServeStdio`.

Per-tool timeout is 25s (strictly below the 30s HTTP timeout). Discovery at
boot is capped at 20s.

## Packages

| Package | Role |
|---------|------|
| `cmd/icloud-mcp` | Wiring, timeouts, stdio serve |
| `internal/config` | Env + `file://` secrets |
| `internal/security` | Allowlist, redaction, audit |
| `internal/icloud` | CalDAV client, iCal, recurrence, free slots, validation, mock |
| `internal/mcptools` | MCP tool schemas and handlers |
| `internal/health` | Optional `/healthz` |

## MCP tools

| Tool | Network | Mutation | Read-only mode |
|------|---------|----------|----------------|
| `list_calendars` | yes | no | available |
| `search_events` | yes | no | available |
| `get_event` | yes | no | available |
| `find_free_slots` | yes (via search) | no | available |
| `validate_event` | **no** | no | available |
| `calendar_capabilities` | **no** | no | available |
| `create_event` | yes | yes | hidden if RO |
| `update_event` | yes | yes | hidden if RO |
| `delete_event` | yes | yes | hidden if RO |

Handlers are concurrent (stdio worker pool). Shared state is thread-safe:
immutable-after-boot discovery cache (`sync.Once`) and rate-limiter buckets.
`update_event` always re-GETs before PUT; no mutable request state is carried
across calls.

## Write and recurrence notes

- **Create storage**: non-recurring timed → UTC `Z`; recurring timed (or
  explicit `timezone`) → TZID + generated VTIMEZONE; all-day → `VALUE=DATE`.
- **Structured recurrence** on create: `recurrence_frequency` /
  `interval` / `count` / `until` / `by_day` / `exceptions`, or raw `rrule`.
- **Alarms**: `alarm_minutes_before` and/or `alarms_minutes` (comma list), max 5.
- **Update/delete scope**: `series` (default) or `occurrence` + `recurrence_id`
  from `search_events.recurrenceId` (YYYY-MM-DD for all-day).
- **Search multi-calendar**: every calendar is queried, results sorted, then
  capped at 400 fairly; non-auth failures become `partialFailure` + warnings.

## Security invariants

- Destinations: `https://caldav.icloud.com` and `pXX-caldav.icloud.com` only.
- Credentials never appear in logs, audit, MCP errors, panics, or health.
- No `os/exec`, no local event store, no third-party telemetry.
- Mutations audited without title/location/notes.
- REPORT body cap 32 MiB; expansion 2000/series; search hard cap 400 / 366 days.

## Hand-rolled CalDAV (do not "simplify" away)

- Discovery PROPFIND (`discovery.go`): go-webdav loses the shard host;
  `net/http` turns 301 into GET and breaks PROPFIND/PUT.
- REPORT with bare `calendar-data` + `getetag` (partial retrieval yields empty
  VEVENTs on iCloud).
- Conditional PUT/DELETE with `If-Match` / create `If-None-Match: *`
  (go-webdav v0.7.0 has no If-Match API).
- Update always GETs the full object before re-PUT (REPORT data omits
  VERSION/PRODID/VTIMEZONE → go-ical encode failure).

See [caldav-compatibility.md](caldav-compatibility.md) and
[security.md](security.md).
