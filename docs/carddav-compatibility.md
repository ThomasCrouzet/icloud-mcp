# CardDAV / iCloud compatibility

This document is Contacts-specific. Calendar uses the separate CalDAV client in
[caldav-compatibility.md](caldav-compatibility.md).

## Endpoint and authentication

- Entry URL: `https://contacts.icloud.com/`.
- Authentication uses HTTP Basic over verified TLS. Contacts owns a copy of
  `ICLOUD_EMAIL` and `ICLOUD_PASSWORD`.
- The allowlist uses case-sensitive equality. It accepts
  `contacts.icloud.com:443` and lowercase
  `p[0-9]{1,3}-contacts.icloud.com:443` shards only.
- The client does not change host case before comparison.
- TLS uses verified system roots and TLS 1.2 or later. The transport sets
  `Proxy: nil`.
- The Contacts transport cannot attach Calendar credentials. A Contacts
  redirect cannot use the Calendar allowlist.

Apple documents third-party Contacts access through CardDAV and app-specific
passwords. Exact shard and discovery behavior is an interoperability property.
It does not prove that any iCloud regional authority is safe. The allowlist
excludes host patterns that the project did not review.

## Lazy discovery

The first Contacts call sends these requests:

1. Depth 0 PROPFIND for `current-user-principal`.
2. Depth 0 PROPFIND on the principal for `addressbook-home-set`.
3. Send Depth 1 PROPFIND on each validated home set. Request address-book
   collections, display metadata, supported address data, and maximum resource
   size.

Discovery runs once for concurrent callers. Its limit is 10 seconds within the
25 second tool deadline. The client caches only a complete, validated success.
A later call can retry after failure. Successful principal, home-set,
collection, and shard authorities stay fixed for the process lifetime.

Zero home sets maps to `not_found`. Duplicate homes or books fail closed.
Collection escape, unapproved authorities, and more than 100 books also fail
closed. Tools receive only opaque `book-...` identifiers from validated
collection URLs. Callers cannot supply a CardDAV URL.

## Redirects and hrefs

The client disables automatic HTTP redirects. For reads, it manually follows
301, 302, 307, and 308 for at most three hops. It preserves the request method
and body.

The client resolves relative `Location` and DAV href values against the exact
response URL. It then validates HTTPS, the port, and collection containment. It
also uses a case-sensitive host allowlist. Production hosts use lowercase.

The client rejects read-side 303 and all other redirect codes. It never follows
a redirect after PUT or DELETE dispatch. Such a redirect returns
`outcome_unknown`. This rule also applies to malformed or policy-violating
redirects.

CardDAV resource hrefs are arbitrary. The client never assumes that a UID maps
to `UID.vcf`. UID lookup uses an `addressbook-query` and requires exactly one
match. The client keeps the returned href internally. It sends a full GET before
it returns or changes the contact.

## vCard model

- Reads accept vCard 3.0 and 4.0.
- Writes encode only vCard 3.0. An address book is writable when it advertises
  3.0 or does not include `supported-address-data`.
- Create includes VERSION, PRODID, UID, FN, and N.
- A caller can provide `client_uid`. Otherwise, the client generates a
  UUIDv4-compatible UID with `crypto/rand`.
- Results do not contain PHOTO bytes, raw vCards, or raw extension values. Full
  contact reads set `hasPhoto` when a PHOTO property is present.
- Thus, a caller can detect an avatar without image bytes.
- A vCard 3.0 update modifies the full decoded object, preserving PHOTO and
  unknown properties that fit the resource limit.
- vCard 4.0 objects are read-only. This rule prevents a silent downgrade through
  the 3.0 encoder.
- Apple group cards are readable and excluded from search by default. The server
  rejects group mutation.
- The result includes a birthday only when it is a valid `YYYY-MM-DD` value.
  Unsupported forms produce `unsupportedFields: ["birthday"]` without the raw
  value.

Modeled contact detail can include these fields:

- Display and structured names
- Organization, title, nickname, and birthday
- Typed email addresses, phone numbers, and URLs
- Postal addresses and notes
- ETag and address-book identifier.

Search summaries omit notes, addresses, URLs, birthdays, raw cards, and photos.

## Search behavior

`search_contacts` uses a bounded CardDAV server predicate when its text matching
has the requested meaning. A general `query` sends one any-of
FN/N/EMAIL/TEL/ORG contains filter. Without a general query, `email` uses an
EMAIL contains filter.

The client then checks each returned card locally. It combines all supplied
`query`, `email`, `phone`, and `include_groups` conditions. Thus, the server
prefilter does not define the final result.

The client normalizes digits for local phone matching. It never sends phone as
a TEL text predicate. Thus, a phone-only search sends the bounded
VERSION-presence all-card query. A compatible query or email predicate can
reduce the candidate set. The client then applies the phone condition locally.

Local matching is:

- `query`: case-insensitive substring across FN, N, EMAIL, TEL, and ORG.
- `email`: case-insensitive EMAIL substring.
- `phone`: digit-normalized TEL substring.
- `include_groups`: false by default.

All selected books share total limits of 2,000 decoded cards and 32 MiB of
REPORT responses. Results sort by normalized display name, then UID, then book.
The default result limit is 50, and the maximum is 100. There is no offset or
continuation cursor.

`truncated` means that the result count or byte limit removed matches.
`scanLimitReached` means that the scan limit excluded selected cards or books.
Narrow the book or filters when either field is true.

## Conditional writes

Create:

- Generates a random `.vcf` child resource name independently of the contact
  UID.
- Sends `Content-Type: text/vcard; charset=utf-8` and `If-None-Match: *`.
- Does not repeat PUT after a transport failure. An ambiguous transport outcome
  returns `outcome_unknown` with an instruction to read again.
- Sends another GET after definitive success to obtain normalized data and a
  fresh ETag. If GET fails, create stays successful with `resultIncomplete`.

Update/delete:

1. Query the exact UID and get the full returned resource.
2. Require a usable specific strong server ETag.
3. Use a valid caller ETag when supplied. Otherwise, use the GET ETag.
4. Reject wildcard, weak, malformed, and missing ETags.
5. Send a specific `If-Match` on every real PUT/DELETE.
6. Map HTTP 412 to `concurrent_modification`.
7. Re-GET after successful update for normalized data and ETag.

`delete_contact` dry run runs lookup and validation. It sends no DELETE.
DAV `no-uid-conflict`, `valid-address-data`, and `max-resource-size`
preconditions map to `conflict`, `validation`, and `payload_too_large`.
Unknown or malformed preconditions map to `protocol_error`. Results never
contain raw XML.

MCP `update_contact` can return a cached result for `idempotency_key`. This path
sends no new CardDAV mutation. See the
[update idempotency contract](architecture.md#update-idempotency).

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

To detect overflow, byte-limited reads read at most one byte above the limit.
The client truncates remote names and fields only at valid UTF-8 boundaries.
Truncation occurs only where the modeled read contract permits it.

The real-iCloud Contacts integration suite requires the `integration` build tag
and `ICLOUD_MCP_ENABLE_CONTACTS=true`. It tests discovery, bounded search, and
get.

A separate write gate controls its disposable CRUD fixture. The fixture tests
general query matches through FN, N, EMAIL, and ORG. It also tests exact email
search, digit-normalized phone search, UID lookup, update, and exact fixture
cleanup.

The suite is optional and never runs in CI. It does not run without credentials.
