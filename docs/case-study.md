# Case study: one stdio boundary for iCloud Calendar, Contacts, and Mail

**Status:** technical case study. Synthetic and CI evidence only. No live
iCloud compatibility claim. No MCPB host installation claim.

## Problem

An MCP host needs Calendar, Contacts, and Mail access on a headless machine.
macOS frameworks, browser automation, and private Apple APIs are not available.
The server must speak CalDAV, CardDAV, IMAP, and SMTP directly, keep credentials
out of results, and make dangerous mutations hard to trigger by accident.

## Shape of the server

- One Go binary, one stdio process, one MCP server.
- Calendar is always available. Contacts, Mail read, Mail mutation, and Mail
  send are independent capabilities with separate clients and credentials.
- Each domain has its own transport, TLS policy, dialer, rate limiter,
  semaphore, and destination allowlist. A shared authenticated client would
  cross a credential boundary.
- Egress is fixed per domain: iCloud CalDAV hosts and validated shards,
  CardDAV hosts and shards, `imap.mail.me.com:993`, and
  `smtp.mail.me.com:587` with STARTTLS.
- The process writes no file after boot. Disk reads are limited to boot-time
  `file://` secrets: a regular file of at most 4 KiB, mode `0600` or stricter.

## Request boundary

The host starts the process and exchanges newline-delimited JSON-RPC on stdin
and stdout. The server bounds each frame to 1 MiB, each result to 256 KiB, and
each tool call to 25 seconds. It replaces oversized protocol errors and never
writes logs to stdout. Credentials, Basic/SASL variants, and configured
identities are redacted from stderr, errors, results, and recovered panics.

Mutation audits carry `domain`, `resourceType`, and a process-local HMAC
`resourceToken`. They never contain a raw path, UID, mailbox, Message-ID, or
recipient.

## Mutation safety

Calendar and Contacts writes use conditional requests. A create needs
`If-None-Match: *`; an update or delete needs a strong `If-Match`. A missing
ETag fails closed. A dispatched mutation is never replayed: an ambiguous result
returns `outcome_unknown` with reconciliation steps.

Group-scheduled Calendar objects need more than an ETag. An object with
`ORGANIZER` or `ATTENDEE` is denied by default. The approved path requires all
of:

1. An explicit operator grant in `ICLOUD_MCP_PERMISSIONS`.
2. `acknowledge_scheduling=true` on the call.
3. The current strong ETag from `get_event`.

The service rechecks scheduling metadata on its final GET. A changed object
invalidates the approval. A deletion dry run reports the risk without exposing
an attendee address.

Mail keeps read, mutation, and send as separate gates. Send requires an exact
recipient policy, validates every recipient before SMTP, sends all `RCPT`
before `DATA`, and never retries. IMAP reads use `EXAMINE` and `PEEK`; plain or
mailbox-wide `EXPUNGE` is rejected and guarded.

## Calendar time semantics

Floating iCalendar values stay floating. The result sets `isFloating` and uses
an offset-free local value; the server does not invent an offset. Availability
interprets a floating value in the requested timezone.

Zoned recurrences expand on the wall clock. A spring-gap occurrence uses the
pre-gap UTC offset that RFC 5545 section 3.3.5 describes and sets
`dstAdjusted`. A fold resolves to the first occurrence. An EXDATE whose value
type cannot match DTSTART fails closed before expansion.

## Evidence

| Area | Evidence |
|---|---|
| Race and unit tests | `go test ./... -race` in CI, with package coverage floors |
| MCP contract | Schema, dispatch, capability manifest, denial, redaction, and result-budget suites |
| Executable protocol | `scripts/protocol_evidence.py`: raw stdio startup, discovery, bounded frames, cancellation, shutdown, modern `2026-07-28` and legacy `2025-11-25` |
| Calendar safety | Synthetic DAV fixtures for ETags, recurrence, DST, floating time, EXDATE, and group-scheduled denials |
| Mail safety | Synthetic IMAP and SMTP peers for recipient policy, STARTTLS, failure stages, and reconciliation |
| Distribution | Deterministic MCPB builds, manifest validation, native archive and bundle smoke, hash and Cosign verification |
| Release provenance | One candidate build per tag, native-tested on three platforms, then signed and published unchanged |

Local commands:

```bash
make test
make lint
make check
make protocol-evidence EVIDENCE_DIR=/absolute/path/outside/repo
make check-release-metadata
```

## Limitations

- No live iCloud run is recorded for the current revision. Every live matrix
  row stays `UNVERIFIED`.
- No real MCPB desktop host install is recorded. The bundle is built, hashed,
  signed, and natively started, but host settings, secret storage, upgrade, and
  uninstall behavior are unverified.
- Windows is compile-only. The Unix mode-bit secret policy has no validated
  Windows ACL equivalent.
- RDATE and `RECURRENCE-ID;RANGE=THISANDFUTURE` are rejected until live Apple
  evidence exists.
- The MCP host can send returned account data to a model provider. The host and
  provider policies govern that transfer; local stdio does not prevent it.

## Where to look

- [Architecture](architecture.md)
- [Compatibility matrix](compatibility-matrix.md)
- [MCPB bundles](distribution/mcpb.md)
- [Platform support](platform-support.md)
- [Synthetic first-run transcript](demo/read-only-transcript.md)
- [Security policy](../SECURITY.md)
