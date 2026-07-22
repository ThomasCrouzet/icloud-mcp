# Changelog

All notable changes to this project are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Tools: `get_event`, `find_free_slots`, `validate_event`, `calendar_capabilities`.
- `search_events` optional filters: multi-calendar list, UID, status, all-day,
  cancelled, busy-only, compact (omit notes), stable sort by start+UID.
- `create_event` optional status, transparency, URL, timezone, `client_uid` /
  `idempotency_key` (conflict if UID already exists; never silent overwrite).
- `update_event` / `delete_event` support `scope=series|occurrence`,
  `recurrence_id`, and optional `etag` (If-Match). Occurrence never deletes
  the whole series. `delete_event` supports `dry_run` (zero PUT/DELETE).
- Conditional DELETE with If-Match; structured `conflict` for HTTP 409.
- Expanded structured error codes (`validation`, `authentication`,
  `authorization`, `timeout`, `unavailable`, `partial_failure`,
  `protocol_error`, `internal_error`) with optional `retryable` /
  `retry_after_seconds`.
- Pure free-slot interval merge (buffers, working hours, overnight windows,
  generative tests). Local event validation shared by create/validate tools.
- Native Go fuzz targets (paths, UIDs, RRULE, dates, redaction, hosts) and
  MCP in-process E2E registration tests.
- Docs under `docs/` (architecture, security, CalDAV, testing, V2 migration,
  audit). CI: mod verify/tidy, fuzz smoke, pinned govulncheck, binary size
  budget, `InsecureSkipVerify` guard.

### Fixed
- Redactor iterates to a fixed point so secrets re-formed across a previous
  `[REDACTED]` boundary are still masked; secrets with newlines are rejected
  at registration (line-buffered writer cannot mask them).

### Changed
- Read-only mode now exposes 6 tools (was 2); write tools remain absent.
- Default `ICLOUD_MCP_DEFAULT_TZ` remains UTC for compatibility (operators
  should set the calendar owner timezone explicitly).

## [0.2.0] - 2026-07-18

### Changed
- `update_event` now sends an opportunistic `If-Match` (ETag) on its PUT,
  re-reading the full object via GET first. A concurrent modification is
  rejected with a stable `concurrent_modification` error instead of silently
  overwriting. go-webdav v0.7.0 `PutCalendarObject` lacks `If-Match` support,
  so the conditional PUT is hand-rolled (consistent with the hand-rolled
  discovery and REPORT). When the server returns no ETag, the PUT degrades to
  unconditional, last-writer-wins, never worse than before.
- CalDAV responses `429`/`502`/`503`/`504` are now retried at the HTTP doer
  layer, honoring `Retry-After` (delta-seconds and HTTP-date forms) and falling
  back to exponential backoff with jitter (6 attempts max, bounded by the 25s
  per-tool timeout). `GuardedService` no longer double-retries classified
  HTTP errors.
- REPORT calendar-query requests `D:getetag` with bare `calendar-data`, so
  imported events found only via REPORT still get conditional PUT when ETag is
  present.
- `findEventByUID` fallback scans about ±5 years around now (not 1970-2100).
- MCP tool errors are JSON `{"code","message"}` when a classified CalDAV error
  applies; plain validation errors omit `code`.
- `search_events` multi-calendar mode stops starting new calendars once the
  400-event hard cap is filled (`multiCalendarCapped`).
- `RedactingWriter` emits only complete lines, buffering the trailing partial
  line across `Write` calls, so a secret split mid-stream is still masked. A
  secret never contains a newline, so no line boundary can split one. `Flush`
  drains an unterminated tail.
- Optional `-health` rejects non-loopback binds (`0.0.0.0`, bare `:port`, etc.).
- CI/Makefile pin `golangci-lint` to v2.1.6 (no bare `@latest`).

### Added
- Client-side input validation on CalDAV `Client` methods (path, UID, text
  bounds, range, RRULE) in addition to MCP handlers.
- `search_events` sets `truncatedByExpansion` when a series hits the 2000
  occurrence expansion cap.
- `create_event` supports `all_day` (VALUE=DATE, exclusive end) and an optional
  `rrule` on the master VEVENT. Timed events are still written as UTC.
- Stable, Apple-aware CalDAV error classification with stable codes
  (`authentication_refused`, `forbidden`, `not_found`,
  `concurrent_modification`, `rate_limited`, `server_unavailable`,
  `http_error`).
- Explicit tests: conditional PUT sends `If-Match`; REPORT-path If-Match;
  two concurrent updates on the same UID yield exactly one success and one
  `412 Precondition Failed`; retry behavior on `429`/`503`; GuardedService
  SearchEvents composition; redactor split-Write; health loopback-only;
  all-day/RRULE create; structured tool errors.
- CI: coverage profile upload (`go test -race -coverprofile`),
  `govulncheck` on `go.mod`, and a multi-arch build matrix (linux/amd64 +
  linux/arm64).
- Dependabot for Go modules and GitHub Actions.

## v0.1.0

Initial public release: stdio MCP server exposing the iCloud calendar over
CalDAV. Calendar only; never another Apple service. 5 direct dependencies, hard
network allowlist, secret redaction, mutation audit, rate limiting, recurrence
expansion (RRULE + EXDATE + RECURRENCE-ID with TZID preservation), read-only
mode, per-tool timeout, bounded retries for idempotent operations.
