# CalDAV / iCloud compatibility

Calendar-specific observations against real iCloud CalDAV that constrain the
client. Contacts and Mail use separate protocol clients and destination
policies; see [CardDAV compatibility](carddav-compatibility.md) and
[Mail compatibility](mail-compatibility.md).

## Discovery

- Entry: `https://caldav.icloud.com`.
- Response redirects to a shard `pXX-caldav.icloud.com` (often with explicit `:443`).
- `go-webdav` `FindCalendarHomeSet` returns a path without the shard host; this
  server uses hand-rolled PROPFIND (`discovery.go`).
- `net/http` converts 301 to GET; discovery preserves method semantics via
  allowlisted redirects and direct PROPFIND.
- Failed discovery is not cached forever (retry after transient errors).

## calendar-query REPORT

- Partial `calendar-data` with nested `<comp>` returns empty VEVENTs on iCloud.
- Only bare `<C:calendar-data/>` works reliably.
- `prop-filter` by UID returns 412; UID lookup uses GET on `<uid>.ics` then a
  bounded time-range REPORT fallback (+/-50 years around now). Events whose
  filename is not `<uid>.ics` and that lie entirely outside that window are
  reported as not found on the fallback path (error text states the window).
- Request `D:getetag` with calendar-data so If-Match works on the REPORT path.
- Imported-UID lookup always re-GETs before mutate so VERSION/PRODID/VTIMEZONE
  survive the round-trip (REPORT payloads are incomplete for go-ical encode).

## Writes

- PUT `text/calendar` objects named `<uid>.ics` for server-created events.
- Imported events may use a different filename; always resolve by UID before mutate.
- Create always sends `If-None-Match: *` so a concurrent same-UID create cannot
  silently overwrite; 412 maps to `conflict`.
- If-Match for optimistic concurrency on update/delete; 412 = concurrent modification.
  Mutations fail closed when no ETag is available.
- Update always GET full object first (preserves VERSION/PRODID/VTIMEZONE).
- Update preserves existing DTSTART/DTEND form (DATE / TZID / Z); never force UTC Z
  on a TZID series.
- Automatic retry is read-only. PUT and DELETE are never replayed, including a
  full-series delete. A transport failure or gateway 502/503/504 after dispatch
  returns `outcome_unknown` with reconciliation guidance; write-side 429 is a
  definitive `rate_limited` response. Any redirect or automatically followed
  response observed after mutation dispatch is also `outcome_unknown` and is
  never replayed.

## Recurrence

- Expand RRULE with TZID preserved (never force `.UTC()` on Dtstart).
- Handle EXDATE and RECURRENCE-ID overrides; include occurrences overlapping
  range start. Missing DTEND: derive from DURATION, preserving nominal calendar
  days/weeks across DST, else use the next civil day for all-day events.
- Cap returned expansion at 2,000 occurrences and iterator work at 100,000
  advances per series. A preflight estimate rejects pathological rules that
  would exceed the work budget before the requested range is reached.
- Scope `series` vs `occurrence` on update/delete; occurrence never deletes the
  series resource. Occurrence EXDATE/RECURRENCE-ID match the master DATE/TZID/Z form.
- Timed recurring creates write TZID + generated VTIMEZONE (explicit `timezone`
  or `ICLOUD_MCP_DEFAULT_TZ` fallback) so wall-clock RRULEs survive DST.
- `this-and-future` is **not** implemented (not proven safe end-to-end).
- RDATE and ranged `RECURRENCE-ID` (`THISANDFUTURE`) are rejected with
  `protocol_error`; they are never silently omitted from availability results.
- Date-selector reachability is preflighted over a bounded Gregorian cycle
  before entering the recurrence iterator. The non-RFC `BYEASTER` extension,
  ordinal `BYDAY` outside monthly/yearly rules, and calendar selectors combined
  with hourly/minutely/secondly frequency are rejected fail-closed because the
  dependency cannot interrupt an empty internal selector scan.

## Parser and result limits

- Inbound stdio JSON-RPC frames are capped at 1 MiB and every serialized
  Calendar/MCP result is capped at 256 KiB.
- Calendar REPORT XML is capped at depth 32, 262,144 tokens, 4,096 response
  elements, 16,384 propstats, and 32,768 properties.
- Parsed iCalendar is capped at 1,024 components, 10,000 properties total, 1,024
  properties per component, 512 overrides, 64 parameters per property, 64
  alarms, and 2,000 EXDATE values. One remote property value is capped at 1 MiB.
- PROPFIND and single-object GET bodies are capped at 8 MiB; REPORT is capped at
  32 MiB.
- A single-calendar search materializes at most 2,500 events. Multi-calendar
  `search_events` still queries every selected calendar, then fails closed at
  10,000 filtered events before the public 400-event sort-cap. Recurrence work
  is capped at 100,000 iterator advances per series and 250,000 across one
  search, including selector reachability proof work reserved before iteration.
- Calendar network concurrency is capped independently at four reads and two
  writes.

## Limits (Apple)

- ~50,000 events per calendar (403 when exceeded).
- One Calendar Apple Account per process instance. The optional Contacts domain
  uses the same configured identity through a separate client; Mail can use a
  distinct mailbox address and app-specific password.
