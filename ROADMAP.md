# icloud-mcp Roadmap

**Status:** Calendar, optional Contacts, and optional Mail are production-ready
in v0.3.x. This file tracks agent maturity work and long-term items.

**Updated:** 2026-07-25

## Phase 1: Agent maturity (shipped)

| Item | Status |
|------|--------|
| 1.1 Error codes and retry semantics | Done. Structured `code`, `retryable`, and `retry_after_seconds`. Catalog in `docs/error-codes.md`. Agent simulation tests. |
| 1.2 Timezone wall-clock clarity | Done. Timed MCP values use RFC3339 with an explicit offset in `ICLOUD_MCP_DEFAULT_TZ`. See `calendar_capabilities.outputFormat`. |
| 1.3 Idempotency keys | Done. `create_event` uses `client_uid` or `idempotency_key`. `update_event` uses a process-local key. Contacts create supports the alias, and update supports the key. |
| 1.4 Mutation audit JSON | Done. slog NDJSON is the default. Use `-audit-format=json\|text` to select the format. |
| 1.5 Health check improvements | Done: `/healthz` and `/status` JSON with version, domains, multi-domain rate limits |

## Phase 2: Feature completeness

| Item | Status |
|------|--------|
| 2.1 Mail domain (read + mutation + send) | Done (v0.3.0). CONDSTORE flag writes remain blocked on go-imap beta.8. |
| 2.2 Contacts domain | Done (v0.3.0). The server exposes only `hasPhoto` metadata. It never exposes PHOTO bytes because of size and PII. |
| 2.3 Reminders (CalDAV VTODO) | Deferred. Modern Apple Reminders do not provide a stable third-party CalDAV VTODO interface. The server filters out these collections. Revisit this item if Apple documents a remote connector. |
| 2.4 Multi-account | Documented: one process per identity. Hosts spawn N processes. |

## Phase 3: Polish and governance

| Item | Status |
|------|--------|
| 3.1 Published roadmap | This file |
| 3.2 Support expectations | `CONTRIBUTING.md` |
| 3.3 Release checksums + signing | `make release*` makes SHA-256 checksums. Tag releases require green CI. GitHub release blobs require cosign keyless signatures. |
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
| Apple CalDAV/IMAP policy changes | Document each change in CHANGELOG immediately. |
| Agent expectation of Reminders | The FAQ states that Reminders are out of scope until Apple documents a remote API. |
| Rate limits under heavy agents | Health reports `rateLimits`, and errors use structured `rate_limited`. |

For community input, open a GitHub issue with the `phase-1`, `phase-2`, or
`phase-3` label.
