# Mail / iCloud compatibility

This document covers the optional IMAP read/mutation and SMTP submission
clients. Mail is independent from Calendar CalDAV and Contacts CardDAV.

## Apple-documented endpoints

| Protocol | Host | Port | Encryption | Username |
|----------|------|------|------------|----------|
| IMAP | `imap.mail.me.com` | 993 | Required implicit verified TLS | Mail address local part first, then full address fallback |
| SMTP submission | `smtp.mail.me.com` | 587 | Mandatory verified STARTTLS | Full iCloud Mail address |

Both protocols use an app-specific password. The client does not support POP.
Apple does not document all related behavior. Examples include IMAP
capabilities, mailbox names, SPECIAL-USE mappings, MOVE, UIDPLUS, CONDSTORE,
authentication details, and SMTP Sent copies. The client negotiates these
properties or fails closed. It does not use fixed assumptions.

The client uses fixed socket destinations. IMAP requires exactly
`imap.mail.me.com:993`. SMTP requires exactly `smtp.mail.me.com:587`. TLS uses
system trust, fixed server names, and TLS 1.2 or later. The client does not
enable protocol debug writers.

## Session lifecycle and authentication

Each tool attempt opens a new socket. It closes the authenticated session after
the operation. Selected mailboxes, IDLE connections, IMAP sessions, and SMTP
sessions do not remain between calls.

IMAP uses this sequence:

```text
fixed dial -> verified implicit TLS -> greeting -> LOGIN -> capability snapshot
           -> LIST or SELECT -> bounded commands -> logout/close
```

The first LOGIN username is the local part of `ICLOUD_MAIL_ADDRESS`. The adapter
tries the full address once after an explicit authentication rejection. It does
not use this fallback after a network, timeout, or generic protocol error.
Errors do not identify the attempted identity.

A transient Mail read can open one replacement session if the first session
returned no result. Mutation and SMTP paths never retry. Cancellation closes
the active connection.

## Mailbox and message identity

LIST supplies mailbox names, hierarchy delimiters, and attributes. The server
does not infer a mailbox purpose from an English display name. This rule applies
to Inbox, Sent, Trash, and other purposes. `list_mailboxes` sends no STATUS
fan-out and returns at most 200 items.

A message identity is:

```text
(mailbox, UIDVALIDITY, UID)
```

`search_messages` returns UIDVALIDITY with each page and message. `get_message`
and each mutation require it. After SELECT, a mismatch returns
`concurrent_modification`. This check occurs before the client gets or changes
the message. UID zero and UIDVALIDITY zero are invalid.

## Search and pagination

`search_messages` accepts one mailbox and optional search criteria. The criteria
are TEXT, From, To, Subject, inclusive `since`, exclusive `before`, unseen, and
flagged. Search strings have a 512-byte UTF-8 limit. Typed go-imap criteria send
them without protocol string concatenation. Dates use `YYYY-MM-DD` and IMAP
internal-date day granularity.

The search reads descending UID ranges in windows of at most 5,000. It starts
below UIDNEXT or the exclusive `before_uid`. It scans at most 50,000 UID values.
It never sends an unrestricted mailbox-wide search. A cursor must include the
UIDVALIDITY from the previous page.

Results sort by descending UID within one UIDVALIDITY. UID order is append
order, not message header-date order. The default result limit is 20. The
maximum is 50. `nextBeforeUid` is the next exclusive cursor.

`scanLimitReached` means that the search did not read older UID space.
`truncated` means that the count or 256 KiB output limit removed summaries.

Search gets only UID, flags, envelope, internal date, RFC822 size, and
BODYSTRUCTURE. It also gets MODSEQ when available. It gets no snippet or body
section.

## Message retrieval

Read tools select mailboxes read-only and use PEEK. They must not set Seen.

`get_message` first gets metadata, BODYSTRUCTURE, and four additional headers:
Message-ID, In-Reply-To, References, and Reply-To. It selects the first inline
`text/plain` leaf without attachment or filename semantics. Then, bounded
partial PEEK requests get the MIME header and body. It never gets an unbounded
`BODY.PEEK[]`.

The result contains selected envelope and header metadata. It also contains
decoded plain text and attachment metadata. It never contains raw MIME, raw
headers, Received or authentication headers, HTML, or attachment payloads.
BODYSTRUCTURE supplies attachment metadata without content retrieval. The
client treats attached `message/rfc822` parts and their descendants as
attachments. It does not inspect them for body text.

If no plain text exists, the result has metadata with `html_only` or
`no_plain_text`. If wire or decoded text is too large, the client omits the body
and adds a bounded warning. Unsafe decoding has the same result. The client can truncate an otherwise valid
body at a valid UTF-8 boundary. This truncation keeps the result within 256 KiB.
Metadata that cannot fit returns `payload_too_large`.

## IMAP decode limits

`internal/mail/imapadapter` isolates the beta go-imap client. Before the library
creates recursive BODYSTRUCTURE values, `guardedConn` applies these limits:

| Resource | Limit |
|----------|-------|
| Inbound bytes per session | 4 MiB |
| One protocol line | 1 MiB |
| Protocol parenthesis depth | 24 |
| Protocol lists | 512 |
| Quoted protocol string | 8,194 bytes |

The modeled layer applies these additional limits:

| Resource | Limit |
|----------|-------|
| MIME parts / nesting | 200 / 20 |
| Curated header section | 64 KiB |
| Selected text wire bytes | 512 KiB |
| Decoded plain text | 100 KiB default, 200 KiB maximum |
| Addresses / attachments | 100 / 100 |
| One metadata string | 4 KiB |
| Serialized result | 256 KiB |

## Flag mutation

`set_message_flags` accepts exactly one add or remove operation. It also accepts
one to three unique values from Seen, Flagged, and Answered. It cannot replace
FLAGS, set Deleted or Recent, or create arbitrary keywords.

The session selects the mailbox for read and write. It checks UIDVALIDITY and
the existence of the message. Without CONDSTORE, the adapter sends one
delta-only `+FLAGS.SILENT` or `-FLAGS.SILENT`. It never sends a full replacement.
The result reports `conditionalUpdate: false`. Then, it tries to get the
resulting flags.

When the IMAP server advertises CONDSTORE, flag updates require `expected_modseq`.
The reviewed go-imap beta.8 API cannot safely expose the tagged MODIFIED result.
Thus, the adapter returns `protocol_error` before STORE. This rule applies even
when `expected_modseq` is present. The adapter does not send an unconditional
mutation. It does not report `concurrent_modification` on this path.

Conditional flag writes stay unavailable until the project proves MODIFIED
detection.

## Move and trash

`move_message` first verifies that LIST contains exactly one selectable match
for the destination. It then selects the source for read and write. It checks
UIDVALIDITY and the source UID.

- When the server advertises MOVE, the client uses native UID MOVE.
- Otherwise, it requires UIDPLUS. It sends UID COPY, adds Deleted to that UID,
  and then sends UID EXPUNGE for that UID.
- It never uses plain EXPUNGE or mailbox-wide EXPUNGE.
- It waits for definitive completion before the next step. It never retries or
  compensates automatically.

A transport ambiguity during native MOVE or COPY returns `outcome_unknown`. A
definitive failure after COPY or Deleted returns a bounded `partial_failure`.
An ambiguous later step returns `outcome_unknown`. Reconciliation identifies
the mailboxes and state that the caller must inspect.

`trash_message` requires exactly one selectable LIST mailbox with SPECIAL-USE
`\Trash`. It then applies the same move policy. Zero or multiple Trash targets
fail closed. The tool has no permanent-delete action. It rejects a source that
is already in that Trash mailbox.

## SMTP submission

The server registers SMTP send only when all send gates are active. Mail must be
enabled, global read-only must be false, and configuration must request Mail send. The
recipient policy must also be valid. Mail mutation is not required.

The path is:

```text
smtp.mail.me.com:587 -> EHLO -> mandatory STARTTLS -> verified TLS -> EHLO
                     -> AUTH PLAIN -> MAIL FROM -> every RCPT TO -> DATA
```

There is no plaintext-authentication fallback. The envelope and MIME From value
is exactly `ICLOUD_MAIL_ADDRESS`. The message is UTF-8 plain text. The client
generates Date and Message-ID locally. To and Cc occur in headers. Bcc occurs
only in the envelope.

The client rejects HTML, attachments, raw MIME, custom headers, and display-name
recipients. It also rejects groups, caller-selected From, header newlines, and
NUL.

The local policy validates all To, Cc, and Bcc addresses before connection. It
permits only exact configured addresses with ASCII case-insensitive matching.
The literal complete policy `*` permits all addresses. Recipients must be unique,
with a limit of 50.

`to`, `cc`, and `bcc` are each optional. The three arrays must contain at least
one recipient in total. Subject has a 998-byte limit. Body has a 100 KiB limit.
The complete encoded message has a 256 KiB limit. Inbound SMTP responses have a
total 1 MiB limit for each session.

The client tries each RCPT command unless an ambiguous failure makes more
commands unsafe. The failure can come from the protocol or transport. A
definitive RCPT rejection prevents DATA. The client sends RSET when possible.
Thus, it never submits an accepted subset.

Submission outcomes are:

- `accepted` only after a definitive successful final DATA response.
- `rejected` when at least one RCPT receives a definitive rejection. A
  definitive DATA rejection has the same result.
- A structured tool error for validation, authorization, authentication,
  protocol, size, or availability failures before DATA. These paths include
  local policy, STARTTLS, AUTH, and MAIL FROM failures. The client submits no
  message on these paths.
- `outcome_unknown` if connection loss or cancellation occurs after DATA may
  have reached the server and no definitive final response is available.

The client never retries SMTP. After `outcome_unknown`, inspect Sent and the
recipients before another send. The client never uses APPEND for a Sent copy.
Thus, accepted SMTP returns `sentCopyUnavailable: true`. This field means that
the client did not make sure that a copy exists. iCloud can create one, but the
client does not assume this behavior.

## Rates and concurrency

| Path | Rate and burst | Concurrent sessions | Retry |
|------|----------------|---------------------|-------|
| IMAP read | 60/minute, burst 10 | 2 | One replacement session for a transient read at most |
| IMAP mutation | 20/minute, burst 3 | 1 | None |
| SMTP send | 20/minute, burst 3 | 1 | None |

Each attempt uses the applicable rate budget. All operations have a 25 second
MCP tool deadline. The active context also limits each socket deadline.
