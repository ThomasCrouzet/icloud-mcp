# CardDAV / iCloud compatibility

This document is Contacts-specific. Calendar uses the separate CalDAV client in
[caldav-compatibility.md](caldav-compatibility.md).

## Endpoint and authentication

- Entry URL: `https://contacts.icloud.com/`.
- Authentication: HTTP Basic over verified TLS using a Contacts-owned copy of
  `ICLOUD_EMAIL` and `ICLOUD_PASSWORD`.
- Allowed authorities: case-sensitive equality to `contacts.icloud.com:443` and
  lowercase `p[0-9]{1,3}-contacts.icloud.com:443` shards only (hosts are not
  folded before comparison).
- TLS uses verified system roots and TLS 1.2 or later. The transport has
  `Proxy: nil`.
- Calendar credentials cannot be attached by the Contacts transport, and
  Contacts redirects cannot switch to the Calendar allowlist.

Apple documents third-party Contacts access with app-specific passwords and
CardDAV configuration. Exact shard/discovery behavior is an interoperability
property, not a guarantee that arbitrary iCloud regional authorities are safe.
The allowlist intentionally excludes unreviewed host patterns.

## Lazy discovery

The first Contacts call performs:

1. Depth 0 PROPFIND for `current-user-principal`.
2. Depth 0 PROPFIND on the principal for `addressbook-home-set`.
3. Depth 1 PROPFIND on every validated home set for address-book collections,
   display metadata, supported address data, and maximum resource size.

Discovery is serialized across concurrent callers and capped at 10 seconds
within the 25 second tool deadline. Only a complete validated success is cached.
A failed attempt can be retried by a later call. Successful principal, home-set,
collection, and shard authorities remain pinned for the process lifetime.

Zero home sets maps to `not_found`. Duplicate homes/books, collection escape,
unapproved authorities, and more than 100 books fail closed. Tools receive only
opaque `book-...` identifiers derived from validated collection URLs; callers
cannot supply a CardDAV URL.

## Redirects and hrefs

Automatic HTTP redirects are disabled. For reads, the Contacts client follows
301, 302, 307, and 308 manually for no more than three hops, preserving the
request method and body. Relative `Location` and DAV href values are resolved
against the exact response URL that supplied them, then HTTPS, case-sensitive
host allowlist match (production hosts are lowercase), port, and collection
containment are revalidated. Read-side 303 and other redirect codes are
rejected. A redirect observed after PUT or DELETE dispatch is never followed and
returns `outcome_unknown`, including malformed or policy-violating redirects.

CardDAV resource hrefs are arbitrary. The client never assumes that UID maps to
`UID.vcf`. UID lookup uses an `addressbook-query`, requires exactly one match,
retains the returned href internally, and performs a full GET before exposing or
mutating the contact.

## vCard model

- vCard 3.0 and 4.0 are accepted on reads.
- Writes encode vCard 3.0 only. An address book is writable when it advertises
  3.0 or omits `supported-address-data`.
- Create includes VERSION, PRODID, UID, FN, and N.
- A caller can provide `client_uid`; otherwise the client generates a
  UUIDv4-compatible UID with `crypto/rand`.
- PHOTO bytes, raw vCards, and raw extension values are not returned.
- A vCard 3.0 update modifies the full decoded object, preserving PHOTO and
  unknown properties that fit the resource limit.
- vCard 4.0 objects are read-only to avoid silent downgrade through the 3.0
  encoder.
- Apple group cards are readable and excluded from search by default. Group
  mutation is rejected.
- Birthday is returned only when it is a valid `YYYY-MM-DD`; unsupported forms
  produce `unsupportedFields: ["birthday"]` without the raw value.

Modeled contact detail can include display/structured name, organization, title,
nickname, birthday, typed emails/phones/URLs, postal addresses, notes, ETag, and
the address-book identifier. Search summaries omit notes, addresses, URLs,
birthday, raw cards, and photos.

## Search behavior

`search_contacts` uses a bounded CardDAV server predicate where its text-match
semantics match the requested filter. A general `query` sends one any-of
FN/N/EMAIL/TEL/ORG contains filter. When no general query is supplied, `email`
uses an EMAIL contains filter. The full returned cards are then checked locally
so every supplied `query`, `email`, `phone`, and `include_groups` condition is
combined rather than allowing the server prefilter to define final semantics.

Phone matching is digit-normalized locally and is never sent as a TEL text
predicate. A phone-only search therefore issues the bounded VERSION-presence
all-card query. When phone is combined with query or email, that compatible
server predicate narrows the bounded candidate set and the phone condition is
then applied locally.

Local matching is:

- `query`: case-insensitive substring across FN, N, EMAIL, TEL, and ORG.
- `email`: case-insensitive EMAIL substring.
- `phone`: digit-normalized TEL substring.
- `include_groups`: false by default.

All selected books share aggregate budgets of 2,000 decoded cards and 32 MiB of
REPORT responses. Results sort by normalized display name, then UID, then book,
and are capped at 100 summaries, default 50. There is no offset or continuation
cursor. `truncated` means the output limit/result-byte cap removed matches;
`scanLimitReached` means not every selected card/book fit the scan budget. Narrow
the book or filters when either is true.

## Conditional writes

Create:

- Generates a random `.vcf` child resource name independently from contact UID.
- Sends `Content-Type: text/vcard; charset=utf-8` and `If-None-Match: *`.
- Does not replay PUT after a transport failure; ambiguous transport outcomes
  return `outcome_unknown` with a re-read instruction.
- Re-GETs after definitive success for server normalization and a fresh ETag.
  If that GET fails, create remains successful with `resultIncomplete`.

Update/delete:

1. Query exact UID and full-GET the returned resource.
2. Require a usable specific strong server ETag.
3. Use a valid caller ETag when supplied; otherwise use the GET ETag.
4. Reject wildcard, weak, malformed, and missing ETags.
5. Send a specific `If-Match` on every real PUT/DELETE.
6. Map HTTP 412 to `concurrent_modification`.
7. Re-GET after successful update for normalized data and ETag.

`delete_contact` dry run performs lookup and validation but sends no DELETE.
DAV `no-uid-conflict`, `valid-address-data`, and `max-resource-size`
preconditions map to `conflict`, `validation`, and `payload_too_large`.
Unknown or malformed preconditions map to `protocol_error`; raw XML is never
returned.

## Limits

| Resource | Limit |
|----------|-------|
| Address books | 100 |
| Search summaries | 100, default 50 |
| Cards scanned per search | 2,000 |
| Aggregate search REPORT | 32 MiB |
| One vCard | 1 MiB and 10,000 properties |
| PROPFIND response | 8 MiB |
| DAV XML parser | Depth 32; 100,000 tokens; 8,192 propstats; 16,384 properties |
| Serialized result | 256 KiB |
| Query/email/phone | 256/320/64 bytes |
| Display name/general text/notes | 500/500/4,000 bytes |
| Emails/phones/URLs/addresses | 10/10/5/5 |
| Concurrent DAV requests | 4 |
| Read/write rates | 60/20 per minute, bursts 10/3 |

All byte caps read at most cap plus one when overflow must be distinguished.
Remote names and fields are truncated only at valid UTF-8 boundaries where the
modeled read contract permits truncation.

The real-iCloud Contacts integration suite is behind the `integration` build tag
and `ICLOUD_MCP_ENABLE_CONTACTS=true`. It exercises discovery, bounded search,
and get. Its separately write-gated disposable CRUD fixture verifies general
query matches through FN, N, EMAIL, and ORG, exact email search, digit-normalized
phone search, UID lookup, update, and exact-fixture cleanup. The suite is opt-in,
never runs in CI, and is not executed without credentials.
