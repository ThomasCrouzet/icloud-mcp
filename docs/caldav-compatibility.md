# CalDAV / iCloud compatibility

Observations against real iCloud CalDAV (2026-07) that constrain the client.

## Discovery

- Entry: `https://caldav.icloud.com`.
- Response redirects to a shard `pXX-caldav.icloud.com` (often with explicit `:443`).
- `go-webdav` `FindCalendarHomeSet` returns a path without the shard host; this server uses hand-rolled PROPFIND (`discovery.go`).
- `net/http` converts 301 to GET; discovery therefore uses a client that preserves method semantics via allowlisted redirects and direct PROPFIND.

## calendar-query REPORT

- Partial `calendar-data` with nested `<comp>` returns empty VEVENTs on iCloud.
- Only bare `<C:calendar-data/>` works reliably.
- `prop-filter` by UID returns 412; UID lookup uses GET on `<uid>.ics` then a bounded time-range REPORT fallback.
- Request `D:getetag` with calendar-data so If-Match works on the REPORT path.

## Writes

- PUT `text/calendar` objects named `<uid>.ics` for server-created events.
- Imported events may use a different filename; always resolve by UID before mutate.
- If-Match for optimistic concurrency; 412 = concurrent modification.
- Update always GET full object first (preserves VERSION/PRODID/VTIMEZONE).

## Recurrence

- Expand RRULE with TZID preserved (never force `.UTC()` on Dtstart).
- Handle EXDATE and RECURRENCE-ID overrides.
- Cap expansion at 2000 occurrences per series.
- Scope `series` vs `occurrence` on update/delete; occurrence never deletes the series resource.
- `this-and-future` is **not** implemented (not proven safe end-to-end).

## Limits (Apple)

- ~50,000 events per calendar (403 when exceeded).
- One Apple ID per process instance.
