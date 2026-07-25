# Security implementation

This document describes the implementation behind the public threat model in
[SECURITY.md](../SECURITY.md).

## One process and domain boundaries

Calendar, Contacts, and Mail can hold credentials in one address space. A memory
disclosure or arbitrary-code defect can cross those in-process boundaries. The
implementation limits accidental credential/transport crossover, not a complete
process compromise.

Each domain owns an immutable credential copy, destination policy, transport or
dialer, limiter, semaphore where applicable, retry policy, protocol service, and
package boundary. Calendar and Contacts never share an authenticated HTTP
client. IMAP and SMTP accept only fixed-destination dial functions. Feature flags
prevent disabled optional clients from being constructed.

## Network allowlists

| Client | Allowed destination | Port | Transport |
|--------|---------------------|------|-----------|
| Calendar | `caldav.icloud.com`, `p[0-9]{1,3}-caldav.icloud.com` | implicit or explicit 443 | HTTPS |
| Contacts | `contacts.icloud.com`, `p[0-9]{1,3}-contacts.icloud.com` | implicit or explicit 443 | HTTPS |
| IMAP | `imap.mail.me.com` | 993 only | Implicit TLS |
| SMTP | `smtp.mail.me.com` | 587 only | TCP upgraded by mandatory STARTTLS |

Host matching is case-sensitive equality against the fixed lowercase production
literals (for example `caldav.icloud.com` and `p12-caldav.icloud.com`). Hosts
are not lowercased before comparison, so mixed-case variants are rejected.
Production destinations are not configurable. Literal disallowed hosts, schemes,
ports, or socket addresses are rejected before the production dialer performs
DNS resolution.

DAV uses `AllowlistTransport` with `Proxy: nil`, verified system roots, and TLS
1.2 minimum. `InsecureSkipVerify` is never set. The Calendar HTTP client
revalidates HTTPS, host, and port on redirected requests. Calendar discovery
also validates principal and home-set authorities before retaining a shard.

Contacts disables automatic redirects and follows only read-side 301, 302, 307,
and 308 for at most three hops. It resolves relative `Location` values against
the response URL, retains the original method/replayable body, and revalidates
every read hop. It rejects read-side 303 and all other redirect statuses. A
redirect after PUT or DELETE is never replayed and returns `outcome_unknown`.
Every principal, home set, address book, REPORT href, GET target, and mutation
target is independently validated against the Contacts policy and collection
boundary.

The IMAP dialer requires exactly `tcp` and `imap.mail.me.com:993`, then completes
a verified TLS handshake with fixed `ServerName=imap.mail.me.com` before the
protocol adapter receives the connection. SMTP requires exactly `tcp` and
`smtp.mail.me.com:587`; authentication is unavailable until the SMTP adapter has
completed mandatory verified STARTTLS with fixed
`ServerName=smtp.mail.me.com`.

Tests inject fake HTTP doers or dialers. Injection does not make production
hosts configurable.

## Credentials and redaction

Calendar and Contacts use separate credential objects populated from
`ICLOUD_EMAIL` and `ICLOUD_PASSWORD`. Mail uses the full
`ICLOUD_MAIL_ADDRESS` and `ICLOUD_MAIL_PASSWORD`, or a distinct copy of
`ICLOUD_PASSWORD` when the dedicated Mail password is unset.

For every enabled credential pair, `RedactionVariants` registers:

- Raw username and password.
- Query-escaped and path-escaped forms.
- Username-only and password-only Base64 in Std, RawStd, URL, and RawURL forms.
- `username:password` and its four Base64 forms.
- SASL PLAIN NUL-separated values, with and without username as authorization
  identity, and their four Base64 forms.

Insertion points are:

1. `RedactingWriter` for all stderr, structured logs, stdlib logs, and audit.
2. Calendar `errResult`, Contacts error/result writers, and Mail error/result
   writers.
3. Success payload serialization for every domain.
4. `RecoverRedactMiddleware` before panic text can reach JSON-RPC stdout.

Configuration and credential-load errors occur before the production redactor
exists, so their messages never include identity, secret, invalid Mail address,
recipient value, or file path. Raw DAV XML, IMAP tagged text, SMTP replies, and
MIME parser errors are mapped to local bounded messages.

## `file://` operator boundary

`ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MAIL_ADDRESS`, and
`ICLOUD_MAIL_PASSWORD` support `file://`. Mail values are read only when Mail is
enabled. The process accepts only a regular file of at most 4 KiB, reads it once
at boot, trims surrounding whitespace, and retains only the value. FIFOs,
devices, directories, and oversized files are rejected.

The operator who controls the environment is trusted to select the file. There
is no chroot, base-directory allowlist, or symlink guarantee. An empty path and a
path component exactly equal to `..` are rejected as footgun guards. Read errors
report only `not_found`, `permission_denied`, or `unreadable`, never the path.
There is no disk access after boot.

## Read-only and capability gates

The global switch removes handlers rather than returning a runtime disabled
error:

- Calendar: `create_event`, `update_event`, `delete_event` are absent.
- Contacts: `create_contact`, `update_contact`, `delete_contact` are absent.
- Mail: `set_message_flags`, `move_message`, `trash_message`, and
  `send_message` are absent.

Contacts read requires its enable flag. Mail read requires its enable flag. Mail
mutation additionally requires the Mail write flag. Mail send independently
requires the send flag and a valid SMTP recipient policy. Mail read does not
grant mutation or send, and Mail mutation does not grant send.

Global read-only suppresses configured writes but does not waive configuration
validation. In particular, requested Mail send still requires a recipient
allowlist at boot.

## SMTP recipient authorization

`ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS` is either literal `*` or a comma-separated
set of unique exact plain addr-specs. Matching trims surrounding configuration
spaces and uses ASCII case-insensitive full-address equality. Display names,
groups, empty entries, partial wildcards, domain-only rules, and suffix rules are
invalid.

`send_message` parses and de-duplicates the complete To/Cc/Bcc set, applies the
allowlist, validates subject/body limits, and builds the bounded message before
opening a socket. `to`, `cc`, and `bcc` are each optional, but at least one
recipient is required across them. From is always the configured Mail address.
Bcc is excluded from headers. This policy limits authorized recipients, but
literal `*` removes that limit.

SMTP sends every RCPT command and starts DATA only if all recipients received a
definitive acceptance. Any RCPT rejection prevents partial submission. SMTP is
never automatically retried. A non-definitive failure after DATA may have been
transmitted returns `outcome_unknown`; callers must inspect Sent and recipients
before considering another send.

## Untrusted remote content and output caps

Calendar text, contact fields, mailbox metadata, headers, message bodies, and
attachment names are untrusted remote data. Prompt-injection labels in tool
descriptions/results are advisory and are not a security boundary.

The implementation therefore restricts data shape and size:

- The stdio transport accepts at most 1 MiB per JSON-RPC frame. A generated
  protocol/schema error record that could reflect caller input is emitted only
  through 64 KiB; a larger record is replaced with a bounded local error. Every
  serialized MCP result, including Calendar results, is capped at 256 KiB.
- Calendar search has range, result, recurrence, field, PROPFIND, and REPORT
  limits. REPORT XML is bounded to depth 32, 262,144 tokens, 4,096 responses,
  16,384 propstats, and 32,768 properties. Parsed iCalendar is bounded to 1,024
  components, 10,000 properties total, 1,024 properties per component, 512
  overrides, 64 parameters per property, 64 alarms, and 2,000 EXDATE values.
  A Calendar search materializes at most 10,000 events. Recurrence expansion
  returns at most 2,000 occurrences and performs at most 100,000 iterator
  advances per series and 250,000 across one search, with preflight work
  rejection.
  `find_free_slots` exposes no busy-event content.
- Contact search returns summaries without notes, raw vCards, PHOTO bytes, or
  raw extension properties. Full get returns modeled fields and bounded notes.
  One vCard is capped at 1 MiB; a search scans at most 2,000 cards and 32 MiB;
  Contacts results are capped at 256 KiB. DAV XML is bounded to depth 32,
  100,000 tokens, 8,192 propstats, and 16,384 properties; one vCard is limited
  to 10,000 properties.
- Mail search returns envelope metadata without snippets or bodies. Message get
  returns curated headers, at most one bounded decoded plain-text part, and
  attachment metadata. It excludes raw headers, raw MIME, HTML, and attachment
  payloads.
- IMAP input is guarded at 4 MiB per session, 1 MiB per protocol line, protocol
  depth 24, and 512 protocol lists before recursive decoding. Modeled MIME is
  capped at 200 parts/depth 20, selected-part headers at 64 KiB, body wire bytes
  at 512 KiB, decoded text at 200 KiB, and serialized Mail results at 256 KiB.
- SMTP accepts at most 50 recipients, a 998-byte subject, 100 KiB plain-text
  body, and 256 KiB encoded message. Aggregate inbound SMTP responses are capped
  at 1 MiB per session.

Truncation happens only at complete result-object or valid UTF-8 boundaries.
When a message body exceeds its selected cap or cannot be decoded safely, useful
metadata is returned with `bodyOmitted` and a warning. If bounded metadata itself
cannot fit, the tool returns `payload_too_large`.

## Consistency and mutation safety

### Calendar and Contacts ETags

- Calendar/Contacts create sends `If-None-Match: *`; HTTP 412 maps to
  `conflict`.
- Update/delete first GETs the complete resource and validates a strong ETag.
- A supplied caller ETag takes precedence; otherwise the GET ETag is used.
- Missing, wildcard, weak, malformed, or unusable ETags fail closed.
- Every real PUT/DELETE sends a specific `If-Match`; HTTP 412 maps to
  `concurrent_modification`.
- Contacts re-GETs after successful create/update for normalized metadata. A
  failed follow-up GET returns known success with `resultIncomplete`, not
  `outcome_unknown`.

### Mail UIDVALIDITY and MODSEQ

A Mail message is identified by `(mailbox, UIDVALIDITY, UID)`. Search cursors
must pair `before_uid` with UIDVALIDITY. Get and every mutation select the named
mailbox and reject a UIDVALIDITY mismatch before acting.

Reads request MODSEQ when CONDSTORE is advertised. Safe conditional STORE
requires detecting the tagged MODIFIED response. The current go-imap beta.8
adapter reports that it cannot provide this guarantee, so
`set_message_flags` rejects before STORE with `protocol_error` on CONDSTORE
servers. It does not report `concurrent_modification` on this unavailable path.
When CONDSTORE is absent, it uses only delta `+FLAGS.SILENT` or
`-FLAGS.SILENT` for Seen, Flagged, and Answered and returns
`conditionalUpdate: false`.

Move uses native UID MOVE when available. Otherwise it requires UIDPLUS and uses
sequential UID COPY, add Deleted, and UID EXPUNGE for the one UID. Plain EXPUNGE
and mailbox-wide EXPUNGE are not exposed. Failures after a completed step return
`partial_failure` or `outcome_unknown` with reconciliation guidance. Trash
requires exactly one selectable SPECIAL-USE Trash mailbox and exposes no
permanent delete.

## Retries, rates, and deadlines

All tool handlers have a 25 second deadline. DAV HTTP timeout is 30 seconds.
Calendar boot discovery is 20 seconds; Contacts lazy discovery is at most 10
seconds within the tool deadline.

Calendar:

- HTTP status 429, 502, 503, and 504 is retried up to 6 total attempts with a
  rewindable read request body and bounded `Retry-After`/backoff.
- `GuardedService` additionally retries only non-classified transient reads, at
  most twice.
- No PUT or DELETE is replayed, including full-series delete. A transport error
  or gateway 502/503/504 after mutation dispatch returns `outcome_unknown`; a
  mutation-side 429 is a definitive `rate_limited` result.
- Read/write rates are 60/20 per minute with bursts 10/3 and concurrency 4/2.
  Local waits over two seconds fail fast.

Contacts:

- Safe reads retry 429, 502, 503, and 504 for at most 3 total attempts, with a
  maximum 2 second delay.
- PUT/DELETE and transport-ambiguous writes are never replayed.
- Read/write rates are 60/20 per minute with bursts 10/3 and at most 4 concurrent
  DAV requests.

Mail:

- A transient read may create one replacement IMAP session before returning a
  result. Each attempt consumes the Mail read rate budget.
- IMAP mutation and SMTP send have no automatic retry.
- Rates are 60 reads, 20 mutations, and 20 sends per minute, with bursts 10/3/3.
  Concurrency is 2 read sessions, 1 mutation, and 1 send.

## Audit

All mutation handlers emit one-line records to redacted stderr. Default format is
JSON NDJSON (`-audit-format=json`). Operators may select plain text with
`-audit-format=text`.

Every production Calendar, Contacts, IMAP, and SMTP mutation uses the unified
shape: tool, `domain`, `resourceType`, process-local opaque HMAC
`resourceToken`, and status. Tokens are stable only for one process and cannot
be correlated across restarts. Calendar hashes its path/UID tuple before the
record is emitted; no raw Calendar path or UID is logged. Raw contact UIDs,
mailbox names, UIDVALIDITY/UID tuples, Message-IDs, and submission recipients
are also absent. Calendar title, location, notes, and `deletedTitle` are never
included. Allowed statuses are `success`, `error`, `denied`, `dry_run`, and
`outcome_unknown`.

## Structured errors

Unified Contacts and Mail error codes are:

`validation`, `authentication`, `authorization`, `not_found`, `conflict`,
`concurrent_modification`, `rate_limited`, `timeout`, `unavailable`,
`partial_failure`, `protocol_error`, `payload_too_large`, `outcome_unknown`, and
`internal_error`.

Calendar maps its established internal classifications to the same public
categories where applicable. Errors contain bounded local text, optional retry
metadata (`retryable`, `retry_after_seconds`), and operation-specific
reconciliation for ambiguous outcomes. They do not include raw HTTP/XML, IMAP,
SMTP, MIME, identity, password, path-to-secret, or recipient-policy values.

Agent-facing examples and retry policy: [error-codes.md](error-codes.md).
