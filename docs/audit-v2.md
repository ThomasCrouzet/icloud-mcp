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
| A08 | Low | Local | — | No offline validate/capabilities | Agent must hit network to sanity-check | `validate_event`, `calendar_capabilities` | failing RT |
| A09 | Medium | Test | — | No native fuzz targets | Parser/path edge cases unfuzzed | Fuzz suites | fuzz smoke |
| A10 | Medium | Test | — | No MCP stdio/in-memory E2E | Registration/RO drift possible | harness + register tests | E2E |
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
- Wide-scan UID fallback still misses events outside ±5 years.
- Real iCloud integration not run without credentials.
- Occurrence updates on non-recurring masters still create a RECURRENCE-ID override (server may accept or reject).
