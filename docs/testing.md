# Testing

## Commands

```bash
make test                 # go test ./... -race -cover
make lint                 # go vet + golangci-lint (pinned)
go test ./... -count=1
go test ./... -race -count=1
go test ./... -race -shuffle=on -count=10
go test ./internal/icloud/ -run=^$ -fuzz=FuzzValidateUID -fuzztime=10s
make release              # static linux/arm64 via pinned golang:1.25 container
```

## Layers

| Layer | Location | Purpose |
|-------|----------|---------|
| Unit | `*_test.go` | validation, free slots, recurrence, redaction |
| Mock CalDAV | `internal/icloud` mock server | discovery, REPORT, PUT, DELETE, ETag, 412 |
| Mock Service | `MockService` | MCP handlers without network |
| Generative | free-slot seeds | interval merge invariants |
| Fuzz | `Fuzz*` targets | paths, UIDs, RRULE, redaction, hosts |
| Contract | `schema_contract_test.go` | schema bounds vs runtime constants |
| MCP E2E | in-process client | tools/list RO/RW, capabilities |
| Integration | `integration_test.go` tag `integration` | real iCloud (manual credentials) |

## Properties tested for free slots

- No free slot overlaps merged busy intervals.
- Slot duration equals requested duration.
- Slots sorted and within range.
- TRANSPARENT and CANCELLED ignored.
- All-day optional via `include_all_day_busy`.

## Dry-run proof

`delete_event` with `dry_run=true` must leave `MockService.RecordedMutations`
empty (no PUT/DELETE recorded).

## CI gates

`.github/workflows/ci.yml`:

- gofmt, go vet, golangci-lint v2.1.6
- `go test -race` with coverage threshold (78%)
- govulncheck, `go mod verify` + tidy diff
- fuzz smoke (icloud + security targets, 3s each)
- security greps: no `os/exec`, no unauthorized URLs, no production
  `InsecureSkipVerify`
- multi-arch build (linux/amd64 + linux/arm64), 20 MiB size budget
- gitleaks on the working tree

## Real iCloud

Never run in CI. Requires explicit credentials and preferably a disposable
calendar. Without credentials, integration is honestly "not run".

### Manual runbook

1. Create an **app-specific password** on
   [appleid.apple.com](https://appleid.apple.com) (Sign-In and Security →
   App-Specific Passwords). Prefer a dedicated Apple ID or a disposable calendar.
2. Export credentials for one shell session only (never commit them):

   ```bash
   export ICLOUD_EMAIL='you@icloud.com'
   export ICLOUD_PASSWORD='xxxx-xxxx-xxxx-xxxx'
   export ICLOUD_MCP_READ_ONLY=1
   export ICLOUD_MCP_DEFAULT_TZ='Europe/Paris'
   ```

   Or use `file://` secrets mounted read-only:

   ```bash
   export ICLOUD_EMAIL='file:///run/secrets/icloud-email'
   export ICLOUD_PASSWORD='file:///run/secrets/icloud-password'
   ```

3. Build and run the integration package (build tag `integration`):

   ```bash
   go test -tags=integration -count=1 -v -timeout=120s .
   ```

4. Optional smoke against a live MCP host: start with `ICLOUD_MCP_READ_ONLY=1`,
   call `list_calendars` then `search_events` on a short window, then lift
   read-only only if write tools are needed. Revoke the app-specific password
   when finished.

5. Expected failures without credentials: tests skip or fail fast at discovery
   with `authentication_refused` (401). Do not disable TLS or the allowlist
   to "make it pass".
