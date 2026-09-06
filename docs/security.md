# Security implementation

This document describes how the server implements the public threat model in
[SECURITY.md](../SECURITY.md).

## One process and domain boundaries

Calendar, Contacts, and Mail can hold credentials in one address space. A memory
disclosure or arbitrary-code defect can cross these in-process boundaries. The
server limits accidental crossover between credentials and transports. It
cannot limit a complete process compromise.

Each domain owns an immutable credential copy and destination policy. It also
owns a transport or dialer, limiter, retry policy, protocol service, and package
boundary. It owns a semaphore when applicable.

Calendar and Contacts never share an authenticated HTTP client. IMAP and SMTP
accept only fixed-destination dial functions. Feature flags prevent the server
from building disabled optional clients.

## Network allowlists

| Client | Allowed destination | Port | Transport |
|--------|---------------------|------|-----------|
| Calendar | `caldav.icloud.com`, `p[0-9]{1,3}-caldav.icloud.com` | implicit or explicit 443 | HTTPS |
| Contacts | `contacts.icloud.com`, `p[0-9]{1,3}-contacts.icloud.com` | implicit or explicit 443 | HTTPS |
| IMAP | `imap.mail.me.com` | 993 only | Implicit TLS |
| SMTP | `smtp.mail.me.com` | 587 only | TCP upgraded by mandatory STARTTLS |

Host matching uses case-sensitive equality with fixed lowercase production
literals. Examples are `caldav.icloud.com` and `p12-caldav.icloud.com`. The
client does not change host case before comparison. Thus, it rejects mixed-case
variants. Production destinations cannot change.

The client rejects disallowed hosts, schemes, ports, or socket addresses before
the production dialer resolves DNS.

DAV uses `AllowlistTransport` with `Proxy: nil` and verified system roots. It
uses TLS 1.2 or later. `InsecureSkipVerify` is never set. The Calendar HTTP
client validates HTTPS, host, and port for each redirected request. Calendar
discovery validates principal and home-set authorities before it keeps a shard.

Contacts disables automatic redirects. For reads, it follows only 301, 302,
307, and 308 for at most three hops. It resolves relative `Location` values
against the response URL. It keeps the original method and replayable body. It
validates each read hop again.

Contacts rejects read-side 303 and all other redirect statuses. It never follows
a redirect after PUT or DELETE. Such a redirect returns `outcome_unknown`.

The client independently validates each principal, home set, address book,
REPORT href, GET target, and mutation target. It applies the Contacts policy and
collection boundary.

The IMAP dialer requires exactly `tcp` and `imap.mail.me.com:993`. It completes a
verified TLS handshake before the protocol adapter receives the connection. The
fixed value is `ServerName=imap.mail.me.com`.

SMTP requires exactly `tcp` and `smtp.mail.me.com:587`. Authentication remains
unavailable until the SMTP adapter completes mandatory verified STARTTLS. The
fixed value is `ServerName=smtp.mail.me.com`.

Tests inject fake HTTP clients or dialers. This injection does not make
production hosts configurable.

## Credentials and redaction

Calendar and Contacts use separate credential objects. `ICLOUD_EMAIL` and
`ICLOUD_PASSWORD` supply their values. Mail uses `ICLOUD_MAIL_ADDRESS` and
`ICLOUD_MAIL_PASSWORD`. If the dedicated Mail password is unset, Mail uses a
separate copy of `ICLOUD_PASSWORD`.

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
exists. Their messages never include an identity, secret, invalid Mail address,
recipient value, or file path. The server maps raw protocol errors to bounded
local messages. This includes DAV XML, IMAP tagged text, SMTP replies, and MIME
parser errors.

## `file://` operator boundary

`ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MAIL_ADDRESS`, and
`ICLOUD_MAIL_PASSWORD` support `file://`. The server reads Mail values only when
Mail is enabled. It accepts only a regular file of 4 KiB or less. The file mode
must prevent group and world access, which is 0600 or stricter.

The server reads the file once at boot. It removes surrounding whitespace and
keeps only the value. It rejects FIFOs, devices, directories, oversized files,
and files that groups or other users can read.

The server trusts the operator who controls the environment to select the file.
There is no chroot, base-directory allowlist, or symlink guarantee. The server
rejects an empty path and a path component equal to `..`.

Read errors report only `not_found`, `permission_denied`, `not_regular`,
`too_large`, `insecure_permissions`, or `unreadable`. They never report the
path. The server does not access the disk after boot.

## Read-only and capability gates

The global switch removes handlers rather than returning a runtime disabled
error:

- Calendar: `create_event`, `update_event`, `delete_event` are absent.
- Contacts: `create_contact`, `update_contact`, `delete_contact` are absent.
- Mail: `set_message_flags`, `move_message`, `trash_message`, and
  `send_message` are absent.

Contacts read requires its enable flag. Mail read requires its enable flag. Mail
mutation also requires the Mail write flag. Mail send independently requires
the send flag and a valid SMTP recipient policy. Mail read does not enable
mutation or send. Mail mutation does not enable send.

Global read-only removes the configured write and send tools. It keeps
configuration validation.
Requested Mail send still requires a recipient allowlist at boot.

## SMTP recipient authorization

`ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS` is the literal `*` or a comma-separated set
of unique, exact, plain addr-specs. Matching removes surrounding configuration
spaces. It uses ASCII case-insensitive equality for the complete address.

Display names, groups, empty entries, partial wildcards, domain-only rules, and
suffix rules are invalid. Use an exact address list in production. The literal
`*` explicitly permits all recipients after SMTP AUTH. It writes a boot warning
to stderr.

`send_message` parses and removes duplicates from the complete To, Cc, and Bcc
set. It applies the allowlist and validates subject and body limits. It builds
the bounded message before it opens a socket.

`to`, `cc`, and `bcc` are each optional. Together, they must contain at least
one recipient. From is always the configured Mail address. Headers exclude Bcc.
This policy limits authorized recipients. The literal `*` removes that limit.

SMTP sends each RCPT command. It starts DATA only after definitive acceptance of
all recipients. Any RCPT rejection prevents partial submission. The client never
retries SMTP automatically.

An ambiguous failure after DATA can mean that the server received the message.
This failure returns `outcome_unknown`. Callers must inspect Sent and recipients
before another send.

## Untrusted remote content and output caps

Calendar text, contact fields, mailbox metadata, headers, message bodies, and
attachment names are untrusted remote data. Prompt-injection labels in tool
descriptions and results are informational. They are not a security boundary.

The implementation therefore restricts data shape and size:

- The stdio transport accepts at most 1 MiB in each JSON-RPC frame.
- A generated protocol or schema error can reflect caller input. The server
  writes such an error only through 64 KiB.
- The server replaces a larger error with a bounded local error. Each serialized
  MCP result, including Calendar results, has a 256 KiB limit.
- Calendar search limits ranges, results, recurrence work, fields, PROPFIND, and
  REPORT.
- REPORT XML has these limits: depth 32, 262,144 tokens, 4,096 responses, 16,384
  propstats, and 32,768 properties.
- Parsed iCalendar has 1,024 components and 10,000 total properties at most.
- Each component has at most 1,024 properties. Other limits are 512 overrides,
  64 parameters per property, 64 alarms, and 2,000 EXDATE values.
- A single-calendar search materializes at most 2,500 events.
- Multi-calendar search still queries each selected calendar. It fails closed
  above 10,000 filtered events, before the public sorted limit of 400 events.
- Recurrence expansion returns at most 2,000 occurrences. Its iterator advances
  at most 100,000 times for each series and 250,000 times for each search.
- A preflight check rejects work above these limits. `find_free_slots` exposes no
  busy-event content.
- Contact search summaries exclude notes, raw vCards, PHOTO bytes, and raw
  extension properties. Full get returns modeled fields and bounded notes.
- One vCard has a 1 MiB limit. A search scans at most 2,000 cards and 32 MiB.
- Contacts results have a 256 KiB limit. DAV XML has depth 32 and 100,000-token
  limits.
- DAV XML also has limits of 8,192 propstats and 16,384 properties. One vCard
  has at most 10,000 properties.
- Mail search returns envelope metadata without snippets or bodies.
- Message get returns selected headers and at most one bounded, decoded
  plain-text part. It also returns attachment metadata.
- Message get excludes raw headers, raw MIME, HTML, and attachment payloads.
- IMAP input has a 4 MiB limit for each session and 1 MiB for each protocol line.
- Before recursive decoding, IMAP limits protocol depth to 24 and protocol lists
  to 512.
- Modeled MIME has limits of 200 parts and depth 20. Selected-part headers have
  a 64 KiB limit.
- Body wire bytes have a 512 KiB limit. Decoded text has a 200 KiB limit.
  Serialized Mail results have a 256 KiB limit.
- SMTP accepts at most 50 recipients. Subject has a 998-byte limit, plain-text
  body 100 KiB, and encoded message 256 KiB.
- Inbound SMTP responses have a total 1 MiB limit for each session.

Truncation occurs only at complete result-object or valid UTF-8 boundaries. When
a message body exceeds its limit, the result keeps useful metadata. It sets
`bodyOmitted` and adds a warning. Unsafe decoding has the same result. If the
bounded metadata cannot fit, the tool returns `payload_too_large`.

## Consistency and mutation safety

### Calendar and Contacts ETags

- Calendar and Contacts create operations send `If-None-Match: *`. HTTP 412 maps to
  `conflict`.
- Update and delete operations first get the complete resource and validate a
  strong ETag.
- A supplied caller ETag has priority. Otherwise, the operation uses the GET
  ETag.
- Missing, wildcard, weak, malformed, or unusable ETags fail closed.
- Each real PUT or DELETE sends a specific `If-Match`. HTTP 412 maps to
  `concurrent_modification`.
- Contacts sends another GET after successful create or update. This GET obtains
  normalized metadata.
- If this GET fails, the known success contains `resultIncomplete`. It does not
  contain `outcome_unknown`.

### Mail UIDVALIDITY and MODSEQ

A Mail message uses the identity `(mailbox, UIDVALIDITY, UID)`. Search cursors
must pair `before_uid` with UIDVALIDITY. Get and each mutation select the named
mailbox. They reject a UIDVALIDITY mismatch before an action.

Reads request MODSEQ when the IMAP server advertises CONDSTORE. Safe conditional
STORE requires detection of the tagged MODIFIED response. The current go-imap
beta.8 adapter cannot give this guarantee. Thus, `set_message_flags` rejects the
request before STORE with `protocol_error`.

This unavailable path does not report `concurrent_modification`. Without
CONDSTORE, the adapter uses only delta `+FLAGS.SILENT` or `-FLAGS.SILENT`. These
commands apply to Seen, Flagged, and Answered. The result contains
`conditionalUpdate: false`.

Move uses native UID MOVE when available. Otherwise, it requires UIDPLUS. It
sends UID COPY, adds Deleted, and sends UID EXPUNGE for the one UID. The server
does not expose plain or mailbox-wide EXPUNGE.

A failure after a completed step returns `partial_failure` or
`outcome_unknown`. The result includes reconciliation guidance. Trash requires
exactly one selectable SPECIAL-USE Trash mailbox. It does not expose permanent
delete.

## Retries, rates, and deadlines

All tool handlers have a 25 second deadline. The DAV HTTP timeout is 30 seconds.
Calendar boot discovery has a 20 second deadline. Contacts lazy discovery uses
at most 10 seconds of the tool deadline.

Calendar:

- Calendar reads retry HTTP 429, 502, 503, and 504 for at most six total
  attempts. The read request has a rewindable body.
- The retry uses bounded `Retry-After` or backoff.
- `GuardedService` also retries only non-classified transient reads, at most two
  times.
- Calendar never repeats PUT, DELETE, or full-series delete.
- A transport error after mutation dispatch returns `outcome_unknown`. Gateway
  502, 503, and 504 have the same result.
- A mutation-side 429 is a definitive `rate_limited` result.
- Read/write rates are 60/20 per minute with bursts 10/3 and concurrency 4/2.
  Local waits over two seconds fail fast.

Contacts:

- Safe reads retry 429, 502, 503, and 504 for at most three total attempts. The
  maximum delay is two seconds.
- PUT/DELETE and transport-ambiguous writes are never replayed.
- Read/write rates are 60/20 per minute with bursts 10/3 and at most 4 concurrent
  DAV requests.

Mail:

- A transient read can create one replacement IMAP session before it returns a
  result. Each attempt uses the Mail read rate budget.
- IMAP mutation and SMTP send have no automatic retry.
- Rates are 60 reads, 20 mutations, and 20 sends per minute, with bursts 10/3/3.
  Concurrency is 2 read sessions, 1 mutation, and 1 send.

## Audit

All mutation handlers write one-line records to redacted stderr. The default
format is JSON NDJSON (`-audit-format=json`). Operators can select plain text
with `-audit-format=text`.

Each production Calendar, Contacts, IMAP, and SMTP mutation uses the same fields.
They are tool, `domain`, `resourceType`, process-local opaque HMAC
`resourceToken`, and status. Tokens are stable only within one process. A restart
prevents correlation.

Calendar hashes its `path/UID` tuple before it writes the record. Logs contain
no raw Calendar path or UID. They also exclude raw contact UIDs, mailbox names,
`mailbox/UIDVALIDITY/UID` tuples, Message-IDs, and recipients.

Logs never include Calendar title, location, notes, or `deletedTitle`. Allowed
statuses are `success`, `error`, `denied`, `dry_run`, and `outcome_unknown`.

## Structured errors

Unified Contacts and Mail error codes are:

`validation`, `authentication`, `authorization`, `not_found`, `conflict`,
`concurrent_modification`, `rate_limited`, `timeout`, `unavailable`,
`partial_failure`, `protocol_error`, `payload_too_large`, `outcome_unknown`, and
`internal_error`.

Calendar maps its established internal classifications to the same public
categories when applicable. Errors contain bounded local text. They can contain
retry metadata: `retryable` and `retry_after_seconds`. Ambiguous outcomes
include operation-specific reconciliation.

Errors exclude raw HTTP, XML, IMAP, SMTP, and MIME data. They also exclude
identity, password, secret file path, and recipient policy values.

Agent-facing examples and retry policy: [error-codes.md](error-codes.md).
