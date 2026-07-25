# Changelog

All notable changes to this project are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-07-25

Unified Calendar, Contacts, and Mail MCP server. Default remains Calendar-only
(10 tools). Global read-only with optional domains off exposes 7 tools. The
complete surface is 23 tools when Contacts, Mail read, Mail mutation, and Mail
send are all enabled.

### Added
- Unified Calendar, Contacts, and Mail server in the existing `icloud-mcp`
  binary. Calendar remains always enabled; Contacts, Mail read, Mail mutation,
  and SMTP submission are independently composed from validated capability
  gates.
- Global local `icloud_capabilities` tool with version, global read-only state,
  enabled domains, effective capability groups, exact sorted tool inventory,
  and count. The default surface is 10 tools, global read-only is 7, and the
  complete surface is 23.
- Six opt-in Contacts tools over CardDAV: `list_address_books`,
  `search_contacts`, `get_contact`, `create_contact`, `update_contact`, and
  `delete_contact`. Contacts uses lazy successful-cache discovery, opaque
  address-book identifiers, bounded vCard 3.0/4.0 reads, vCard 3.0 writes,
  strong ETags, conditional mutations, compatible server-side search
  prefilters, combined bounded local filtering, and a bounded all-card path for
  digit-normalized phone search.
- Three opt-in Mail read tools over IMAP: `list_mailboxes`, `search_messages`,
  and `get_message`. Reads use fresh sessions, UIDVALIDITY identities,
  descending bounded UID windows, EXAMINE/PEEK, curated headers, bounded plain
  text, attachment metadata, and no raw MIME, HTML, or attachment payloads.
- Three independently gated IMAP mutation tools: `set_message_flags`,
  `move_message`, and `trash_message`. Mutations check UIDVALIDITY, fail closed
  with `protocol_error` before STORE when CONDSTORE is advertised because
  go-imap beta.8 cannot expose MODIFIED, use native MOVE or a UIDPLUS-only
  fallback, and never use plain EXPUNGE. The unavailable CONDSTORE flag path
  does not claim `concurrent_modification`.
- Independently gated `send_message` over authenticated SMTP submission with
  mandatory STARTTLS, a boot-validated exact-address recipient allowlist,
  all-RCPT-before-DATA policy, no automatic retry, and explicit
  `outcome_unknown` reconciliation. `to`, `cc`, and `bcc` are each optional,
  with at least one aggregate recipient required.
- Unified 12-variable configuration with strict parsing for all five booleans,
  dedicated Mail identity/password support, Mail password fallback, and
  boot-only `file://` support for every identity/password input.
- Per-domain Calendar/Contacts HTTP allowlists plus fixed IMAP and SMTP socket
  destinations, TLS 1.2 minimum, separate credentials/transports/rates/
  semaphores, Mail SASL redaction, and process-local opaque HMAC audit tokens for
  every Calendar, Contacts, IMAP, and SMTP mutation. Production audit records
  no longer contain raw Calendar paths or UIDs.
- Direct protocol dependencies for vCard, IMAP, MIME, SMTP, and SASL, bringing
  the documented direct dependency set to 10. Added CardDAV and Mail
  compatibility documentation and expanded architecture, security, setup, and
  testing guidance for the unified server.
- Tools: `get_event`, `find_free_slots`, `validate_event`, `calendar_capabilities`.
- `search_events` optional filters: multi-calendar list, UID, status, all-day,
  cancelled, busy-only, compact (omit notes), stable sort by start+UID.
- `search_events` returns `recurrenceId`, `isOverride`, and REPORT `etag` on
  each row; all-day times formatted as `YYYY-MM-DD`. Multi-calendar queries
  always hit every calendar then cap fairly; non-auth failures surface as
  `partialFailure` + warnings.
- `get_event` returns `overrides[]` with `recurrenceId` for exception targeting.
- `create_event` optional status, transparency, URL, timezone, `client_uid` /
  `idempotency_key` (conflict if UID already exists; never silent overwrite).
- Recurring timed creates write TZID + generated VTIMEZONE (timezone param or
  `ICLOUD_MCP_DEFAULT_TZ` fallback) so wall-clock RRULEs survive DST.
- `update_event` / `delete_event` support `scope=series|occurrence`,
  `recurrence_id`, and optional `etag` (If-Match). Occurrence never deletes
  the whole series. `delete_event` supports `dry_run` (zero PUT/DELETE).
- `ParseRecurrenceID` accepts `YYYY-MM-DD` for all-day occurrence ops.
- Conditional DELETE with If-Match; structured `conflict` for HTTP 409.
- Create PUT sends `If-None-Match: *` (hand-rolled) so concurrent same-UID
  creates cannot silently overwrite (412 maps to `conflict`).
- Expanded structured error codes (`validation`, `authentication`,
  `authorization`, `timeout`, `unavailable`, `partial_failure`,
  `protocol_error`, `internal_error`) with optional `retryable` /
  `retry_after_seconds`.
- Pure free-slot interval merge (buffers, working hours, overnight windows,
  generative tests). Local event validation shared by create/validate tools.
- Native Go fuzz targets (paths, UIDs, RRULE, dates, redaction, hosts) and
  MCP in-process E2E registration tests.
- Docs under `docs/` (architecture, security, CalDAV, CardDAV, Mail, testing).
  CI: mod verify/tidy, fuzz smoke, pinned govulncheck, binary size budget,
  `InsecureSkipVerify` guard.
- `CONTRIBUTING.md`; integration runbook in `docs/testing.md`.
- Redactor also masks password-only Base64 (Std/RawStd/URL/RawURL).
- Enforced a 1 MiB stdio frame cap, 64 KiB caller-reflecting error threshold,
  256 KiB Calendar/MCP result caps, 1 MiB SMTP inbound-response budget,
  XML/IMAP/MIME/iCalendar parser depth and item bounds, and pathological
  recurrence work budgets.
- Native fuzz targets in all five parser/security packages, per-package coverage
  floors, a 78% aggregate coverage gate, and opt-in live Contacts and Mail
  integration gates. Live tests require credentials and are not run in CI.

### Fixed
- SMTP failures or parser panics after DATA starts now retain
  `outcome_unknown`; cleanup failures cannot overwrite a definitive accepted
  result. Successful IMAP COPY/MOVE with unusable COPYUID metadata likewise
  retains reconciliation guidance.
- `find_free_slots` now fails closed on any incomplete calendar, widens its
  query for event buffers, and never reports authoritative availability from a
  truncated recurrence or partial search.
- Calendar recurrence validation rejects unreachable clock and date selectors
  before entering non-interruptible dependency loops; unsafe non-RFC or
  sub-daily selector combinations fail closed. Search now has aggregate
  event/work bounds and independent 4/2 read/write semaphores. Malformed remote
  objects fail closed instead of disappearing from results.
- iCalendar input is structurally preflighted before recursive decoding.
  VTIMEZONE transitions use exact pre-transition wall times and correct local
  annual rules; all-day EXDATE/UNTIL values preserve DATE form.
- CardDAV REPORT/GET redirects remain inside the selected opaque address book.
  Contacts paths reject encoded traversal/separators and queries, ETags are
  strict and singular, and ambiguous-write reconciliation reaches MCP clients.
- Every JSON-RPC output frame is redacted, including protocol errors emitted
  before tool middleware. Overlapping redaction secrets are processed longest
  first.
- Credential files are regular-file-only and capped at 4 KiB. Accepted
  identities are plain addresses, and credential control characters fail at
  boot without leaking values.
- The optional health listener no longer resolves arbitrary hostnames and now
  has read, write, idle, and header limits.
- Local rate limiter fails fast (`rate_limited`) when the next token is more
  than 2s away, instead of blocking toward the 25s tool timeout.
- Calendar retry handling now rewinds read request bodies only. Automatic
  retries are limited to reads; PUT, DELETE, and full-series delete are never
  replayed, and every ambiguous dispatched mutation, including a redirect,
  returns `outcome_unknown`.
- Imported-UID fallback scan window widened from +/-5 years to +/-10 years.
- `gofmt` alignment on `Event.ETag` field.
- Occurrence update with only `start` keeps duration (DTEND = start + prior length).
- Client-supplied `etag=*` rejected (no last-writer-wins If-Match).
- Caller-supplied calendar paths bound to the discovered home-set; REPORT hrefs
  validated.
- Expanded occurrences no longer carry the master RRULE; EXDATE re-delete is
  deduped; UID rejects backslash and other controls.
- Redactor iterates to a fixed point so secrets re-formed across a previous
  `[REDACTED]` boundary are still masked; secrets with newlines are rejected
  at registration (line-buffered writer cannot mask them).
- Config validation and `file://` load errors no longer embed email, password,
  or filesystem paths (boot failures log before the Redactor is installed).
- Discovery revalidates port empty-or-443 on production iCloud hostnames
  (principal and home-set), matching the HTTP transport allowlist.
- Calendar paths reject scheme-relative hosts (`//...`), query/fragment, and
  percent-encoded `..`; PUT/REPORT/DELETE keep the resolved URL on the shard.
- `update_event` preserves existing DTSTART/DTEND form (DATE / TZID / Z) instead
  of forcing UTC Z; occurrence EXDATE/RECURRENCE-ID match the master form.
- Imported-UID lookup (REPORT) always re-GETs before mutate so VTIMEZONE and
  VERSION survive the round-trip.
- `update_event` and series `delete_event` fail closed when no ETag is available
  (no unconditional PUT/DELETE; pass `etag` from `get_event`).
- `scope=occurrence` requires a recurring master (RRULE); override copy skips
  ATTENDEE/ORGANIZER; duration defaults honor nominal DURATION and next-civil-day
  all-day semantics across DST.
- `update_event` rejects non-string optional fields (no silent clear).
- Discovery failures are no longer cached forever (retry after transient errors).
- MCP success payloads and path-escaped password forms are redacted; free-slot
  duration/buffers clamped to schema maxes; `validate_event` accepts
  `idempotency_key`; `list_calendars` normalizes absolute hrefs to paths.
- Binary version for `calendar_capabilities` is passed via tool deps (no global).

### Changed
- Read-only mode with optional domains disabled now exposes 7 tools (was 2),
  including the global capability tool; write tools remain absent.
- Default `ICLOUD_MCP_DEFAULT_TZ` remains UTC for compatibility (operators
  should set the calendar owner timezone explicitly).
- Docs: operator trust for `file://` secrets, read-request body rewind plus the
  2s local rate-limit cap, CONTRIBUTING; threat model notes `deletedTitle`
  on MCP success (never in audit logs). README and `docs/` refreshed to match
  current behavior (fair multi-calendar search, TZID+VTIMEZONE recurring
  creates, structured recurrence/alarms). Removed internal audit and V2
  migration notes from the tree.
- `file://` load failures report a stable reason code (`not_found`,
  `permission_denied`, or `unreadable`) without the path.
- `make release` pins `golang:1.25` by image digest; `make lint` falls back to
  pinned `golangci-lint` via `go run` when not on PATH.
- `make build` embeds `VERSION` via the same ldflags as release targets
  (`make build VERSION=v0.3.0`).
- Release targets now reject unset/`dev` versions, create `dist/` from a clean
  checkout, package binaries with license notices, and emit SHA-256 checksums.
  `make install` now builds for the host instead of installing linux/arm64.
- Version reporting falls back to Go module build information for
  `go install ...@version` while preserving the release ldflags override.
- CI now checks Darwin arm64 builds, release version injection, public-text
  ASCII policy, forbidden attribution trailers, and unresolved direct network
  destinations.

### Security
- Account identity is never printed on invalid `ICLOUD_EMAIL` at boot.
- `file://` credential paths with a `..` segment or an empty path are rejected.
- Redactor also masks Basic-auth base64 in RawStd, URL, and RawURL encodings.
- Stdio output applies a hard 256 KiB frame cap after redaction so a missed
  domain result budget cannot inflate the JSON-RPC channel.

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
- `findEventByUID` fallback scans about +/-5 years around now (not 1970-2100).
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
