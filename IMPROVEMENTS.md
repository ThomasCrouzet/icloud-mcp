# Improvement priorities

Keep the domain and mutation contracts in [Architecture](docs/architecture.md)
and [Testing](docs/testing.md).

## P1: Retain executable protocol evidence

- Component: `cmd/icloud-mcp/bounded_stdio.go` and startup wiring.
- Benefit: verify framing, capability registration, and cancellation together.
- Completion: a fixture-only executable harness records initialization, denied
  writes, oversized frames, cancellation, and clean shutdown.
  Production endpoint allowlists must remain fixed.

## P1: Define ambiguous idempotency outcomes

- Component: `internal/mcptools/idempotency.go` and mutation handlers.
- Benefit: clarify how callers reconcile an unknown outcome before a new claim.
- Completion: protocol scenarios retain success, conflict, cancelled waiter,
  and ambiguous-dispatch responses without automatically replaying mutations.

## P2: Expand combined recurrence fixtures

- Component: `internal/icloud/recurrence.go`, `freeslots.go`, `client.go`.
- Benefit: verify interaction between overrides, DST, all-day events, and limits.
- Completion: retained calendar artifacts show exact occurrences and free slots
  for combined fixtures, including truncated and rejected workloads.

## P2: Verify domain failure isolation through MCP

- Component: `internal/mcptools/register.go`, Contacts and Mail adapters.
- Benefit: show that one failed optional domain does not change another domain.
- Completion: a local protocol run keeps Calendar usable after Contacts or Mail
  failures and proves that disabled mutation tools remain absent.
