# Architecture

## Overview

`icloud-mcp` is a stdio MCP server that exposes a minimal, calendar-only surface
over Apple iCloud CalDAV. It is designed to run as a child process of an MCP host.

## Boot flow

1. `config.Load` reads environment variables and optional `file://` secrets.
2. A `security.Redactor` is built from the password, email, Basic auth material, and URL-escaped password.
3. `security.NewICloudHTTPClient` enforces HTTPS, TLS 1.2+, host allowlist, and redirect revalidation.
4. Basic auth wraps the client; `icloud.NewRetryClassifier` retries 429/502/503/504 and classifies errors.
5. `Client.Discover` PROPFINDs current-user-principal and calendar-home-set, then revalidates the shard host.
6. `GuardedService` applies rate limits (60 reads/min, 20 writes/min) and retries only idempotent ops.
7. MCP tools are registered; write tools are omitted when `ICLOUD_MCP_READ_ONLY=1`.
8. Optional loopback-only healthcheck; then `ServeStdio`.

## Packages

| Package | Role |
|---------|------|
| `cmd/icloud-mcp` | Wiring, timeouts, stdio serve |
| `internal/config` | Env + `file://` secrets |
| `internal/security` | Allowlist, redaction, audit |
| `internal/icloud` | CalDAV client, iCal, recurrence, free slots, validation, mock |
| `internal/mcptools` | MCP tool schemas and handlers |
| `internal/health` | Optional `/healthz` |

## MCP tools (V2)

| Tool | Network | Mutation | Read-only |
|------|---------|----------|-----------|
| `list_calendars` | yes | no | yes |
| `search_events` | yes | no | yes |
| `get_event` | yes | no | yes |
| `find_free_slots` | yes (search) | no | yes |
| `validate_event` | **no** | no | yes |
| `calendar_capabilities` | **no** | no | yes |
| `create_event` | yes | yes | hidden if RO |
| `update_event` | yes | yes | hidden if RO |
| `delete_event` | yes | yes | hidden if RO |

## Security invariants

- Destinations: `https://caldav.icloud.com` and `pXX-caldav.icloud.com` only.
- Credentials never appear in logs, audit, MCP errors, panics, or health.
- No `os/exec`, no local event store, no third-party telemetry.
- Mutations audited without title/location/notes.
- REPORT body cap 32 MiB; recurrence expansion 2000/series; search hard cap 400 / 366 days.

## Hand-rolled CalDAV (do not "simplify" away)

- Discovery PROPFIND (go-webdav loses shard host).
- REPORT with bare `calendar-data` + `getetag` (partial retrieval yields empty VEVENTs on iCloud).
- Conditional PUT/DELETE with `If-Match` (go-webdav v0.7.0 has no If-Match API).

See also `docs/caldav-compatibility.md`.
