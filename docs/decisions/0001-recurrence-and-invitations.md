# 0001 Recurrence and invitations

- Status: research decision, accepted 2026-11-08.
- Issue: [#100](https://github.com/ThomasCrouzet/icloud-mcp/issues/100).
- Scope: Calendar only. Contacts and Mail are out of scope.
- Decision owner: maintainers.

## Decision

Do not implement RDATE, `RECURRENCE-ID;RANGE=THISANDFUTURE`, or invitation
writes in this change. Keep the current fail-closed read behavior and the
current series and occurrence write behavior.

Open implementation issues only after a live Apple account probe supplies the
missing compatibility evidence. The probe must use disposable fixtures and a
test calendar that has no attendees.

Two current behaviors need a separate safety decision before any invitation
work starts:

1. A floating iCalendar time is read as UTC. The client never reports that
   this conversion happened.
2. A series update or delete of a group-scheduled object can cause the server
   to notify attendees. The client does not detect ORGANIZER or ATTENDEE and
   does not ask for confirmation.

This record does not change the tool surface. `calendar_capabilities` keeps
`this_and_future: false` and `attendees_invitations: false`
(`internal/mcptools/calendar_capabilities.go:59-61`).

## Evidence basis

| Evidence | Source | Label |
|----------|--------|-------|
| Current read, expansion, and write behavior | Code at commit `8992a27` and the tests named in the fixture matrix | Code-verified |
| RFC requirements and formats | RFC 5545, RFC 5546, RFC 6638, RFC 4791 | RFC-verified |
| Apple iCloud live behavior | No live test was run | Apple-unverified |
| Apple scheduling endpoint support | No live test was run | Apple-unverified |
| Public endpoint claims | `README.md`, `docs/caldav-compatibility.md` | Documented |

The environment has no `ICLOUD_EMAIL`, no `ICLOUD_PASSWORD`, and no
`ICLOUD_MCP_INTEGRATION_WRITES`. No request reached an Apple server. Every
sentence about Apple behavior below is a hypothesis or a residual risk, not a
measurement.

The words in this record have these meanings:

- **Supported**: the code path exists and a test in this repository proves it.
- **Unsupported**: the code rejects the input or the tool cannot express it.
- **Technically feasible**: the RFC defines the behavior and the code has the
  data needed to implement it, but no implementation exists.
- **Apple-unverified**: the client behavior is known, but Apple server behavior
  for this case was not observed.

## Current implemented behavior

### Read path

A Calendar object goes through four stages before use:

1. `decodeRemoteCalendar` parses iCalendar with `go-ical`
   (`internal/icloud/dav.go:237`).
2. `validateRemoteCalendar` applies shape, count, and unsupported-form checks
   (`internal/icloud/ical.go:628`).
3. `validateCalendarObjectIdentity` enforces one UID, one master, unique
   RECURRENCE-ID values, and no RRULE on an override
   (`internal/icloud/ical.go:760`).
4. `parseVEvent` maps the master and each override into `Event`
   (`internal/icloud/ical.go:926`).

The GET path and the REPORT path both run all four stages
(`internal/icloud/dav.go:407-415`, `internal/icloud/client.go:1023-1030`).
Search, get, update, and delete use the same validators. A validation error
therefore stops a mutation before the PUT or DELETE.

### Supported recurrence forms

- RRULE expansion uses the `rrule-go` iterator. The code does not call `.UTC()`
  on DTSTART, so the local wall clock and the TZID survive a DST change
  (`internal/icloud/recurrence.go:80-86`).
- EXDATE is parsed from one or more comma-separated values. A TZID on the
  property resolves the value, and the result is compared at UTC second
  precision (`internal/icloud/ical.go:1031-1044`,
  `internal/icloud/recurrence.go:111-114`).
- A single-instance RECURRENCE-ID override replaces the generated occurrence
  with the same UTC instant. An override moved into the range is also returned
  (`internal/icloud/recurrence.go:178-225`).
- An override keeps the duration form of the master. A DURATION with days or
  weeks stays nominal across DST. An elapsed DURATION stays elapsed
  (`internal/icloud/ical.go:542-599`,
  `internal/icloud/recurrence.go:781-786`).
- An all-day event with no DTEND and no DURATION gets one civil day
  (`internal/icloud/ical.go:981-986`).
- Expansion stops at 2,000 occurrences and 100,000 iterator advances for one
  series. A search adds a 250,000 advance aggregate budget
  (`internal/icloud/recurrence.go:15-24`).
- A search returns an error when expansion fails. It does not return a partial
  event list or partial availability (`internal/icloud/client.go:123-128`,
  `internal/icloud/client.go:146-152`).

### Unsupported recurrence forms

- Any RDATE property in a VEVENT causes `protocol_error` with the message
  "Calendar event uses unsupported RDATE recurrence data"
  (`internal/icloud/ical.go:713-716`). The check covers DATE-TIME, DATE, and
  PERIOD forms because it rejects the property before value inspection.
- Any RECURRENCE-ID with a non-empty `RANGE` parameter causes
  `protocol_error` with the message "Calendar event uses unsupported ranged
  recurrence data" (`internal/icloud/ical.go:717-721`). The check does not
  depend on the parameter value or letter case.
- A duplicate RECURRENCE-ID, an override with its own RRULE, an override
  without a recurring master, and more than one UID all cause
  `protocol_error` (`internal/icloud/ical.go:805-830`).
- An unknown TZID on read causes `protocol_error` before the event is used
  (`internal/icloud/ical.go:942-951`). An unknown TZID on write causes
  `protocol_error` in `setDatePropLike`
  (`internal/icloud/ical.go:457-461`).
- Non-RFC `BYEASTER`, ordinal `BYDAY` outside MONTHLY or YEARLY, and sub-daily
  rules with calendar selectors cause `protocol_error`
  (`internal/icloud/recurrence.go:403-431`).

### Write path

- `update_event` and `delete_event` accept `scope=series` or
  `scope=occurrence`. Occurrence scope requires `recurrence_id`
  (`internal/icloud/client.go:346-353`,
  `internal/icloud/client.go:689-696`).
- A series update reads the complete object, changes only the requested
  fields, increments SEQUENCE, and PUTs the complete object with `If-Match`
  (`internal/icloud/client.go:360-419`). Unknown properties, VALARM
  components, ORGANIZER, and ATTENDEE are preserved.
- A new occurrence override copies only `occurrenceCopyProps`. That list does
  not include ATTENDEE or ORGANIZER
  (`internal/icloud/client.go:483-504`). The comment states the reason:
  copying them can cause invitation churn.
- An occurrence delete adds EXDATE to the master and removes a matching
  override. It never sends an HTTP DELETE for the series
  (`internal/icloud/client.go:766-832`).
- The EXDATE form follows the master DTSTART form: DATE, TZID, or Z
  (`internal/icloud/ical.go:489-516`).
- A missing ETag fails closed. An ambiguous mutation returns
  `outcome_unknown`. The client does not replay a mutation
  (`internal/icloud/client.go:405-417`).

### Invitations

The client has no invitation feature:

- `create_event` has no attendee input. Its description states that attendees
  are unsupported (`internal/mcptools/create_event.go:21`).
- No parser field exposes ORGANIZER, ATTENDEE, PARTSTAT, or SCHEDULE-STATUS.
  `Event` has no such fields (`internal/icloud/service.go:34-68`).
- The client does not read calendar-user-address-set,
  schedule-inbox-URL, schedule-outbox-URL, or SCHEDULE-AGENT.
- The client does not send iTIP messages. Mail send is a separate domain with a
  separate recipient policy.
- A series edit of an object that already has ORGANIZER and ATTENDEE keeps
  those properties and increments SEQUENCE. On a scheduling-capable server,
  RFC 6638 section 3.2.1.2 says that this change can cause a REQUEST or CANCEL.
  The client does not ask for confirmation.

## Behavior classification

| Case | Status | Evidence |
|------|--------|----------|
| RRULE expansion with TZID and wall clock across DST | Supported | `recurrence.go:80-86`, `TestResearchRecurrenceDSTSpringForwardKeepsWallClock`, `TestResearchRecurrenceDSTFallBackKeepsWallClock` |
| EXDATE with one or more values and an optional TZID | Supported | `ical.go:1016-1045`, `TestResearchRecurrenceMixedRDATEAndEXDATEFailsClosed` |
| Single-instance RECURRENCE-ID override | Supported | `recurrence.go:178-225`, `TestExpandOccurrences_OverrideReplacesOccurrence` |
| Occurrence update with a new override | Supported | `client.go:510-585`, `TestResearchRecurrenceOccurrenceUpdatePreservesMasterAndDropsSchedulingOnOverride` |
| Occurrence delete by EXDATE | Supported | `client.go:766-832`, `TestResearchRecurrenceOccurrenceDeleteAddsExdateWithoutSeriesDelete` |
| All-day DATE values and one-day default | Supported | `ical.go:579-583`, `TestResearchRecurrenceAllDayExpansionUsesDateBounds` |
| Duplicate or ambiguous override identity | Unsupported, fail-closed | `ical.go:805-830`, `TestResearchRecurrenceConflictingOverridesFailClosed` |
| RDATE in any value form | Unsupported, fail-closed | `ical.go:713-716`, `TestResearchRecurrenceMixedRDATEAndEXDATEFailsClosed` |
| `RECURRENCE-ID;RANGE=THISANDFUTURE` | Unsupported, fail-closed | `ical.go:717-721`, `TestResearchRecurrenceThisAndFutureFailsClosed` |
| RDATE read expansion | Technically feasible | RFC 5545 section 3.8.5.2, `Event` has no RDATE field |
| RDATE write and period duration | Technically feasible, larger change | RFC 5545 sections 3.3.9 and 3.8.5.2 |
| THISANDFUTURE read application | Technically feasible, high risk | RFC 5545 section 3.8.4.4 |
| THISANDFUTURE write | Technically feasible, high risk | RFC 5545 section 3.8.4.4 |
| Create an invitation with ATTENDEE and ORGANIZER | Technically feasible, not implemented | RFC 5545 sections 3.8.4.1 and 3.8.4.3 |
| Read attendee and organizer metadata | Technically feasible, not implemented | RFC 5545 sections 3.8.4.1 and 3.8.4.3 |
| Change own PARTSTAT | Technically feasible, not implemented | RFC 6638 section 3.2.8 |
| Send iTIP `REQUEST`, `REPLY`, `CANCEL`, `ADD`, or `COUNTER` | Not implemented | RFC 5546 sections 3.2.2 to 3.2.8 |
| Server-side scheduling discovery | Not implemented | RFC 6638 sections 2 and 6 |
| Apple notification behavior for a series PUT | Apple-unverified | RFC 6638 section 3.2.1.2, no live probe |
| Apple acceptance of ATTENDEE through CalDAV PUT | Apple-unverified | RFC 6638 section 3.2.3.1, no live probe |
| Apple scheduling inbox and outbox | Apple-unverified | RFC 6638 section 6, no live probe |
| Apple handling of RDATE and THISANDFUTURE from another client | Apple-unverified | RFC 5545, no live probe |
| Apple normalization of a nonexistent local time in a DST gap | Apple-unverified | No live probe |

## Recurrence semantics

### RDATE

RFC 5545 section 3.8.5.2 defines the recurrence set as the union of RRULE and
RDATE values, minus EXDATE values. RDATE values can be DATE-TIME, DATE, or
PERIOD. A PERIOD value carries an explicit start and end or duration. Duplicate
instances are ignored.

Current behavior rejects the whole object. This is the correct safe choice for
the current `Event` model. Three reasons apply:

1. The model has no RDATE field. A partial parse would drop dates.
2. The model has no per-occurrence duration for a PERIOD value. A partial
   parse would use the master duration and show wrong end times.
3. The 2,000 occurrence cap and the 100,000 advance budget are based on RRULE
   iteration. An RDATE union needs a separate count and merge rule.

A future read-only implementation must:

- parse all RDATE values with the same value-form rules as DTSTART;
- apply EXDATE after the union, as RFC 5545 requires;
- suppress duplicates between RRULE and RDATE;
- use the PERIOD end or duration for that occurrence only;
- keep the same cap, sort order, and fail-closed behavior;
- expose the source of each row only if the MCP schema needs it.

RDATE write support is a separate, larger change. It needs a new input shape,
new validation, and new tests for PERIOD and mixed forms.

### EXDATE

RFC 5545 section 3.8.5.1 says that EXDATE values take precedence over RRULE and
RDATE values. The current code applies EXDATE before override matching
(`internal/icloud/recurrence.go:111-114` and `:164-166`).

Supported behavior:

- One EXDATE property can list several comma-separated values.
- A TZID on the property resolves each value.
- A DATE value excludes an all-day occurrence.
- A Z value resolves as UTC.
- The limit is 2,000 EXDATE values for one object
  (`internal/icloud/ical.go:704-708`).

DST behavior: EXDATE matches the generated instant at UTC second precision.
This is correct for a TZID series because the generated instant already uses
the local wall clock. A fixed-offset comparison would miss the slot after a DST
change. The spring and fall tests pin this behavior.

All-day behavior: DATE values have no time zone. The comparison uses midnight
UTC. The all-day test pins this behavior.

Known limitation: the parser accepts an EXDATE whose value form differs from
DTSTART. For example, a DATE EXDATE on a DATE-TIME series is parsed as midnight
UTC and then does not match a 10:00 occurrence. The object is accepted and the
exclusion has no effect. RFC 5545 defines separate DATE and DATE-TIME EXDATE
forms, so a DATE value cannot exclude a DATE-TIME instance. This is a
low-severity input hardening item, not a time shift. See gap G4.

### RECURRENCE-ID

RFC 5545 section 3.8.4.4 defines RECURRENCE-ID as the original DTSTART of one
instance. The value type MUST match DTSTART. The value stays at the original
slot when the instance is moved.

Current behavior:

- The client matches an override to a generated occurrence by UTC second
  (`internal/icloud/recurrence.go:178-203`).
- The client returns an override that moved into the requested range even when
  its original slot is outside the range
  (`internal/icloud/recurrence.go:216-225`).
- The expanded rows expose the original slot as `recurrenceId`. This lets an
  agent target `scope=occurrence` without using the moved start
  (`internal/icloud/service.go:51-58`).
- A duplicate RECURRENCE-ID fails closed. So does an override with its own
  RRULE, an override without a recurring master, and a second UID.
- An override with a RECURRENCE-ID that does not match any RRULE slot is still
  returned when it overlaps the range. The client does not invent a slot for
  it. This is a data-preserving choice, but it can expose an orphan override
  from another client. The record treats this as observable, not silent.

DST and value form:

- A RECURRENCE-ID with `TZID` resolves in that zone. An equivalent value in
  another zone matches at the same instant.
- A DATE RECURRENCE-ID matches an all-day series.
- A floating RECURRENCE-ID matches a floating series only because both sides
  are read as UTC. See gap G1.

### Ranged THISANDFUTURE

RFC 5545 section 3.8.4.4 gives `RANGE=THISANDFUTURE` three effects:

1. The override applies to the named instance and all later instances.
2. Later instances are selected by RECURRENCE-ID, not by the new start time.
3. A start shift applies the same time difference to later instances. A
   duration change applies the same duration to later instances.

The RFC also says that overrides in separate components are not affected by
the range. That sentence creates a precedence problem: a later single-instance
override must win over the ranged override for its own slot.

The client rejects the form. That is correct for the current model. A correct
implementation needs:

- an ordered list of ranged overrides, not a map keyed only by instant;
- wall-clock delta arithmetic in the master TZID, not elapsed UTC arithmetic;
- a rule for a shift that crosses a DST transition;
- a rule for a duration change with nominal days or weeks;
- a rule for a later single-instance override inside the range;
- a rule for a range that starts before DTSTART or after the last instance;
- a write model that can create, replace, or remove a range without touching
  unrelated overrides;
- a migration rule for clients that do not understand the range.

RFC 5545 offers a safer write alternative: truncate the series with COUNT or
UNTIL and create a second series for the future. That alternative still
creates two resources and needs explicit confirmation. It is not implemented.

### DST changes

The client keeps the DTSTART location. `rrule-go` generates wall-clock values in
that location. The tests show the intended result: a 10:00 series stays at
10:00 and its UTC offset changes from +01:00 to +02:00, or from -04:00 to
-05:00.

Known limitation: a nonexistent local time in the spring gap is normalized by
Go to an earlier instant. A weekly 02:30 America/New_York series produced
01:30 on 2026-03-08 and 02:30 on the other weeks. A fixed 03:30 shift is the
common behavior in other calendaring systems. The client does not detect this
case and does not warn. See gap G3.

The autumn fold is less severe. An ambiguous 01:30 on 2026-11-01 resolves to
the first occurrence, EDT. The next week returns to 01:30 EST. The client does
not report which offset it chose. Apple behavior is unverified.

All-day events use DATE values and civil-day arithmetic. A one-day all-day
event stays one civil day across a DST change. A DURATION of days or weeks is
nominal, so a P1D event can be 23 or 25 elapsed hours
(`internal/icloud/ical.go:1194-1215`).

### Floating and local time

RFC 5545 defines a DATE-TIME with no TZID and no Z as floating time. The value
means the local time of the observer.

The read path calls `DateTime(time.UTC)` and therefore treats a floating value
as UTC (`internal/icloud/ical.go:943`). The raw parser also defaults to UTC
(`internal/icloud/ical.go:1240-1248`). `Event.Timezone` stays empty. The output
is an RFC3339 timestamp with a `Z` offset, so the caller cannot see that the
value was floating.

This is a semantic gap. A 10:00 floating event in Europe/Paris is shown as
10:00Z, which is 11:00 or 12:00 local time. A recurrence on that value uses the
wrong wall clock. The write path does preserve an existing floating form
(`internal/icloud/ical.go:471-475`,
`TestResearchRecurrenceFloatingWriteKeepsFloatingForm`), but a new event is
always written with Z or TZID. See gap G1.

### All-day events

- DTSTART and DTEND use DATE values. DTEND is exclusive.
- A missing DTEND and DURATION means one civil day.
- An all-day override keeps DATE bounds.
- An all-day EXDATE uses DATE values.
- The client never adds a time zone to a DATE value.

Apple may store an all-day event with a time zone other than UTC. The client
does not read that time zone because a DATE value has no time component. This
is standards-correct. Apple round-trip behavior is unverified.

### Conflicting overrides

| Conflict | Current result |
|----------|----------------|
| Two overrides with the same RECURRENCE-ID instant | `protocol_error` |
| Two RECURRENCE-ID values that resolve to the same instant in different zones | `protocol_error` |
| Override with its own RRULE | `protocol_error` |
| Override without a recurring master | `protocol_error` |
| Two UIDs in the same object | `protocol_error` |
| Override with an RDATE | `protocol_error` |
| Override with a RANGE parameter | `protocol_error` |
| Orphan override with no matching RRULE slot | Returned when it overlaps the range |
| EXDATE and an override for the same slot | EXDATE removes the generated slot, then the override is returned as a moved-in row |
| Ranged override plus a later single override | `protocol_error` because the range is rejected |

The EXDATE plus override case follows the RFC precedence for EXDATE. The
override remains visible because it is a separate component. A caller sees the
override, not the excluded slot. This is data-preserving. A future range
implementation must define this case explicitly.

## Invitations and scheduling

### RFC 5545 properties

- ATTENDEE (section 3.8.4.1) is a CAL-ADDRESS property. It carries ROLE,
  PARTSTAT, RSVP, CUTYPE, MEMBER, DELEGATED-TO, DELEGATED-FROM, SENT-BY, CN,
  DIR, and LANGUAGE parameters. PARTSTAT is a parameter, not a property.
- ORGANIZER (section 3.8.4.3) is a CAL-ADDRESS property. It carries CN, DIR,
  SENT-BY, and LANGUAGE parameters.
- A group-scheduled component MUST have ORGANIZER and at least one ATTENDEE.
- RECURRENCE-ID (section 3.8.4.4) can select one instance. A range parameter
  can select the instance and all later instances.
- EXDATE (section 3.8.5.1) and RDATE (section 3.8.5.2) change the recurrence
  set. They do not send scheduling messages by themselves.

### RFC 5546 iTIP

iTIP defines scheduling messages between calendar users:

- REQUEST asks an attendee to act on an event (section 3.2.2).
- REPLY reports an attendee PARTSTAT (section 3.2.3).
- ADD adds instances or attendees (section 3.2.4).
- CANCEL removes an event or instance (section 3.2.5).
- REFRESH asks the organizer for a current copy (section 3.2.6).
- COUNTER and DECLINECOUNTER negotiate a change (sections 3.2.7 and 3.2.8).

An iTIP message is a complete iCalendar object with a METHOD property. It is
not a CalDAV PUT. The client has no iTIP sender and no iTIP receiver.

### RFC 6638 CalDAV scheduling

RFC 6638 puts scheduling authority on the server:

- The server identifies organizer and attendee scheduling object resources
  (section 3.1).
- On organizer create, the server sends REQUEST when SCHEDULE-AGENT is SERVER
  or absent (section 3.2.1.1).
- On organizer modify, the server compares the old and new ATTENDEE properties
  and can send REQUEST, ADD, or CANCEL (section 3.2.1.2).
- On organizer remove, the server can send CANCEL (section 3.2.1.3).
- The server can add or change SCHEDULE-STATUS on ATTENDEE properties
  (sections 3.2.1 and 7.3).
- The server can reject a forbidden change with the
  `CALDAV:allowed-organizer-scheduling-object-change` or
  `CALDAV:allowed-attendee-scheduling-object-change` precondition
  (sections 3.2.4.3 and 3.2.4.4).
- An attendee can change only a limited set of properties, including its own
  PARTSTAT, TRANSP, alarms, and some EXDATE changes (section 3.2.2.1).
- SCHEDULE-AGENT defaults to SERVER. CLIENT and NONE suppress server sending
  (section 7.1).
- SCHEDULE-FORCE-SEND can request a message when the server would not send one
  (section 3.2.7).
- The server MUST update DTSTAMP and SEQUENCE for scheduling operations
  (section 3.2.5).
- The server sends attendee only the instances that name that attendee
  (section 3.2.6).

The last point matters for partial-series edits. An EXDATE on the master can
remove an instance for every attendee. A per-attendee exclusion needs an
override with a reduced ATTENDEE list. The current client cannot express that
distinction.

### RFC 4791 CalDAV

RFC 4791 defines calendar object resources and the PUT model:

- A PUT to an unmapped URI creates a resource. A PUT to a mapped URI replaces
  the complete resource (section 5.3.2).
- `If-None-Match: *` protects a create. `If-Match` protects an update.
- A server can advertise `CALDAV:max-attendees-per-instance` and reject an
  object that exceeds it (section 5.2.9).
- RFC 4791 does not define scheduling. RFC 6638 adds it.

The client already uses the correct PUT and conditional request model. The
missing part is the scheduling layer and the confirmation policy.

### What the documented public endpoints demonstrate

The documented public interface is CalDAV over HTTPS with HTTP Basic
authentication at `caldav.icloud.com` and `p[0-9]{1,3}-caldav.icloud.com:443`.
The client proves these operations against the documented endpoints:

- discovery of principal and calendar home set;
- PROPFIND on collections;
- calendar-query REPORT with a bare `calendar-data` request;
- GET of a complete calendar object;
- conditional PUT and DELETE;
- pass-through of unknown iCalendar properties on a series update.

Nothing in `README.md` or `docs/` documents a scheduling inbox, a scheduling
outbox, SCHEDULE-AGENT support, or ATTENDEE submission. The client does not
probe these features. Therefore this record cannot claim that iCloud supports
them.

### Apple-unverified claims

The following claims need a live account before any implementation promise:

1. iCloud accepts a PUT with ORGANIZER and ATTENDEE and creates a scheduling
   object resource.
2. iCloud sends a REQUEST on organizer create.
3. iCloud sends REQUEST, ADD, or CANCEL on organizer modify.
4. iCloud sends CANCEL on organizer delete.
5. iCloud exposes schedule-inbox-URL, schedule-outbox-URL, and
   calendar-user-address-set on the principal.
6. iCloud honors SCHEDULE-AGENT=NONE or CLIENT on an ATTENDEE property.
7. iCloud preserves or rewrites SCHEDULE-STATUS and PARTSTAT.
8. iCloud lets an attendee change only its own PARTSTAT.
9. iCloud accepts RDATE and returns it unchanged, or expands it itself.
10. iCloud accepts THISANDFUTURE and preserves it, or rewrites it.
11. iCloud normalizes a nonexistent local time in a DST gap to the same instant
    as the client.
12. iCloud returns a stable ETag and SEQUENCE after a scheduling change.

## Safety boundaries

These boundaries are mandatory for any future implementation. The current code
meets some of them and misses others, as noted.

| Boundary | Current state | Required action |
|----------|---------------|-----------------|
| Explicit confirmation for invitations | Missing | Any create or update that adds, removes, or changes an ATTENDEE must require a separate explicit confirmation input. A dry run must show the recipient list and the expected iTIP method. |
| No unintentional notifications | Partial | Detect ORGANIZER and ATTENDEE before a series update or delete. Require confirmation, or refuse the mutation. Do not use SCHEDULE-AGENT=NONE silently. |
| No irreversible series mutation | Met for RDATE and THISANDFUTURE | Keep the fail-closed rejection. Never convert a range to a full-series rewrite without confirmation. |
| Preserve unknown properties | Met for series updates | Keep the full GET and re-PUT. For a new override, document the allowlist and add any non-scheduling unknown property that is safe to copy. |
| Preserve ATTENDEE on existing overrides | Met | An existing override is edited in place. A new override drops scheduling properties by design. |
| Fail closed | Met | Keep `protocol_error` for RDATE, RANGE, duplicate identity, and unknown TZID. Never return a partial result. |
| No partial availability | Met | A recurrence error stops the search and the free-slot result. |
| No automatic retry after dispatch | Met | PUT and DELETE are never replayed. `outcome_unknown` is returned. |
| Stable sequence | Met | Every series or occurrence update increments SEQUENCE once. |
| Bounded work | Met | 2,000 occurrences, 100,000 advances per series, 250,000 per search. |

## Findings and gaps

### G1: Floating time is read as UTC

Evidence: `internal/icloud/ical.go:943` calls `p.DateTime(time.UTC)`.
`internal/icloud/ical.go:1240-1248` defaults a value with no TZID and no Z to
UTC.

Impact: a floating event is shifted by the difference between UTC and the
observer zone. A recurring floating event keeps the wrong wall clock. The
output does not show that the value was floating.

Status: unresolved. No new test asserts that this conversion is correct. The
new floating test covers only the write-side preservation.

Suggested action: add a read-side flag or reject a floating recurring master
with `protocol_error`. A safe first step is a warning in `get_event` and a
documented limitation. A full fix needs a default zone on read and a schema
field that tells the caller the value was floating.

### G2: Series mutation of a group-scheduled object can notify attendees

Evidence: `internal/icloud/client.go:414` PUTs the complete object.
`internal/icloud/client.go:396-399` increments SEQUENCE. The update path keeps
ORGANIZER and ATTENDEE. RFC 6638 section 3.2.1.2 says the server can send
REQUEST, ADD, or CANCEL for such a change. `internal/icloud/client.go:757`
deletes the complete series, and RFC 6638 section 3.2.1.3 says the server can
send CANCEL.

Impact: an agent can cause mail to attendees with a title or location edit. A
series delete can cancel a meeting for every attendee. The current tool surface
has no confirmation input and no warning.

Status: unsafe if the server implements scheduling. Apple support is
unverified, but the client cannot rely on its absence. The capability flag
already says invitations are unsupported, but that flag does not stop a series
edit of an imported invited event.

Suggested action before any invitation feature:

1. Parse ORGANIZER and ATTENDEE presence into `EventDetail` without exposing
   raw addresses. A boolean `IsGroupScheduled` is enough for the first step.
2. Refuse a series update or delete of a group-scheduled object unless a new
   explicit confirmation field is true. The dry run must return the presence of
   attendees and the expected notification risk.
3. Keep the occurrence path. An EXDATE change can also produce a message, so
   apply the same confirmation rule to an occurrence delete.
4. Add a test that a group-scheduled series update fails without confirmation
   and sends no PUT. Add a test that it proceeds with confirmation.

This is the highest-priority safety gap in this record.

### G3: Spring-gap local time moves backward

Evidence: `internal/icloud/recurrence.go:86` sets `ropt.Dtstart` to the parsed
location. `go-ical` and `time.Date` normalize a nonexistent local time. A
weekly 02:30 America/New_York series produced 01:30 on 2026-03-08.

Impact: one occurrence can move backward by one hour. The client does not warn.
The event still appears, so the failure is silent.

Status: unresolved. A test would bless a behavior that is probably not what a
user expects. The record keeps the observation without a test.

Suggested action: detect a local time that does not exist at DTSTART and at
each generated occurrence. Return `protocol_error` for a recurring master that
starts in a gap, and add a warning for a gap inside the range. Apple behavior
is unverified.

### G4: EXDATE value-form mismatch is accepted and ignored

Evidence: `internal/icloud/ical.go:1031-1044` parses a DATE value as midnight
UTC. The exclusion then does not match a DATE-TIME occurrence.

Impact: a malformed EXDATE has no effect. No event time changes.

Status: low severity. No test blesses the silent ignore.

Suggested action: compare the EXDATE value type with DTSTART and fail closed on
a mismatch. This is a small parser change with a focused test.

### G5: A new override drops unknown and scheduling properties

Evidence: `internal/icloud/client.go:487-504` copies only the allowlist. The
test `TestResearchRecurrenceOccurrenceUpdatePreservesMasterAndDropsSchedulingOnOverride`
pins the result.

Impact: a new override loses ORGANIZER, ATTENDEE, and any unknown property.
This is deliberate for the current no-invitation scope. On a scheduling server,
an override without ATTENDEE can change who receives an instance update. RFC
6638 section 3.2.6 says the server sends an attendee only the instances that
name that attendee.

Status: deliberate tradeoff, but it needs review before invitation support.

Suggested action: keep ATTENDEE and ORGANIZER out of a new override only while
invitations are unsupported. When invitations are supported, copy the
attendee list and add an explicit confirmation step.

### G6: Orphan overrides are visible

Evidence: `internal/icloud/recurrence.go:216-225` returns an override that does
not match a generated slot when it overlaps the range.

Impact: an event from another client with a stale RECURRENCE-ID appears as an
extra row. This preserves data and does not hide an event. It can surprise a
caller.

Status: acceptable. Document it and keep it.

## Complexity, test coverage, and architecture

### Complexity estimate

| Work item | Complexity | Main touch points | Main risk |
|-----------|------------|-------------------|-----------|
| RDATE read expansion | Medium | `ical.go` parser, `recurrence.go` merge, `service.go` fields | PERIOD duration, duplicate suppression, cap accounting |
| RDATE write | Large | new input shape, validation, `buildEventCalendar`, MCP schema | mixed forms and round-trip fidelity |
| THISANDFUTURE read | Large | ordered range list, expansion, conflict rules | DST delta, nominal duration, later overrides |
| THISANDFUTURE write | Large | range replace and remove, ETag, confirmation | irreversible multi-instance change |
| Group-scheduled edit guard | Small to medium | `EventDetail`, `UpdateEvent`, `DeleteEvent`, MCP schema | false positives, confirmation UX |
| Invitation read metadata | Medium | parser, `EventDetail`, redaction, schema | address privacy, PARTSTAT semantics |
| Invitation create and update | Large | confirmation flow, ATTENDEE model, scheduling discovery | live server authority, notifications |
| iTIP sender or receiver | Out of scope | new protocol client | duplicate delivery, security |

The estimated code change for the guard is small. The estimated code change for
a full invitation feature is large and depends on server behavior that this
environment cannot observe.

### Test coverage

The new tests in `internal/icloud/recurrence_research_test.go` cover the safe
current behavior for the fixture matrix. They use synthetic fixtures and the
in-process CalDAV mock. They need no network and no credentials.

Existing tests that also cover matrix rows:

- `TestExpandOccurrences_PreservesTimezoneAcrossDST`
- `TestExpandOccurrences_OverrideReplacesOccurrence`
- `TestOccurrenceDurationPreservesNominalDaysAcrossDST`
- `TestOccurrenceAllDayWithoutEndKeepsDateBounds`
- `TestOccurrenceTimedWithoutEndKeepsZeroDuration`
- `TestUpdateEventOccurrenceDurationUsesConditionalPut`
- `TestUnsupportedRecurrenceFormsFailClosed`
- `TestCalendarObjectRejectsAmbiguousOverrideIdentity`
- `TestParseExDateProp_MultipleCommaSeparated`

A live probe can add integration tests only in `integration_test.go`, guarded
by the existing account variables and the write gate. It must use a disposable
calendar and must not invite a real person.

### Architectural impact

This record changes no production code and no tool schema. The `Service`
interface stays the same. A future RDATE or range feature adds internal fields
to `Event` and new validation branches. A future invitation feature adds a new
capability, new redaction rules for addresses, and a confirmation input. The
current domain boundary, rate limits, ETag rules, and retry policy stay
unchanged.

## Proposed split after compatibility evidence

Do not open these issues now. Each one needs the named evidence first.

### Candidate A: RDATE read-only expansion

Blocked on: a live probe that shows whether iCloud returns RDATE unchanged and
whether Apple writes RDATE for a normal series.

Acceptance tests:

- RRULE plus RDATE union, with duplicate suppression.
- EXDATE wins over RRULE and RDATE.
- DATE, DATE-TIME, and PERIOD values.
- TZID and Z forms, with a DST transition.
- All-day and mixed all-day and timed values.
- 2,000 occurrence cap and 100,000 advance cap.
- No partial result on overflow.

Protocol references: RFC 5545 sections 3.3.9, 3.8.5.1, and 3.8.5.2.

### Candidate B: THISANDFUTURE read application or permanent rejection

Blocked on: a live probe that shows how iCloud stores a range from Apple
Calendar and from another client, and how it handles a later single override.

Acceptance tests:

- Start shift before, across, and after a DST transition.
- Duration change with elapsed and nominal durations.
- Later single override inside the range wins.
- Range start before DTSTART and after the last instance.
- Range plus EXDATE.
- All-day range.
- Fail closed on an ambiguous or conflicting range.

Protocol reference: RFC 5545 section 3.8.4.4.

A decision to keep permanent rejection is acceptable if the live probe shows
that Apple clients rewrite ranges into separate series or overrides.

### Candidate C: group-scheduled edit confirmation guard

Blocked on: nothing. This is a safety fix and can proceed before invitation
support.

Acceptance tests:

- A series update of an object with ORGANIZER and ATTENDEE fails without
  confirmation and sends no PUT.
- The same update proceeds with confirmation and sends one conditional PUT.
- A series delete follows the same rule and sends no DELETE without
  confirmation.
- An object without ORGANIZER and ATTENDEE is unchanged.
- The dry run reports attendee presence without raw addresses.
- Redaction removes addresses from errors and logs.

Protocol reference: RFC 6638 sections 3.2.1.2 and 3.2.1.3.

### Candidate D: invitation read metadata and attendee submission

Blocked on: a live probe of scheduling discovery, organizer create, attendee
reply, and notification behavior.

Acceptance tests:

- Read ORGANIZER and ATTENDEE counts and PARTSTAT without raw addresses in
  public output.
- Create with one attendee after explicit confirmation.
- The server response is re-read after create because SCHEDULE-STATUS can
  change.
- A rejected scheduling change maps to a structured error.
- No automatic retry after an ambiguous submission.
- No duplicate REQUEST after a timeout.

Protocol references: RFC 5545 sections 3.8.4.1 and 3.8.4.3, RFC 5546 sections
3.2.2 and 3.2.3, RFC 6638 sections 3.2, 3.2.4, and 7.

## Fixture matrix

All new tests are in `internal/icloud/recurrence_research_test.go`. Run them
with:

```bash
go test ./internal/icloud -run 'Research|Recurrence|RDATE|ThisAndFuture' -count=1
```

| Row | Scenario | Fixture | Test | Current result |
|-----|----------|---------|------|----------------|
| 1 | DST spring, valid wall clock | Europe/Paris weekly 10:00, COUNT=4, TZID EXDATE | `TestResearchRecurrenceDSTSpringForwardKeepsWallClock` | 3 rows, wall clock 10:00, offset +01:00 then +02:00 |
| 2 | DST fall, valid wall clock | America/New_York weekly 10:00, COUNT=3 | `TestResearchRecurrenceDSTFallBackKeepsWallClock` | 3 rows, wall clock 10:00, offset -04:00 then -05:00 |
| 3 | DST spring gap | Nonexistent 02:30 local start or occurrence | No test, gap G3 | Observed 01:30 on the gap day; Apple-unverified |
| 4 | DST fall fold | Ambiguous 01:30 local start | No test, residual risk | Observed first 01:30 EDT; Apple-unverified |
| 5 | All-day and EXDATE | DATE DTSTART, COUNT=4, DATE EXDATE | `TestResearchRecurrenceAllDayExpansionUsesDateBounds` | 3 rows, one civil day each |
| 6 | All-day override | DATE master, title-only occurrence update | `TestOccurrenceAllDayWithoutEndKeepsDateBounds` | DATE bounds and next civil day |
| 7 | Conflicting overrides | Duplicate RECURRENCE-ID, own RRULE, no master, second UID | `TestResearchRecurrenceConflictingOverridesFailClosed` | `protocol_error` |
| 8 | Mixed RDATE and EXDATE | RRULE, EXDATE, then RDATE DATE-TIME, DATE, PERIOD, TZID | `TestResearchRecurrenceMixedRDATEAndEXDATEFailsClosed` | EXDATE alone gives 3 rows; RDATE gives `protocol_error` |
| 9 | Ranged THISANDFUTURE | UTC, TZID, DATE, lower-case RANGE | `TestResearchRecurrenceThisAndFutureFailsClosed` | `protocol_error` |
| 10 | Partial series update | Invited master, occurrence title change | `TestResearchRecurrenceOccurrenceUpdatePreservesMasterAndDropsSchedulingOnOverride` | Master keeps RRULE, ORGANIZER, ATTENDEE, X-property; new override drops them |
| 11 | Partial series delete | Invited master, occurrence delete | `TestResearchRecurrenceOccurrenceDeleteAddsExdateWithoutSeriesDelete` | One PUT with EXDATE, no DELETE, master properties kept |
| 12 | Invited series update | ORGANIZER, ATTENDEE, X-property, SEQUENCE | `TestResearchInvitationsSeriesUpdatePreservesSchedulingAndUnknownProperties` | Full re-PUT keeps properties and increments SEQUENCE |
| 13 | Unknown TZID | `TZID=Custom/Nowhere` on read and write | `TestResearchRecurrenceUnknownTZIDFailsClosed` | `protocol_error` on both paths |
| 14 | Floating write | Floating DTSTART during update | `TestResearchRecurrenceFloatingWriteKeepsFloatingForm` | No TZID and no Z added |
| 15 | Floating read | Floating DTSTART with RRULE | No test, gap G1 | Read as UTC; Apple-unverified |

## Residual unverified claims

1. No live iCloud account was used. No Apple behavior in this record is
   measured.
2. Apple may reject, accept, or rewrite RDATE and THISANDFUTURE.
3. Apple may or may not implement RFC 6638 scheduling on the public CalDAV
   endpoints.
4. Apple may or may not send notifications for a series PUT or DELETE that
   keeps ORGANIZER and ATTENDEE.
5. Apple may use a different DST gap or fold rule.
6. Apple may return different ETag, SEQUENCE, or SCHEDULE-STATUS values after a
   scheduling change.
7. The public documentation does not state an attendee limit or a scheduling
   limit. RFC 4791 section 5.2.9 defines a limit property, but the client does
   not read it.
8. A third-party CalDAV client can write a shape that this client reads in a
   way that differs from the writer's intent. Orphan overrides and orphan
   EXDATE values are the known examples.

## References

- RFC 5545, Internet Calendaring and Scheduling Core Object Specification
  (iCalendar), sections 3.3.9, 3.8.4.1, 3.8.4.3, 3.8.4.4, 3.8.5.1, and
  3.8.5.2.
- RFC 5546, iCalendar Transport-Independent Interoperability Protocol (iTIP),
  sections 3.2.2 to 3.2.8.
- RFC 6638, Scheduling Extensions to CalDAV, sections 3.1, 3.2, 3.2.1,
  3.2.2, 3.2.4, 3.2.5, 3.2.6, 3.2.7, 7.1, and 7.3.
- RFC 4791, Calendaring Extensions to WebDAV (CalDAV), sections 4.1, 5.2.9,
  and 5.3.2.
- `docs/caldav-compatibility.md`, Recurrence section.
- `internal/icloud/recurrence.go`, `internal/icloud/ical.go`,
  `internal/icloud/client.go`, and
  `internal/icloud/recurrence_research_test.go`.
