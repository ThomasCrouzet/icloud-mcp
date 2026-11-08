# CalDAV / iCloud compatibility

These Calendar-specific observations from real iCloud CalDAV constrain the
client. Contacts and Mail use separate protocol clients and destination
policies. See [CardDAV compatibility](carddav-compatibility.md) and
[Mail compatibility](mail-compatibility.md).

## Compatibility summary

| Item | Value |
|------|-------|
| Protocol | CalDAV over HTTPS |
| Endpoints | `caldav.icloud.com` and `p[0-9]{1,3}-caldav.icloud.com:443` |
| Authentication | HTTP Basic with `ICLOUD_EMAIL` and `ICLOUD_PASSWORD` (app-specific password) |
| Writes | Conditional PUT/DELETE with a strong `If-Match`; create uses `If-None-Match: *` |
| Recurrence | RRULE, EXDATE, and RECURRENCE-ID with TZID; at most 2,000 occurrences |
| Limits | 366-day range; 400 results; 60 reads and 20 writes per minute |

## Discovery

- Entry: `https://caldav.icloud.com`.
- The response redirects to a `pXX-caldav.icloud.com` shard. It often includes
  the explicit port `:443`.
- `go-webdav` `FindCalendarHomeSet` returns a path without the shard host. The
  server uses custom PROPFIND code in `discovery.go`.
- `net/http` converts 301 to GET. Discovery keeps the method through allowlisted
  redirects and direct PROPFIND requests.
- The server does not cache a failed discovery. A later call can retry after a
  transient error.
- Discovery requires exactly one successful principal and one calendar home set.
  Multiple values return `protocol_error`.

### Calendar selection for search and availability

`search_events` and `find_free_slots` use the same `calendar.read` selection
boundary. Implicit selection retains only permitted discovered paths before
event queries. No permitted calendar returns `authorization`, not availability.
An explicitly requested forbidden path rejects the entire selection before queries.
Default unscoped and wildcard read scopes retain access to all discovered calendars.

Explicit selections retain their ten-unique-path limit and remove duplicates.
Discovery does not inherit that limit. Search processes `calendar` before
`calendars`; availability processes `calendars` before `calendar`.
Permitted discovery order stays unchanged. Search falls back to discovery for
empty selections. Availability rejects explicitly empty selections.

## calendar-query REPORT

- Partial `calendar-data` with nested `<comp>` returns empty VEVENTs on iCloud.
- Only a bare `<C:calendar-data/>` request works reliably.
- A UID `prop-filter` returns 412. UID lookup first sends GET for `<uid>.ics`.
- If the resource is missing, lookup sends a bounded time-range REPORT. The
  range is 50 years before and after the current time.
- The fallback does not find an event outside this range if its filename is not
  `<uid>.ics`. The error text states this range.
- Request `D:getetag` with calendar-data. This ETag enables If-Match on the
  REPORT path.
- Imported-UID lookup always sends another GET before a mutation. This step
  preserves VERSION, PRODID, and VTIMEZONE.
- REPORT payloads are incomplete for encoding with go-ical.

## Event text

- Event reads decode iCalendar TEXT in SUMMARY, LOCATION, and DESCRIPTION as
  title, location, and notes. This includes recurrence overrides.
- Explicit TEXT value types are case-insensitive. Reading does not change stored properties.
- Escaped commas, semicolons, backslashes, and newlines become ordinary text.
  Unicode stays unchanged. Missing and empty properties produce empty fields.
- `search_events` matches query text against these decoded fields.
  The successful `delete_event` response also decodes `deletedTitle`, including dry runs.
- Invalid TEXT escapes or value types return `protocol_error` when read.
  An invalid delete title fails before mutation. Errors do not include remote text.

## Writes

- Use PUT to store server-created events as `text/calendar` objects named
  `<uid>.ics`.
- Imported events can use a different filename. Always resolve the UID before a
  mutation.
- Create always sends `If-None-Match: *`. Thus, a concurrent create with the
  same UID cannot overwrite an event silently.
- HTTP 412 from create maps to `conflict`.
- Update and delete use If-Match for optimistic concurrency. HTTP 412 maps to
  `concurrent_modification`.
- A mutation fails closed when no ETag is available.
- Update always gets the full object first. This step preserves VERSION,
  PRODID, and VTIMEZONE.
- Update preserves the existing DTSTART and DTEND form: DATE, TZID, or Z.
- Never force UTC Z on a TZID series.
- Automatic retries apply only to reads. The client never repeats PUT, DELETE,
  or a full-series delete.
- A transport failure after dispatch returns `outcome_unknown` with
  reconciliation guidance. Gateway 502, 503, and 504 have the same result.
- A write-side 429 is a definitive `rate_limited` response.
- A redirect after mutation dispatch also returns `outcome_unknown`. An
  automatically followed response observed after dispatch has the same result.
- The client never follows or repeats that mutation.

MCP `update_event` can return a cached result for `idempotency_key`. This path
sends no new CalDAV mutation. See the
[update idempotency contract](architecture.md#update-idempotency).

### Validated location cache

The client keeps at most 1,024 process-local resource locations. A key is the
validated calendar path plus the exact UID. Search populates it only from
validated CalendarObject responses. A lookup first tries the cached location,
then the `<uid>.ics` path, then the bounded REPORT fallback.

A cached path returning 404 is evicted and uses the normal fallback. A UID
mismatch or a resource outside its calendar fails closed. A confirmed series
delete invalidates the entry. An ambiguous delete does not. The cache stores no
ETag and grants no mutation permission: every write still reads and validates
the complete object. Identical UIDs in different calendars stay isolated.

## Recurrence

- Recurrence expansion preserves the RRULE TZID. It never calls `.UTC()` on
  Dtstart.
- Expansion handles EXDATE and RECURRENCE-ID overrides. It includes occurrences
  that overlap the range start.
- Each override must have a unique recurrence identifier and a recurring master.
  Ambiguous objects return `protocol_error` before use or mutation.
- When DTEND is missing, DURATION sets the duration. Calendar days and weeks
  follow local time across DST.
- Without DURATION, an all-day event ends on the next civil day.
- An occurrence override copies the master duration form. A DURATION with days
  or weeks stays nominal across DST. An elapsed DURATION stays elapsed.
- A title-only occurrence update does not change its start or end.
- A start-only occurrence update keeps the inherited duration. An all-day
  override keeps DATE bounds, and a timed event without an end keeps zero
  duration.
- Expansion returns at most 2,000 occurrences. The iterator advances at most
  100,000 times for each series.
- A preflight estimate rejects a rule that would exceed the work budget before
  the requested range.
- Update and delete accept `series` or `occurrence` scope. Occurrence scope never
  deletes the series resource.
- Occurrence EXDATE and RECURRENCE-ID values match the master DATE, TZID, or Z
  form.
- A timed recurring create writes TZID and a generated VTIMEZONE. It uses the
  explicit `timezone` or the `ICLOUD_MCP_DEFAULT_TZ` fallback.
- This method keeps wall-clock RRULEs correct across DST.
- The client does not implement `this-and-future`. Its end-to-end safety is not
  proven.
- The client rejects RDATE and ranged `RECURRENCE-ID` (`THISANDFUTURE`) with
  `protocol_error`. Availability results never omit them silently.
  See [decision 0001](decisions/0001-recurrence-and-invitations.md) for the
  RFC-grounded assessment, safety gaps, and fixture matrix.
- A preflight check tests date-selector reachability over a bounded Gregorian
  cycle. The check runs before the recurrence iterator.
- The client rejects the non-RFC `BYEASTER` extension.
- It also rejects ordinal `BYDAY` outside monthly or yearly rules.
- It rejects calendar selectors with hourly, minutely, or secondly frequency.
- These cases fail closed because the dependency cannot stop an empty internal
  selector scan.

## Parser and result limits

- Inbound stdio JSON-RPC frames have a 1 MiB limit. Each serialized Calendar or
  MCP result has a 256 KiB limit.
- Calendar REPORT XML has these limits: depth 32, 262,144 tokens, 4,096 response
  elements, 16,384 propstats, and 32,768 properties.
- Parsed iCalendar has 1,024 components and 10,000 total properties at most.
- Each component has at most 1,024 properties. Other limits are 512 overrides,
  64 parameters per property, 64 alarms, and 2,000 EXDATE values.
- One remote property value has a 1 MiB limit.
- PROPFIND and single-object GET bodies have an 8 MiB limit. REPORT has a 32 MiB
  limit.
- A single-calendar search materializes at most 2,500 events.
- Multi-calendar `search_events` still queries each selected calendar. It uses
  at most four concurrent reads in one request. The domain-wide semaphore still
  caps all concurrent MCP requests at four. It fails closed when more than
  10,000 filtered events materialize.
- `find_free_slots` accepts at most 10,000 returned events across permitted
  calendars, for explicit and implicit selections. It counts before busy filtering.
- Availability reads ordered waves of at most four calendars. It checks each
  batch before accumulation and releases wave results before the next reads.
- Overflow returns `payload_too_large` with guidance to narrow the range or calendars.
  No partial availability is returned. Incomplete searches and truncated recurrence also fail closed.
- Cancellation joins the active wave. Errors follow selected-calendar order,
  not completion order. Availability stops before starting another wave on failure.
- Results, calendar-index warnings, and free-slot output keep input order.
- Every search row carries the validated calendar path and the recurrence
  identifier when applicable. The internal resource href stays private.
- Sort order is start, UID, calendar, then title. Identical events from
  different calendars therefore keep a deterministic order.
- This failure occurs before the public sorted result limit of 400 events.

### Bounded cursor continuation

`search_events` has two explicit modes. A call without `cursor` keeps the legacy
response. It keeps the legacy ordering, duplicate handling, offset meaning and
first-400 row window. It never returns `nextCursor`.

A call with `cursor: "start"` opts into bounded keyset paging. It needs a
nonempty explicit `calendar` or `calendars` selection and `offset: 0`. `limit`
stays 1 to 400 and can change between pages. Empty or malformed cursor values
are validation errors.

Each continuation is a stateless `v1` token with a base64url payload and an
HMAC-SHA256 signature. The payload holds only a positive expiry, a query digest,
and a last-row identity digest. It never holds a raw query, UID, title, calendar
path, ETag, or internal href. The 32-byte key lives for one registered handler.
A restart or new registration invalidates all tokens. A token expires 300
seconds after issuance and can be reused as a read until then. It is not
encrypted, a lease, or a single-use credential.

The token binds the normalized range, explicit selected calendars, result
filters, default presence, compact and recurrence modes, and the current
`calendar.read` scope. `limit` is not bound. The explicit selection and scope,
token structure, signature, expiry, and query binding are checked before any
Calendar backend access. A missing resume anchor is stale source: it needs one
normal bounded fetch to detect, with no rescan or amplification.

Cursor mode keeps the primary order start, UID, calendar, then title. It adds
recurrence ID, end time, override flag, internal path, ETag, and a full-row
digest as strict tie-breaks. Exact duplicate backend rows in one calendar are
collapsed only in this opt-in mode. Distinct calendars and recurring overrides
stay distinct. Legacy duplicate handling does not change.

Every page still queries each selected calendar with at most four concurrent
reads. It keeps the 2,500 expanded-event per-calendar limit, the 10,000 filtered
materialized limit, the 366-day range, ten explicit calendars, the 32 MiB REPORT
limit, and all recurrence work caps. More than 2,500 expanded events in one
calendar fails closed before paging. Pagination does not permit loading a whole
history.

`nextCursor` is emitted only when an explicit selection has a complete page with
at least one later row. It is omitted on `partialFailure`, recurrence truncation,
materialization overflow, authentication, authorization, protocol or payload
errors, cancellation, and deadline. In cursor mode `truncated` is true when rows
remain or the result is incomplete. A result beyond the 256 KiB serialized
budget uses only rows that fit with the token and metadata. If zero rows fit, it
returns no cursor and asks for a narrower range, compact mode, or fewer
calendars.

`total` is the current bounded fetch count, not a snapshot. If the last row
identity disappears or changes, the cursor is stale and the caller restarts.
Rows inserted before the anchor can be omitted. A row moved from before to after
the anchor can appear again. A row moved after to before can be omitted. No
snapshot isolation or exhaustive provider coverage is claimed.
- Recurrence work has a limit of 100,000 iterator advances for each series. One
  search has a total limit of 250,000 advances.
- The search reserves selector reachability proof work before iteration.
- Calendar permits four concurrent reads and two concurrent writes.

Working hours require exact ASCII `HH:MM` values from `00:00` through `24:00`.
Invalid hours or weekdays return a validation error before free-slot calculation.

## Limits (Apple)

- Approximately 50,000 events per calendar. Apple returns 403 when the limit is
  exceeded.
- One process supports one Calendar Apple Account. The optional Contacts domain
  uses the same configured identity through a separate client.
- Mail can use a different mailbox address and app-specific password.
