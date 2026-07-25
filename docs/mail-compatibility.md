# Mail / iCloud compatibility

This document covers the optional IMAP read/mutation and SMTP submission
clients. Mail is independent from Calendar CalDAV and Contacts CardDAV.

## Apple-documented endpoints

| Protocol | Host | Port | Encryption | Username |
|----------|------|------|------------|----------|
| IMAP | `imap.mail.me.com` | 993 | Required implicit verified TLS | Mail address local part first, then full address fallback |
| SMTP submission | `smtp.mail.me.com` | 587 | Mandatory verified STARTTLS | Full iCloud Mail address |

Both protocols use an app-specific password. POP is not supported. Apple does
not document every IMAP capability, mailbox name, SPECIAL-USE mapping, MOVE,
UIDPLUS, CONDSTORE behavior, authentication detail, or SMTP Sent-copy behavior.
The implementation negotiates or fails closed rather than hardcoding those
properties.

The socket destinations are fixed and not configurable. IMAP requires exactly
`imap.mail.me.com:993`; SMTP requires exactly `smtp.mail.me.com:587`. TLS uses
system trust, fixed server names, and TLS 1.2 or later. Protocol debug writers
are not enabled.

## Session lifecycle and authentication

Each tool attempt opens a new socket and closes the authenticated session after
the operation. No selected mailbox, IDLE connection, IMAP session, or SMTP
session persists between calls.

IMAP performs this sequence:

```text
fixed dial -> verified implicit TLS -> greeting -> LOGIN -> capability snapshot
           -> LIST or SELECT -> bounded commands -> logout/close
```

The first LOGIN username is the local part of `ICLOUD_MAIL_ADDRESS`. The adapter
retries exactly once with the full address only when the first response is an
explicit authentication rejection. It does not fall back after a network,
timeout, or generic protocol error, and errors do not reveal which identity was
attempted.

A transient Mail read may open one replacement session if no result was
returned. Mutation and SMTP paths never retry. Cancellation closes the active
connection.

## Mailbox and message identity

Mailbox names, hierarchy delimiters, and attributes come from LIST. The server
does not infer Inbox, Sent, Trash, or another purpose from an English display
name. `list_mailboxes` performs no STATUS fan-out and returns at most 200 items.

A message identity is:

```text
(mailbox, UIDVALIDITY, UID)
```

`search_messages` returns UIDVALIDITY with every page and message. `get_message`
and every mutation require it. After SELECT, a mismatch returns
`concurrent_modification` before fetching or changing the requested message.
UID zero and UIDVALIDITY zero are invalid.

## Search and pagination

`search_messages` accepts one mailbox plus optional TEXT, From, To, Subject,
inclusive `since`, exclusive `before`, unseen, and flagged criteria. Search
strings are capped at 512 UTF-8 bytes and are passed through typed go-imap search
criteria rather than concatenated into protocol syntax. Dates use `YYYY-MM-DD`
and IMAP internal-date day granularity.

The search walks descending UID ranges in windows of up to 5,000, beginning
below UIDNEXT or below exclusive `before_uid`. It scans at most 50,000 UID values
and never issues an unrestricted mailbox-wide search. A cursor must include the
same UIDVALIDITY returned by the previous page.

Results are sorted by UID descending within one UIDVALIDITY. UID order is append
order, not message header-date order. The default result limit is 20 and the
maximum is 50. `nextBeforeUid` is the next exclusive cursor.
`scanLimitReached` means older UID space was not searched; `truncated` means the
result count or 256 KiB output budget removed summaries.

Search fetches only UID, flags, envelope, internal date, RFC822 size, MODSEQ when
available, and BODYSTRUCTURE. It fetches no snippet or body section.

## Message retrieval

Read tools select mailboxes read-only and use PEEK. They must not set Seen.

`get_message` first fetches metadata, BODYSTRUCTURE, and only these additional
headers: Message-ID, In-Reply-To, References, and Reply-To. It chooses the first
inline `text/plain` leaf with no attachment/filename semantics, then fetches the
part MIME header and that part body with bounded partial PEEK requests. It never
fetches an unbounded `BODY.PEEK[]`.

The result contains curated envelope/header metadata, decoded plain text, and
attachment metadata. It never returns raw MIME, raw headers, Received/authentication
headers, HTML, or attachment payloads. Attachment metadata is derived from
BODYSTRUCTURE without fetching content. Attached `message/rfc822` and descendants
are treated as attachments rather than traversed for body text.

If no plain text exists, metadata is returned with `html_only` or
`no_plain_text`. If wire/decoded text is too large or decoding is unsafe, the
body is omitted with a bounded warning. A body that fits its requested ceiling
can be truncated at a valid UTF-8 boundary to fit the 256 KiB result. Metadata
that cannot fit returns `payload_too_large`.

## IMAP decode limits

The beta go-imap client is isolated behind `internal/mail/imapadapter`. Before
the library materializes recursive BODYSTRUCTURE values, `guardedConn` enforces:

| Resource | Limit |
|----------|-------|
| Inbound bytes per session | 4 MiB |
| One protocol line | 1 MiB |
| Protocol parenthesis depth | 24 |
| Protocol lists | 512 |
| Quoted protocol string | 8,194 bytes |

The modeled layer additionally enforces:

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

`set_message_flags` accepts exactly one add/remove operation and one to three
unique values from Seen, Flagged, and Answered. It cannot replace FLAGS, set
Deleted or Recent, or create arbitrary keywords.

The session selects the mailbox read-write, checks UIDVALIDITY, and verifies the
message exists. When CONDSTORE is absent, the adapter sends one delta-only
`+FLAGS.SILENT` or `-FLAGS.SILENT`, never a full replacement. The result reports
`conditionalUpdate: false`, then attempts to fetch resulting flags.

When CONDSTORE is advertised, `expected_modseq` is required. The reviewed
go-imap beta.8 API does not safely expose the tagged MODIFIED result. The current
adapter therefore returns `protocol_error` before STORE, even when
`expected_modseq` is present. It does not silently degrade to an unconditional
mutation and does not claim `concurrent_modification` on this path. Conditional
flag writes remain unavailable until MODIFIED detection can be proven.

## Move and trash

`move_message` first verifies that the destination occurs exactly once as a
selectable LIST mailbox. It then selects the source read-write, checks
UIDVALIDITY, and verifies the source UID.

- When MOVE is advertised, it uses native UID MOVE.
- Otherwise it requires UIDPLUS and performs UID COPY, add Deleted to that UID,
  then UID EXPUNGE for that UID.
- It never uses plain EXPUNGE or mailbox-wide EXPUNGE.
- It waits for definitive completion before each next step and never retries or
  compensates automatically.

A native or COPY transport ambiguity returns `outcome_unknown`. A definitive
failure after COPY or after adding Deleted returns a bounded `partial_failure`;
an ambiguous later step returns `outcome_unknown`. Reconciliation tells callers
which mailboxes/state to inspect.

`trash_message` requires exactly one selectable LIST mailbox with SPECIAL-USE
`\Trash`, then applies the same move policy. Zero or multiple Trash targets fail
closed. It exposes no permanent-delete action and rejects a source already in
that Trash mailbox.

## SMTP submission

SMTP send is registered only when Mail is enabled, global read-only is false,
Mail send is requested, and the recipient policy is valid. Mail mutation is not
required.

The path is:

```text
smtp.mail.me.com:587 -> EHLO -> mandatory STARTTLS -> verified TLS -> EHLO
                     -> AUTH PLAIN -> MAIL FROM -> every RCPT TO -> DATA
```

There is no plaintext-authentication fallback. From in both envelope and MIME is
exactly `ICLOUD_MAIL_ADDRESS`. The message is UTF-8 plain text with locally
generated Date and Message-ID. To and Cc appear in headers; Bcc is envelope-only.
HTML, attachments, raw MIME, custom headers, display-name recipients, groups,
caller-selected From, header newlines, and NUL are rejected.

The local policy validates all To/Cc/Bcc addresses before connecting. It permits
only exact configured addresses, using ASCII case-insensitive matching, unless
the complete policy is literal `*`. Recipients must be unique and are capped at
50. `to`, `cc`, and `bcc` are each optional; at least one recipient is required
across the three arrays. Subject is capped at 998 bytes, body at 100 KiB, and the
complete encoded message at 256 KiB. Aggregate inbound SMTP responses are
capped at 1 MiB per session.

Every RCPT command is attempted unless a non-definitive protocol/transport
failure makes further commands unsafe. Any definitive RCPT rejection causes
RSET when possible and prevents DATA, so accepted subsets are never submitted.

Submission outcomes are:

- `accepted` only after a definitive successful final DATA response.
- A `rejected` result when at least one RCPT receives a definitive rejection, or
  when DATA receives a definitive rejection.
- A structured validation, authorization, authentication, protocol, size, or
  availability tool error for local/policy, STARTTLS, AUTH, MAIL FROM, or other
  pre-DATA failures. No message has been submitted on those paths.
- `outcome_unknown` if connection loss or cancellation occurs after DATA may
  have reached the server and no definitive final response is available.

SMTP is never retried. After `outcome_unknown`, inspect Sent and recipients
before deciding whether to send again. The implementation never APPENDs a Sent
copy and therefore returns `sentCopyUnavailable: true` after accepted SMTP.
That field means the client did not ensure a copy; whether iCloud creates one is
server behavior and is not assumed by the client.

## Rates and concurrency

| Path | Rate and burst | Concurrent sessions | Retry |
|------|----------------|---------------------|-------|
| IMAP read | 60/minute, burst 10 | 2 | One replacement session for a transient read at most |
| IMAP mutation | 20/minute, burst 3 | 1 | None |
| SMTP send | 20/minute, burst 3 | 1 | None |

Every attempt consumes the applicable rate budget. All operations remain within
the 25 second MCP tool deadline, and socket deadlines are clamped by the active
context.
