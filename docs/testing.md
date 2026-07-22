# Testing

## Commands

```bash
make test                 # go test ./... -race -cover
go test ./... -count=1
go test ./... -race -count=1
go test ./... -race -count=10
go test ./... -race -shuffle=on -count=10
go test ./internal/icloud/ -run=^$ -fuzz=FuzzValidateUID -fuzztime=10s
make release              # static linux/arm64 via golang:1.25 container
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

`delete_event` with `dry_run=true` must leave `MockService.RecordedMutations` empty
(no PUT/DELETE recorded).

## Real iCloud

Never run in CI. Requires explicit credentials and preferably a disposable calendar.
Without credentials, integration is honestly "not run".
