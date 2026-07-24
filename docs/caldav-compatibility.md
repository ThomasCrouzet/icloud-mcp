# CalDAV / iCloud compatibility

Observations against real iCloud CalDAV that constrain the client.

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
  bounded time-range REPORT fallback (±10 years around now). Events whose
  filename is not `<uid>.ics` and that lie entirely outside that window are
  reported as not found on the fallback path.
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

## Recurrence

- Expand RRULE with TZID preserved (never force `.UTC()` on Dtstart).
- Handle EXDATE and RECURRENCE-ID overrides; include occurrences overlapping
  range start. Missing DTEND: derive from DURATION, else StartTime (+24h if all-day).
- Cap expansion at 2000 occurrences per series.
- Scope `series` vs `occurrence` on update/delete; occurrence never deletes the
  series resource. Occurrence EXDATE/RECURRENCE-ID match the master DATE/TZID/Z form.
- Timed recurring creates write TZID + generated VTIMEZONE (explicit `timezone`
  or `ICLOUD_MCP_DEFAULT_TZ` fallback) so wall-clock RRULEs survive DST.
- `this-and-future` is **not** implemented (not proven safe end-to-end).
- RDATE is not expanded.

## Limits (Apple)

- ~50,000 events per calendar (403 when exceeded).
- One Apple ID per process instance.
