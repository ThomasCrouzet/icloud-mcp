# CalDAV / iCloud compatibility

These Calendar-specific observations from real iCloud CalDAV constrain the
client. Contacts and Mail use separate protocol clients and destination
policies. See [CardDAV compatibility](carddav-compatibility.md) and
[Mail compatibility](mail-compatibility.md).

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

## Recurrence

- Recurrence expansion preserves the RRULE TZID. It never calls `.UTC()` on
  Dtstart.
- Expansion handles EXDATE and RECURRENCE-ID overrides. It includes occurrences
  that overlap the range start.
- When DTEND is missing, DURATION sets the duration. Calendar days and weeks
  follow local time across DST.
- Without DURATION, an all-day event ends on the next civil day.
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
- Multi-calendar `search_events` still queries each selected calendar. It fails
  closed when more than 10,000 filtered events materialize.
- This failure occurs before the public sorted result limit of 400 events.
- Recurrence work has a limit of 100,000 iterator advances for each series. One
  search has a total limit of 250,000 advances.
- The search reserves selector reachability proof work before iteration.
- Calendar permits four concurrent reads and two concurrent writes.

## Limits (Apple)

- Approximately 50,000 events per calendar. Apple returns 403 when the limit is
  exceeded.
- One process supports one Calendar Apple Account. The optional Contacts domain
  uses the same configured identity through a separate client.
- Mail can use a different mailbox address and app-specific password.
