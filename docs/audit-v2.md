# V2 initial hostile audit (pre-change)

Baseline captured 2026-07-22 on `main` @ `1c6e34c`.

| Check | Result |
|-------|--------|
| `go test ./... -count=1` | PASS |
| `go test ./... -race -count=1` | PASS |
| Coverage total | 80.8% |
| `gofmt -l .` | empty |
| `go vet ./...` | PASS |
| `go mod verify` | PASS |
| `go build ./cmd/icloud-mcp` | PASS |
| golangci-lint (host) | not installed (CI pins v2.1.6) |

## Architecture (concise)

```
config.Load → Redactor → AllowlistHTTP+TLS → BasicAuth → RetryClassifier
  → Client.Discover (PROPFIND principal + home-set, shard revalidation)
  → GuardedService (60 reads/min, 20 writes/min; retry reads only)
  → MCP Register (RO hides create/update/delete)
  → ServeStdio (stderr redacted; recover-redact on tools)
```

## Security invariants

1. Network destinations: `https://caldav.icloud.com` and `pXX-caldav.icloud.com` only, port 443, TLS verified.
2. Credentials/email never appear in logs, audit, MCP errors, panics, health, or retries.
3. Read-only removes write tools from `tools/list` (not present).
4. No `os/exec`, no local event/credential persistence, no third-party telemetry.
5. Mutations audited without title/location/notes.
6. REPORT body capped at 32 MiB; recurrence expansion 2000/series; search hard cap 400 / 366 days.

## Defect table (initial)

| ID | Severity | Domain | File | Evidence | Impact | Fix | Test |
|----|----------|--------|------|----------|--------|-----|------|
| A01 | Medium | Functional | service.go | No `GetEvent` API | Agent must scan ranges to fetch one UID | Add `GetEvent` + tool | unit + mock |
| A02 | Medium | Functional | mcptools | No free-busy tool | Agents invent availability from full events (PII) | `find_free_slots` pure merge | generative |
| A03 | Medium | CalDAV | client.go DeleteEvent | No `If-Match` on DELETE | Last-writer-wins on concurrent delete | Conditional DELETE + 412 | mock 412 |
| A04 | High | CalDAV | service.go | No series/occurrence scope | Cannot safely edit one occurrence | `scope` + RECURRENCE-ID/EXDATE | round-trip |
| A05 | Medium | MCP | helpers.go | Error codes incomplete vs objective list | Agents cannot branch on validation/timeout | Expand `Code` + MCP payload | contract |
| A06 | Low | Functional | create | Single alarm; no status/transp/URL/client UID | Limited write surface | Enrich `NewEvent` | unit |
| A07 | Medium | Search | search_events | No status/all-day/busy/UID filters; notes always returned | Over-fetch / weak filtering | Optional filters + compact | pagination |
| A08 | Low | Local | n/a | No offline validate/capabilities | Agent must hit network to sanity-check | `validate_event`, `calendar_capabilities` | failing RT |
| A09 | Medium | Test | n/a | No native fuzz targets | Parser/path edge cases unfuzzed | Fuzz suites | fuzz smoke |
| A10 | Medium | Test | n/a | No MCP stdio/in-memory E2E | Registration/RO drift possible | harness + register tests | E2E |
| A11 | Low | Network | retry.go | Retry-After not hard-capped below maxDelay for huge values | Long sleep within tool timeout | already capDelay(max 10s) | unit (existing) |
| A12 | Info | Compat | config | DEFAULT_TZ default UTC not Europe/Paris | Documented intentional compat | Keep UTC; migration note | docs |

## Decision: language and commits

AGENTS.md open-source rules win: all tracked code, docs, errors, and commits stay **English**. No em dash. No AI trailers. User-facing structured error *messages* remain English (stable codes for agents).

## Decision: DEFAULT_TZ

Keep default `UTC` for backward compatibility. Document that operators should set `ICLOUD_MCP_DEFAULT_TZ=Europe/Paris` (or owner TZ). Not a silent default change.

## Post-implementation status

| ID | Status |
|----|--------|
| A01 get_event | Fixed |
| A02 free slots | Fixed |
| A03 delete If-Match | Fixed |
| A04 series/occurrence | Fixed (this-and-future not shipped) |
| A05 error codes | Fixed |
| A06 enriched create | Fixed |
| A07 search filters | Fixed |
| A08 validate/capabilities | Fixed |
| A09 fuzz | Fixed (smoke green after redactor iteration) |
| A10 MCP E2E | Fixed (in-process) |
| A11 Retry-After | Already capped |
| A12 DEFAULT_TZ | Documented, unchanged |

## Residual risks

- `this-and-future` recurrence scope not implemented (safety).
- Wide-scan UID fallback still misses events outside ±10 years (documented in
  `docs/caldav-compatibility.md`).
- Real iCloud integration not run in CI (manual runbook in `docs/testing.md`;
  operator smoke via live MCP host + `go test -tags=integration`).
- Occurrence updates on non-recurring masters are rejected (require RRULE).

## Follow-up audit remediation (2026-07-24, fourth pass)

| ID | Severity | Status |
|----|----------|--------|
| F1 gofmt Event.ETag | Medium (CI) | Fixed |
| F2 UID window ±5y | Medium | Fixed (±10y) |
| F5 main wiring tests | Low | Fixed (timeouts, redactor set, RO register) |
| F9 rate limit burns tool timeout | Low | Fixed (2s fail-fast `rate_limited`) |
| Retry PUT body rewind | Low | Fixed (`GetBody` each attempt) |
| Integration + live MCP smoke | Ops | Verified (real iCloud + MCP host smoke) |

## Follow-up audit (2026-07-24)

| ID | Status |
|----|--------|
| Boot config / `file://` no path or email in errors | Fixed |
| Discovery port revalidation on prod iCloud hosts | Fixed |
| Create `If-None-Match: *` | Fixed |
| CONTRIBUTING + main timeout tests | Fixed |
| golang image digest pin + local lint via `go run` | Fixed |
| GitHub secret scanning + private vuln reporting | Enabled (ops) |

## Hostile audit follow-up (2026-07-24, second pass)

| ID | Severity | Status |
|----|----------|--------|
| M1 Series DELETE fail-closed without ETag | Medium | Fixed |
| M2 Working-tree hardening commit | Medium | Fixed (this commit) |
| M3 Document `deletedTitle` on MCP success | Medium | Fixed (docs) |
| L1 Basic-auth base64 RawStd/URL/RawURL redaction | Low | Fixed |
| L3 `ServerVersion` via Deps (no global) | Low | Fixed |
| L5 AGENTS layout tool count | Low | Fixed (local) |

## Full audit remediation (2026-07-24, third pass)

| ID | Severity | Status |
|----|----------|--------|
| H1 Occurrence start-only DTEND | High | Fixed (`applyOccurrenceUpdate` keeps duration) |
| H2 Expose `recurrenceId` / `isOverride` | High | Fixed (search DTO + get_event overrides) |
| H3 Recurring create TZID+VTIMEZONE | High | Fixed (`buildEventCalendar` + DEFAULT_TZ fallback) |
| M1 Reject `etag=*` | Medium | Fixed (`ValidateIfMatchETag`) |
| M2 Bind paths to home-set | Medium | Fixed (`validateAgentCalendarPath`) |
| M3 Search etag from REPORT | Medium | Fixed |
| M4 All-day `recurrence_id` date-only | Medium | Fixed (`ParseRecurrenceID`) |
| M5 Clear RRULE on expanded rows | Medium | Fixed |
| M6 Multi-cal partial failure | Medium | Fixed (soft warnings; auth hard-fail) |
| M7 Fair multi-cal cap | Medium | Fixed (query all, cap after sort) |
| L1 Password-only base64 redaction | Low | Fixed |
| L3 EXDATE dedupe on re-delete | Low | Fixed |
| L4 REPORT href path validation | Low | Fixed |
| L5 UID backslash/controls | Low | Fixed |
