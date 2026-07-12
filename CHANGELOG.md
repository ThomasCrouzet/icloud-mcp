# Changelog

All notable changes to this project are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

### Added
- Stable, Apple-aware CalDAV error classification with stable codes
  (`authentication_refused`, `forbidden`, `not_found`,
  `concurrent_modification`, `rate_limited`, `server_unavailable`,
  `http_error`). The code prefixes the MCP error text so callers can match on
  it.
- Explicit tests: conditional PUT sends `If-Match`; two concurrent updates on
  the same UID yield exactly one success and one `412 Precondition Failed`;
  retry behavior on `429`/`503` with and without `Retry-After`; non-retryable
  statuses are not retried; retry aborts on tool-timeout; error classification
  mapping.
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
