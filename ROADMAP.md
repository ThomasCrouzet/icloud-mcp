# icloud-mcp Roadmap

**Status:** Calendar + optional Contacts + optional Mail are production-ready in
v0.3.x. This file tracks agent maturity work and longer-horizon items.

**Updated:** 2026-07-25

## Phase 1: Agent maturity (shipped)

| Item | Status |
|------|--------|
| 1.1 Error codes and retry semantics | Done: structured `code` / `retryable` / `retry_after_seconds`; catalog in `docs/error-codes.md`; agent simulation tests |
| 1.2 Timezone wall-clock clarity | Done: timed MCP times use RFC3339 with explicit offset in `ICLOUD_MCP_DEFAULT_TZ`; `calendar_capabilities.outputFormat` |
| 1.3 Idempotency keys | Done: `create_event` `client_uid`/`idempotency_key`; `update_event` process-local key; Contacts create alias + update key |
| 1.4 Mutation audit JSON | Done: slog NDJSON default; `-audit-format=json\|text` |
| 1.5 Health check improvements | Done: `/healthz` and `/status` JSON with version, domains, multi-domain rate limits |

## Phase 2: Feature completeness

| Item | Status |
|------|--------|
| 2.1 Mail domain (read + mutation + send) | Done (v0.3.0). CONDSTORE flag writes remain blocked on go-imap beta.8. |
| 2.2 Contacts domain | Done (v0.3.0). `hasPhoto` metadata only; PHOTO bytes never exposed (size/PII). |
| 2.3 Reminders (CalDAV VTODO) | Deferred. Modern Apple Reminders are not a stable third-party CalDAV VTODO surface; collections are filtered out deliberately. Revisit if Apple documents a remote connector. |
| 2.4 Multi-account | Documented: one process per identity. Hosts spawn N processes. |

## Phase 3: Polish and governance

| Item | Status |
|------|--------|
| 3.1 Published roadmap | This file |
| 3.2 Support expectations | `CONTRIBUTING.md` |
| 3.3 Release checksums + signing | SHA-256 checksums in `make release*`; tag releases gated on green CI; cosign keyless signatures required on GitHub release blobs |
| 3.4 Agent-specific docs | `docs/agent-hosts.md` |

## Success criteria

- Agents can create/update/delete without blind retries (codes + idempotency).
- Timed event times always carry an explicit offset matching DEFAULT_TZ.
- Health JSON exposes domains and rate-limit tokens.
- Mutation audit is machine-readable JSON by default.
- Mail and Contacts optional domains are stable behind capability gates.

## Known risks

| Risk | Mitigation |
|------|------------|
| go-imap v2 beta.8 CONDSTORE | Fail closed on flag writes when CONDSTORE is advertised. |
| Apple CalDAV/IMAP policy changes | Document in CHANGELOG immediately. |
| Agent expectation of Reminders | FAQ: out of scope until a documented remote API exists. |
| Rate limits under heavy agents | Health `rateLimits` + structured `rate_limited`. |

Community input: GitHub issues labeled `phase-1`, `phase-2`, or `phase-3` when
opened.
