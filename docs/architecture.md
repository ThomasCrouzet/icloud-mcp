# Architecture

## Overview

`icloud-mcp` is one stdio MCP server for Apple/iCloud Calendar, Contacts, and
Mail. It runs as a child process of any MCP-compatible host, speaks JSON-RPC on
stdin/stdout, takes configuration only from the process environment, and
produces one release artifact. It does not embed or prefer a particular model
vendor or agent product. Calendar is always enabled; Contacts, Mail read, Mail
mutation, and Mail send are composable capabilities.

The protocol boundary is deliberately narrow:

- Calendar uses CalDAV over HTTPS.
- Contacts uses CardDAV over HTTPS.
- Mail reads and mutations use IMAP over implicit TLS.
- Mail send uses authenticated SMTP submission with mandatory STARTTLS.

No private Apple API, browser automation, local Apple framework, local synced
database, external protocol executable, plugin, or runtime-downloaded code is
used.

## Boot and lifecycle

1. `config.Load` reads and validates the 12-variable environment contract. It
   resolves boot-only `file://` values for configured identities/passwords
   (regular file, at most 4 KiB, mode 0600 or stricter). Configuration failure
   occurs before network access. Literal SMTP recipient policy `*` is accepted
   when send is requested and emits a boot warning on stderr.
2. A shared `security.Redactor` is built from the enabled credential pairs,
   including Basic-auth and SASL PLAIN encodings. All stderr and stdlib logging
   is redirected through `RedactingWriter`.
3. One immutable `CapabilityPlan` applies global read-only and the optional
   domain gates. It is the source of both tool registration and
   `icloud_capabilities` output.
4. Separate Calendar, Contacts, IMAP, and SMTP transports/dialers are built only
   for enabled capabilities. Calendar and Contacts receive distinct copied
   credential objects and authenticated HTTP clients.
5. Calendar performs eager two-step CalDAV discovery under a 20 second boot
   deadline. Failure prevents stdio from starting.
6. Contacts performs no boot network access. Its first call runs discovery under
   a concurrency-safe gate with a 10 second attempt deadline. Only a complete,
   validated success is cached; failure is not cached.
7. Mail performs no boot network access. Every read, mutation, or send attempt
   creates and closes a fresh authenticated protocol session. A transient read
   may make one replacement-session attempt; mutations and SMTP never retry.
8. MCP handlers register from the finalized plan. Optional loopback-only
   `-health` starts, then `ServeStdio` owns stdin/stdout.

After boot, a Contacts, IMAP, or SMTP failure affects only that tool call. It
does not unregister tools, poison a successful Contacts discovery cache, or
alter another domain client.

## Packages

| Package | Role |
|---------|------|
| `cmd/icloud-mcp` | Configuration wiring, domain construction, eager Calendar discovery, capability plan, timeouts, stdio |
| `internal/config` | Strict booleans, environment validation, `file://` secrets (0600+, 4 KiB), Mail recipient policy |
| `internal/security` | DAV and socket allowlists, TLS policy, redaction, process-local audit tokens |
| `internal/icloud` | Calendar CalDAV, iCalendar, recurrence, free slots, validation, Calendar retry/rate policy; per-calendar search materialization 2,500; multi-calendar filtered materialization 10,000; imported-UID REPORT +/-50y |
| `internal/contacts` | Lazy CardDAV discovery, bounded DAV/XML, vCard model, search, conditional writes |
| `internal/mail` | Mail service, MIME handling, IMAP mutation policy, SMTP submission |
| `internal/mail/imapadapter` | Narrow beta go-imap boundary and decode-time protocol guard |
| `internal/mcptools` | Compositional schemas, handlers, capability reporting, redacted results, mutation audit |
| `internal/health` | Optional loopback-only `/healthz` and `/status` with version, domain enablement, and multi-domain rate limits |

`internal/icloud` retains its historical name but is scoped to Calendar. Shared
code may provide pure redaction, audit formatting, result sizing, and limiter
primitives; it does not own a cross-domain authenticated client.

## Capability composition

The complete surface is grouped as follows:

| Capability group | Read/local tools | Mutation tools |
|------------------|------------------|----------------|
| Global | `icloud_capabilities` | none |
| Calendar | `list_calendars`, `search_events`, `get_event`, `find_free_slots`, `validate_event`, `calendar_capabilities` | `create_event`, `update_event`, `delete_event` |
| Contacts | `list_address_books`, `search_contacts`, `get_contact` | `create_contact`, `update_contact`, `delete_contact` |
| Mail read | `list_mailboxes`, `search_messages`, `get_message` | none |
| Mail mutation | none | `set_message_flags`, `move_message`, `trash_message` |
| Mail send | none | `send_message` |

Default registration is 10 tools: 9 Calendar tools plus the global capability
tool. Global read-only with optional domains disabled is 7 tools. The complete
surface is 23 tools. Disabled tools have no handler and disabled optional domains
have no client.

`icloud_capabilities` is local and generated from the same immutable plan used
for registration. It reports version, global read-only state, healthcheck state,
configured domains, effective capability groups, sorted tool names, and count.
It exposes no identity, secret, host, shard, path, mailbox, recipient, or runtime
error. `calendar_capabilities` remains Calendar-specific.

## Domain request paths

### Calendar

```text
tool -> 25s context -> Calendar read/write limiter -> 4/2 semaphore -> retry policy
     -> Calendar Basic-auth client -> Calendar allowlist -> verified HTTPS
```

Calendar discovery is eager and successful state is immutable. Reads have
bounded service retries. The HTTP classifier retries 429, 502, 503, and 504 with
bounded `Retry-After`/backoff and rewinds read request bodies; it does not retry
transport errors. PUT and DELETE are never replayed, including full-series
delete. A transport failure or gateway 502/503/504 after mutation dispatch maps
to `outcome_unknown`. Update and delete re-read full objects and use conditional
requests. Multi-calendar `search_events` queries every selected calendar, then
sorts and applies the fair 400-event return cap; filtered materialization above
10,000 events fails closed with `payload_too_large` (per-calendar REPORT
materialization is 2,500). Imported-UID REPORT fallback uses a +/-50-year window
around now when `<uid>.ics` is missing.

### Contacts

```text
tool -> 25s context -> lazy discovery -> Contacts read/write limiter
     -> 4-request semaphore -> Contacts Basic-auth client
     -> Contacts allowlist -> verified HTTPS
```

Discovery resolves current-user-principal, one or more address-book home sets,
and up to 100 books. Validated collection URLs are pinned behind opaque book
identifiers. Reads can retry bounded HTTP status failures. PUT and DELETE are
never replayed, and transport ambiguity maps to `outcome_unknown`.

Contact search uses CardDAV text predicates as a bounded prefilter when their
semantics match. A general query uses any-of FN/N/EMAIL/TEL/ORG; when no general
query is supplied, email uses EMAIL. Every supplied query, email, phone, and
group condition is then combined locally. Phone matching normalizes digits, so a
phone-only search uses a bounded all-card VERSION-presence query instead of a
server TEL predicate.

### Mail read and mutation

```text
tool -> 25s context -> Mail limiter/semaphore -> fixed IMAP dial
     -> verified implicit TLS -> login -> LIST or SELECT -> bounded commands
     -> logout/close
```

Read operations use EXAMINE and PEEK. Search scans bounded descending UID
windows. Message retrieval fetches metadata, curated headers, BODYSTRUCTURE, and
at most one selected plain-text MIME section. A guarded connection enforces the
4 MiB inbound session budget and protocol nesting/list caps before go-imap can
materialize a recursive BODYSTRUCTURE.

Mutation sessions reselect the source mailbox read-write and compare
UIDVALIDITY before mutation. Only one mutation executes at a time. Mutation
commands are never automatically retried.

### Mail send

```text
tool -> local input/recipient/message validation -> 25s context
     -> send limiter/semaphore -> smtp.mail.me.com:587 -> EHLO
     -> mandatory STARTTLS -> verified TLS -> EHLO -> AUTH
     -> MAIL FROM -> every RCPT TO -> DATA only if all recipients succeeded
```

The encoded message is built in bounded memory before connecting. From is the
configured Mail address. `to`, `cc`, and `bcc` are individually optional, with
at least one aggregate recipient required. Bcc exists only in the envelope. No
SMTP session is retained and no stage is retried.

## State and concurrency

MCP handlers may run concurrently. Shared mutable state is limited and
concurrency-safe:

- Calendar successful discovery state, independent rate buckets, and 4/2
  read/write semaphores.
- Contacts successful lazy-discovery state, independent rate buckets, and a
  four-request semaphore.
- Mail independent read/mutation/send buckets and 2/1/1 semaphores.
- Process-local keyed audit token material.

There is no selected-mailbox state, Mail connection pool, SMTP session, contact
write cache, local event/contact/message store, or cross-call remote-content
cache.

## Consistency tokens

- Calendar and Contacts expose ETags. Update/delete perform a full GET, require a
  usable strong server ETag, and always send a specific `If-Match`; a caller ETag
  can strengthen the precondition. Create uses `If-None-Match: *`.
- A Mail message reference is `(mailbox, UIDVALIDITY, UID)`. Get and mutation
  compare UIDVALIDITY after selecting the mailbox. Search cursors pair
  `before_uid` with the preceding page's UIDVALIDITY.
- Reads expose MODSEQ when CONDSTORE is available. The current adapter cannot
  safely detect tagged MODIFIED responses, so conditional flag mutation fails
  with `protocol_error` before STORE instead of degrading to an unconditional
  update. That unavailable beta.8 path does not report
  `concurrent_modification`.

## Output model

Calendar text, contact data, and Mail content are untrusted. The stdio reader
accepts at most 1 MiB per JSON-RPC frame. Protocol/schema errors that could
reflect caller input are passed through only up to 64 KiB; larger records are
replaced with bounded local errors. Every serialized MCP result, including
Calendar, is capped at 256 KiB.

Search tools return summary models. Contact PHOTO/raw vCard and Mail raw
MIME/raw headers/HTML/body attachments are excluded. Calendar REPORT XML is
bounded to depth 32 and 262,144 tokens, with response/property item caps;
iCalendar has component, property, parameter, override, alarm, and EXDATE caps.
Contact DAV XML is bounded to depth 32 and 100,000 tokens, with propstat/property
caps, and each vCard has a 10,000-property cap. Recurrence expansion returns at
most 2,000 occurrences and performs at most 100,000 iterator advances per
series. IMAP guards protocol nesting/list counts before recursive decode and
modeled MIME parts/depth afterward. SMTP accepts at most 1 MiB of aggregate
inbound responses per session.

## Hand-rolled DAV boundaries

Calendar keeps hand-rolled discovery, iCloud-compatible REPORT, and conditional
PUT/DELETE because go-webdav v0.7.0 loses shard authority in discovery and lacks
the required conditional write API. Calendar update always GETs the full object
before PUT so VERSION, PRODID, and VTIMEZONE survive.

Contacts uses hand-rolled bounded PROPFIND, REPORT, GET, PUT, DELETE, href
resolution, redirects, and XML decoding. Resource hrefs are arbitrary and are
never derived from a contact UID.

## Mutation audit model

Every production Calendar, Contacts, IMAP, and SMTP mutation emits the same
resource-safe shape: tool, `domain`, `resourceType`, process-local opaque HMAC
`resourceToken`, and status. Calendar hashes its path/UID tuple before logging;
no production audit record contains the raw Calendar path or UID. Contact UIDs,
mailbox/UIDVALIDITY/UID tuples, and recipients are likewise never logged raw.

See [CalDAV compatibility](caldav-compatibility.md),
[CardDAV compatibility](carddav-compatibility.md),
[Mail compatibility](mail-compatibility.md), and [security](security.md).
