# Architecture

## Overview

`icloud-mcp` is one stdio MCP server for Apple/iCloud Calendar, Contacts, and
Mail. Any MCP-compatible host can run it as a child process. It uses JSON-RPC on
stdin and stdout. It reads configuration only from the process environment. The
build produces one release artifact.

The server does not embed or prefer a model vendor or agent product. Calendar
is always enabled. Contacts, Mail read, Mail mutation, and Mail send are
composable capabilities.

The protocol boundary is narrow:

- Calendar uses CalDAV over HTTPS.
- Contacts uses CardDAV over HTTPS.
- Mail reads and mutations use IMAP over implicit TLS.
- Mail send uses authenticated SMTP submission with mandatory STARTTLS.

The server does not use a private Apple API or browser automation. It does not
use a local Apple framework or synced database. It also does not use an external
protocol executable, plugin, or runtime-downloaded code.

## Boot and lifecycle

1. `config.Load` reads and validates the 12 environment variables. It resolves
   boot-only `file://` identity and password values.
2. Each secret file must be a regular file of 4 KiB or less. Its mode must be
   0600 or stricter.
3. Configuration failure occurs before network access. When configuration
   requests send, the literal SMTP recipient policy `*` is valid.
4. This policy writes a boot warning to stderr.
5. The server builds a shared `security.Redactor` from the enabled credential
   pairs. The variants include Basic-auth and SASL PLAIN encodings.
6. `RedactingWriter` receives all stderr and standard library logs.
7. One immutable `CapabilityPlan` applies global read-only and the optional
   domain gates. It is the source of both tool registration and
   `icloud_capabilities` output.
8. The server builds separate Calendar, Contacts, IMAP, and SMTP transports or
   dialers only for enabled capabilities. Calendar and Contacts receive separate
   credential objects and authenticated HTTP clients.
9. Calendar runs eager two-step CalDAV discovery under a 20 second boot
   deadline. Failure prevents stdio from starting.
10. Contacts makes no boot network access. Its first call starts discovery
    through a concurrency-safe gate with a 10 second attempt deadline.
11. Contacts caches only a complete, validated discovery. It does not cache a
    failure.
12. Mail makes no boot network access. Each attempt creates and closes a new
    authenticated protocol session.
13. A transient Mail read can use one replacement session. Mutations and SMTP
    never retry.
14. The finalized plan registers the MCP handlers. The optional loopback-only
    `-health` endpoint starts next.
15. Then, `ServeStdio` controls stdin and stdout.

EOF cancels active handlers before the server waits for its workers. EOF and
SIGTERM are normal shutdown conditions. Terminal input failures, including an
oversized frame, remain errors even when cancellation interrupts the reader.

After boot, a Contacts, IMAP, or SMTP failure affects only that tool call. It
does not unregister tools or change another domain client. It also does not
change a successful Contacts discovery cache.

## Packages

| Package | Role |
|---------|------|
| `cmd/icloud-mcp` | Configuration wiring, domain construction, eager Calendar discovery, capability plan, timeouts, stdio |
| `internal/config` | Strict booleans, environment validation, `file://` secrets (0600+, 4 KiB), Mail recipient policy |
| `internal/security` | DAV and socket allowlists, TLS policy, redaction, process-local audit tokens |
| `internal/icloud` | Calendar CalDAV, iCalendar, recurrence, free slots, and validation.<br>Calendar retry/rate policy; per-calendar materialization 2,500; multi-calendar materialization 10,000; imported-UID REPORT +/-50y. |
| `internal/contacts` | Lazy CardDAV discovery, bounded DAV/XML, vCard model, search, conditional writes |
| `internal/mail` | Mail service, MIME handling, IMAP mutation policy, SMTP submission |
| `internal/mail/imapadapter` | Narrow beta go-imap boundary and decode-time protocol guard |
| `internal/mcptools` | Compositional schemas, handlers, capability reporting, redacted results, mutation audit |
| `internal/health` | Optional loopback-only `/healthz` and `/status` with version, domain enablement, and multi-domain rate limits |

`internal/icloud` keeps its historical name but contains only Calendar code.
Shared code can provide redaction, audit formatting, result sizing, and limiter
primitives. It does not own an authenticated client for multiple domains.

## Capability composition

The following table groups the complete surface:

| Capability group | Read/local tools | Mutation tools |
|------------------|------------------|----------------|
| Global | `icloud_capabilities` | none |
| Calendar | `list_calendars`, `search_events`, `get_event`, `find_free_slots`, `validate_event`, `calendar_capabilities` | `create_event`, `update_event`, `delete_event` |
| Contacts | `list_address_books`, `search_contacts`, `get_contact` | `create_contact`, `update_contact`, `delete_contact` |
| Mail read | `list_mailboxes`, `search_messages`, `get_message` | none |
| Mail mutation | none | `set_message_flags`, `move_message`, `trash_message` |
| Mail send | none | `send_message` |

Default registration has 10 tools: nine Calendar tools and the global capability
tool. Global read-only with optional domains disabled has seven tools. The
complete surface has 23 tools. Disabled tools have no handler. Disabled optional
domains have no client.

The same immutable registration plan generates the local
`icloud_capabilities` result. It reports the version, global read-only state,
healthcheck state, configured domains, and effective capability groups. It also
reports sorted tool names and the count.

The result contains no identity, secret, host, shard, path, mailbox, recipient,
or runtime error. `calendar_capabilities` contains only Calendar data.

## Domain request paths

### Calendar

```text
tool -> 25s context -> Calendar read/write limiter -> 4/2 semaphore -> retry policy
     -> Calendar Basic-auth client -> Calendar allowlist -> verified HTTPS
```

Calendar discovery is eager. A successful discovery state is immutable. Reads
have bounded service retries. The HTTP classifier retries 429, 502, 503, and
504. It uses bounded `Retry-After` or backoff and rewinds read request bodies. It
does not retry transport errors.

The client never repeats PUT, DELETE, or full-series delete. A transport failure
after mutation dispatch maps to `outcome_unknown`. Gateway 502, 503, and 504
have the same result. Update and delete read the full object again and use
conditional requests.

Multi-calendar `search_events` queries each selected calendar. It then sorts the
events and applies the fair 400-event result limit. More than 10,000 filtered
materialized events cause `payload_too_large`. The per-calendar REPORT
materialization limit is 2,500. When `<uid>.ics` is missing, imported-UID REPORT
fallback uses a 50-year window on each side of now.

### Contacts

```text
tool -> 25s context -> lazy discovery -> Contacts read/write limiter
     -> 4-request semaphore -> Contacts Basic-auth client
     -> Contacts allowlist -> verified HTTPS
```

Discovery resolves current-user-principal and one or more address-book home
sets. It also resolves at most 100 books. Opaque book identifiers refer to fixed,
validated collection URLs. Reads can retry bounded HTTP status failures. The
client never repeats PUT or DELETE. Transport ambiguity maps to
`outcome_unknown`.

Contact search uses CardDAV text predicates as a bounded prefilter when their
meaning matches. A general query uses any-of FN/N/EMAIL/TEL/ORG. Without a
general query, email uses EMAIL. The client then combines each query, email,
phone, and group condition locally.

Phone matching normalizes digits. Thus, a phone-only search uses a bounded
all-card VERSION-presence query instead of a server TEL predicate.

### Mail read and mutation

```text
tool -> 25s context -> Mail limiter/semaphore -> fixed IMAP dial
     -> verified implicit TLS -> login -> LIST or SELECT -> bounded commands
     -> logout/close
```

Read operations use EXAMINE and PEEK. Search scans bounded descending UID
windows. Message retrieval gets metadata, selected headers, BODYSTRUCTURE, and
at most one selected plain-text MIME section. A guarded connection applies the 4 MiB
inbound session limit. It also limits protocol nesting and lists before go-imap
creates a recursive BODYSTRUCTURE.

Mutation sessions select the source mailbox for read and write. They compare
UIDVALIDITY before mutation. Only one mutation runs at a time. The client never
retries a mutation command automatically.

### Mail send

```text
tool -> local input/recipient/message validation -> 25s context
     -> send limiter/semaphore -> smtp.mail.me.com:587 -> EHLO
     -> mandatory STARTTLS -> verified TLS -> EHLO -> AUTH
     -> MAIL FROM -> every RCPT TO -> DATA only if all recipients succeeded
```

The client builds the encoded message in bounded memory before connection. From
is the configured Mail address. `to`, `cc`, and `bcc` are individually optional.
Together, they must contain at least one recipient. Bcc exists only in the
envelope. The client keeps no SMTP session and retries no stage.

## State and concurrency

MCP handlers can run concurrently. The server limits and protects this shared
mutable state:

- Calendar successful discovery state, independent rate buckets, and 4/2
  read/write semaphores.
- Contacts successful lazy-discovery state, independent rate buckets, and a
  four-request semaphore.
- Mail independent read/mutation/send buckets and 2/1/1 semaphores.
- Process-local keyed audit token material.
- Tool-namespaced idempotency entries shared by Calendar and Contacts updates.

The server has no selected-mailbox state, Mail connection pool, or SMTP session.
Read operations have no result cache across calls. Keyed updates keep bounded
result text in memory under the contract below.

## Consistency tokens

- Calendar and Contacts expose ETags. Update and delete send a full GET and
  require a usable, strong server ETag.
- They always send a specific `If-Match`. A caller ETag can make the precondition
  stronger. Create uses `If-None-Match: *`.
- A Mail message reference is `(mailbox, UIDVALIDITY, UID)`. Get and mutation
  compare UIDVALIDITY after selecting the mailbox. Search cursors pair
  `before_uid` with the preceding page's UIDVALIDITY.
- Reads expose MODSEQ when CONDSTORE is available. The current adapter cannot
  safely detect tagged MODIFIED responses.
- Thus, a conditional flag mutation fails with `protocol_error` before STORE. It
  does not become an unconditional update.
- This unavailable beta.8 path does not report `concurrent_modification`.

## Update idempotency

Only `update_event` and `update_contact` use this result cache when the caller
supplies `idempotency_key`. Create tools use resource UIDs for conflict detection.
They do not use this cache.

Each entry identifies a tool, a key, and a SHA-256 hash of parsed update
parameters. Parameters include the resource identity, changed fields, and `etag`.
Calendar scope and `recurrence_id` also participate. Tool namespaces prevent a
Calendar key from matching a Contacts key.

The first valid request owns a pending claim. Concurrent calls with the same key
and parameters wait for that owner. Different parameters return `conflict`, even
while the owner is pending. Cancellation or timeout ends only the waiting call.
It does not release the owner's claim or permit another write.

| Entry state | Lifetime |
|-------------|----------|
| Pending claim | No expiry while the process runs |
| Success or definitive domain error | 15 minutes after the request completes |
| `outcome_unknown`, unclassified error, or `internal_error` | Until process exit |
| Response serialization or result-size error after a successful write | Until process exit |
| Interrupted handler or result that cannot be recorded | Bounded `outcome_unknown` result until process exit |

The server saves result text and the MCP `IsError` flag. Cache hits return the
saved result through the domain writer, which applies redaction and result
limits again. A cache hit does not extend the expiry interval. Local validation
failures before a claim do not create an entry.

The cache has a shared limit of 1,024 entries and a 256 KiB payload limit per
entry. Expired known results free capacity. Pending and uncertain entries are
not removed to make space. A full cache returns `conflict` for a new claim before
it calls the service. Existing entries can still return their saved results.

All entries exist only in process memory. Another process cannot use them.
Process exit removes every entry. After expiry or restart, the same key can
start a new write. Neither condition proves that the previous write failed.
Before choosing a new key after an ambiguous result, use `get_event` or
`get_contact` to reconcile the resource.

See [error recovery](error-codes.md#update-idempotency-and-recovery) for caller
procedures, including full-cache conflicts.

## Output model

Calendar text, contact data, and Mail content are untrusted. The stdio reader
accepts at most 1 MiB in each JSON-RPC frame. Protocol or schema errors can
reflect caller input. The server passes them through only up to 64 KiB. It
replaces larger records with bounded local errors. Each serialized MCP result,
including Calendar, has a 256 KiB limit.

Search tools return summary models. Contact results exclude PHOTO and raw vCard.
Mail results exclude raw MIME, raw headers, HTML, and attachment bodies.

Calendar REPORT XML has depth 32 and 262,144-token limits. It also has response
and property item limits. iCalendar limits components, properties, parameters,
overrides, alarms, and EXDATE values.

Contact DAV XML has depth 32 and 100,000-token limits. It also limits propstats
and properties. Each vCard has a 10,000-property limit.

Calendar recurrence expansion returns at most 2,000 occurrences. Its iterator
advances at most 100,000 times for each series.

IMAP limits protocol nesting and list counts before recursive decode. It limits
modeled MIME parts and depth after decode. SMTP accepts at most 1 MiB of inbound
responses in each session.

## Hand-rolled DAV boundaries

Calendar uses custom discovery, iCloud-compatible REPORT, and conditional PUT or
DELETE code. go-webdav v0.7.0 loses shard authority during discovery. It also
lacks the required conditional write API. Calendar update always gets the full
object before PUT. Thus, VERSION, PRODID, and VTIMEZONE remain.

Contacts uses custom bounded PROPFIND, REPORT, GET, PUT, and DELETE code. It also
uses custom href resolution, redirects, and XML decoding. Resource hrefs are
arbitrary. The client never derives them from a contact UID.

## Mutation audit model

Each production Calendar, Contacts, IMAP, and SMTP mutation writes the same safe
fields. They are tool, `domain`, `resourceType`, process-local opaque HMAC
`resourceToken`, and status.

Calendar hashes its `path/UID` tuple before logging. Production audit records
contain no raw Calendar path or UID. They also contain no raw contact UID,
`mailbox/UIDVALIDITY/UID` tuple, or recipient.

## Protocol verification

Production startup and the fixture-only executable use shared MCP registration
and bounded stdio startup code. The fixture executable is compiled from
`cmd/icloud-mcp` tests with synthetic services. It checks initialization,
capability registration, denied writes, frame limits, cancellation, domain
failure isolation, and shutdown. These fixtures do not change production
destination policies.

The external `scripts/protocol_evidence.py` runner also selects `^TestProtocol`
scenarios in `internal/mcptools`. Idempotency scenarios use stateful service
fixtures. Recurrence scenarios use a local TLS DAV fixture and the Calendar
client. These checks do not establish live iCloud compatibility.

The runner saves transcripts, commands, environment metadata, source revision,
working diff, fixture SHA-256 values, and results outside the repository.
See [Testing](testing.md) for commands and artifact details.

See [CalDAV compatibility](caldav-compatibility.md),
[CardDAV compatibility](carddav-compatibility.md),
[Mail compatibility](mail-compatibility.md), and [security](security.md).
