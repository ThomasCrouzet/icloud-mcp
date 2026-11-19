package icloud

import (
	"fmt"
	"strings"
	"time"
)

// Time policy: centralized parsing and formatting rules for all MCP date
// inputs and iCalendar boundary handling. See ParseDateTime for agent-facing
// rules; FormatEventTime for stable output.

// FormatEventTime formats t for MCP JSON responses. All-day dates use
// YYYY-MM-DD (UTC calendar date). Timed events use RFC3339 with an explicit
// numeric offset in loc (ICLOUD_MCP_DEFAULT_TZ). A nil loc selects UTC. The
// output still uses +00:00 instead of Z. Thus, agents do not receive mixed Z
// and offset forms.
func FormatEventTime(t time.Time, allDay bool, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if allDay {
		return t.UTC().Format("2006-01-02")
	}
	local := t.In(ResolveLocation(loc))
	return local.Format("2006-01-02T15:04:05-07:00")
}

// ResolveLocation returns loc or UTC when loc is nil.
func ResolveLocation(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// WallClockOf returns the local wall-clock components of t as a UTC time.
// A floating iCalendar value is represented with this form so its components
// survive independent of the process timezone.
func WallClockOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// ResolveWallClock converts naive wall-clock components to an instant in loc
// with the RFC 5545 section 3.3.5 policy: a fold uses the first occurrence
// (earlier offset), and a nonexistent spring-gap time uses the pre-gap offset
// (which moves it forward). It reports whether the value fell in a gap.
func ResolveWallClock(wall time.Time, loc *time.Location) (time.Time, bool) {
	loc = ResolveLocation(loc)
	wallUTC := WallClockOf(wall)
	_, before := wallUTC.Add(-24 * time.Hour).In(loc).Zone()
	_, after := wallUTC.Add(24 * time.Hour).In(loc).Zone()

	var candidates []time.Time
	for _, offset := range uniqueOffsets(before, after) {
		instant := wallUTC.Add(-time.Duration(offset) * time.Second)
		local := instant.In(loc)
		if !sameWallClock(local, wallUTC) {
			continue
		}
		if _, effective := local.Zone(); effective != offset {
			continue
		}
		candidates = append(candidates, instant)
	}
	if len(candidates) == 0 {
		// Nonexistent local time. RFC 5545 uses the UTC offset before the gap.
		return wallUTC.Add(-time.Duration(before) * time.Second).In(loc), true
	}
	// Fold: the same wall clock maps to two instants. Choose the first
	// occurrence, as RFC 5545 requires. Sort defensively because Go does not
	// guarantee which offset time.Date selects.
	first := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.Before(first) {
			first = candidate
		}
	}
	return first.In(loc), false
}

func uniqueOffsets(offsets ...int) []int {
	out := make([]int, 0, len(offsets))
	for _, offset := range offsets {
		seen := false
		for _, existing := range out {
			if existing == offset {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, offset)
		}
	}
	return out
}

func sameWallClock(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd &&
		a.Hour() == b.Hour() && a.Minute() == b.Minute() && a.Second() == b.Second() &&
		a.Nanosecond() == b.Nanosecond()
}

// WallClockExists reports whether wall exists in loc without a gap.
func WallClockExists(wall time.Time, loc *time.Location) bool {
	_, adjusted := ResolveWallClock(wall, loc)
	return !adjusted
}

// ValidateNamedTZIDRoundTrip checks that instant survives a write as a bare
// TZID wall time. A TZID value carries no offset, so a reader resolves the wall
// clock again with the first-fold policy. A second-fold instant therefore
// re-reads at a different instant, and a caller that expects the requested time
// would be wrong by the fold offset. UTC and nil locations keep absolute
// instants and always pass. field names the rejected input in the message.
//
// A nonexistent local time cannot come from a real instant: the wall clock of
// any instant exists in its own location. Gap input is rejected earlier, when a
// text value is parsed into an instant (ParseDateTime).
func ValidateNamedTZIDRoundTrip(field string, instant time.Time, loc *time.Location) error {
	if loc == nil || loc == time.UTC || instant.IsZero() {
		return nil
	}
	wall := WallClockOf(instant.In(loc))
	resolved, adjusted := ResolveWallClock(wall, loc)
	if adjusted {
		return NewValidationError(field + " does not exist in timezone " + loc.String() + " because of a daylight-saving gap")
	}
	if resolved.Equal(instant) {
		return nil
	}
	err := NewValidationError(field + " " + wall.Format("2006-01-02T15:04:05") + " in timezone " + loc.String() +
		" does not round-trip: a TZID value cannot encode the second occurrence of a daylight-saving fold")
	err.Details = map[string]string{
		"reconciliation": "Select an unambiguous instant, for example one outside the repeated hour, or send an offset-free local time so the first fold applies. Use get_event to confirm the stored value.",
	}
	return err
}

// WallClockInLocation reinterprets the wall-clock components of t in loc.
// It is used only when a floating value must be compared with an instant in a
// caller-selected timezone; it never changes the stored floating form.
func WallClockInLocation(t time.Time, loc *time.Location) time.Time {
	resolved, _ := ResolveWallClock(WallClockOf(t), loc)
	return resolved
}

// FormatEventTimeForm formats an event value using its iCalendar form. A
// floating value uses an offset-free local form; an all-day value uses a civil
// date; every other value keeps the RFC3339 offset contract.
func FormatEventTimeForm(t time.Time, allDay, floating bool, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if allDay {
		return t.UTC().Format("2006-01-02")
	}
	if floating {
		return WallClockOf(t).Format(naiveDateTimeLayout)
	}
	return FormatEventTime(t, false, loc)
}

// NormalizeAllDayBounds ensures exclusive end is strictly after start for
// VALUE=DATE events. If end is not after start, end becomes start+24h.
func NormalizeAllDayBounds(start, end time.Time) (time.Time, time.Time) {
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	if !endDay.After(startDay) {
		endDay = startDay.Add(24 * time.Hour)
	}
	return startDay, endDay
}

// ParseOptionalDateTime is like ParseDateTime but treats empty value as zero time.
func ParseOptionalDateTime(name, value string, defaultLoc *time.Location) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	return ParseDateTime(name, value, defaultLoc)
}

// AmbiguousDSTWarning returns a non-empty warning when the local wall-clock
// representation of t may be ambiguous or nonexistent around a DST transition
// in loc. Go's ParseInLocation picks one interpretation; we surface a warning
// rather than failing hard.
func AmbiguousDSTWarning(t time.Time, loc *time.Location) string {
	if t.IsZero() || loc == nil || loc == time.UTC {
		return ""
	}
	local := t.In(loc)
	wall := local.Format(naiveDateTimeLayout)
	again, err := time.ParseInLocation(naiveDateTimeLayout, wall, loc)
	if err != nil {
		return fmt.Sprintf("time %s may be invalid in %s", wall, loc)
	}
	// If converting again back to the same location yields a different
	// instant, the wall time is in a fold/gap.
	if !again.Equal(local) {
		return fmt.Sprintf("time %s may be ambiguous or nonexistent around a DST transition in %s", wall, loc)
	}
	return ""
}
