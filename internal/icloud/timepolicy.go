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
// YYYY-MM-DD (UTC calendar date). Timed events use RFC3339 with the
// original location offset when available, else the stored offset.
func FormatEventTime(t time.Time, allDay bool) string {
	if t.IsZero() {
		return ""
	}
	if allDay {
		return t.UTC().Format("2006-01-02")
	}
	return t.Format(time.RFC3339)
}

// ResolveLocation returns loc or UTC when loc is nil.
func ResolveLocation(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
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
