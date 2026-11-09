# 0003 Override-only CalDAV resources

- Status: research decision, accepted 2026-11-09.
- Issue: [#181](https://github.com/ThomasCrouzet/icloud-mcp/issues/181).
- Scope: read-only enumeration of a calendar object that contains only
  `RECURRENCE-ID` overrides, with no master VEVENT.
- Decision owner: maintainers.

## Decision

Keep the current fail-closed `protocol_error`. Do not implement read-only
enumeration in this change, and keep every write disabled.

The synthetic fixture is standards-valid. The current client rejects it before
search, get, availability, or mutation and does not return false free time.
That is a documented compatibility gap, not a silent partial result.

## Why

An override-only object has no master RRULE and no master DTSTART. Enumerating
its explicit instances is possible in principle, but the current public model
(`Event` plus `EventDetail`) assumes a master and a recurrence identity.
Serving the overrides as ordinary events would invent a master or lose the
distinction the caller needs for later mutation.

Write support is unsafe without separate evidence: there is no master ETag
semantics for a per-instance change, no recurrence-set membership to validate,
and no provider evidence that iCloud preserves or rewrites such resources.

## Reopen conditions

Open a separate implementation issue only after:

- an authorized, disposable live iCloud run records whether the provider stores,
  returns, and rewrites override-only resources;
- a read-only response model exists that exposes the typed master-absent class
  and each explicit `RECURRENCE-ID` without fabricating a master;
- time-range, byte, occurrence, scope, and redaction limits are specified;
- mutation remains separately gated by group-scheduling approval and a fresh
  strong ETag.

Until then the failure is explicit, bounded, and safe.
