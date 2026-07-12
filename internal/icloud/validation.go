package icloud

import (
	"fmt"
	"strings"
	"time"
)

// Input validation bounds, enforced on the MCP handler side (before any
// network call) AND on the Client side (defense in depth).
const (
	MaxTitleLen    = 500
	MaxLocationLen = 1000
	MaxNotesLen    = 4000
	MaxQueryLen    = 200
	MaxUIDLen      = 255
	MaxRangeDays   = 366 // bounds the search_events window (and thus expansion)
	MaxResults     = 400 // hard limit from the spec
)

// ValidateCalendarPath checks that a calendar path is plausible: non-empty,
// starts with '/', no directory traversal or control characters, bounded
// length.
func ValidateCalendarPath(path string) error {
	if path == "" {
		return fmt.Errorf("calendar path cannot be empty")
	}
	if len(path) > 1024 {
		return fmt.Errorf("calendar path is too long (max 1024 characters)")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("calendar path must start with '/'")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("calendar path contains a directory traversal sequence ('..')")
	}
	if strings.ContainsAny(path, "\x00\n\r") {
		return fmt.Errorf("calendar path contains invalid characters")
	}
	return nil
}

// ValidateUID checks that an event UID is plausible.
func ValidateUID(uid string) error {
	if uid == "" {
		return fmt.Errorf("UID cannot be empty")
	}
	if len(uid) > MaxUIDLen {
		return fmt.Errorf("UID is too long (max %d characters)", MaxUIDLen)
	}
	if strings.Contains(uid, "..") {
		return fmt.Errorf("UID contains a directory traversal sequence ('..')")
	}
	if strings.ContainsAny(uid, "\x00\n\r/%") {
		return fmt.Errorf("UID contains invalid characters")
	}
	return nil
}

// ValidateTextField checks the length of a free-text field
// (title/location/notes/query) and rejects NUL characters. Newlines are
// tolerated (notes may span multiple lines); go-ical properly escapes \n,
// ;, , and \ during TEXT encoding (SetText), so no iCalendar property
// injection is possible through these fields and no manual re-escaping is
// needed here.
func ValidateTextField(name, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("%s too long (max %d characters, got %d)", name, max, len(value))
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains a forbidden character (NUL)", name)
	}
	return nil
}

// naiveDateTimeLayout is the RFC3339 date-time layout stripped of the
// "Z07:00" offset designator: a local wall-clock time with no timezone
// information at all, e.g. "2026-07-01T14:00:00".
const naiveDateTimeLayout = "2006-01-02T15:04:05"

// ParseDateTime parses a date/time supplied by the calling MCP agent for a
// start/end parameter. Two forms are accepted:
//
//   - RFC3339 WITH an explicit offset ("2026-07-01T14:00:00+02:00", or
//     "...Z" for UTC): parsed literally. The offset is a deliberate,
//     self-declared choice by the caller, so it is always honored as-is,
//     including "Z" (never silently reinterpreted as "local time typed by
//     the user").
//   - A local wall-clock time with NO offset ("2026-07-01T14:00:00"):
//     interpreted in defaultLoc (nil defaults to UTC).
//
// The no-offset form exists because converting a stated local hour to the
// correct UTC offset is precisely the step an LLM agent gets wrong: on
// 2026-07-12, asked to create "Grand ménage" from 10h to 14h (Europe/Paris,
// confirmed in French with the user), the calling agent sent
// start=2026-07-12T10:00:00Z / end=2026-07-12T14:00:00Z, i.e. literal UTC.
// iCloud rendered that 2h later than intended (CEST = UTC+2) once displayed
// in the user's Europe/Paris calendar. Accepting a bare local time and
// resolving the DST-aware offset server-side (via defaultLoc, see
// ICLOUD_MCP_DEFAULT_TZ in internal/config) removes that arithmetic from
// the agent's job entirely; the tool description steers callers toward this
// form for "the time the user said" and reserves the explicit-offset form
// for a deliberately different timezone (e.g. a call with someone abroad).
func ParseDateTime(name, value string, defaultLoc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	loc := defaultLoc
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.ParseInLocation(naiveDateTimeLayout, value, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf(
		"invalid %s (%q): expected RFC3339 with an explicit offset (e.g. 2026-07-01T14:00:00+02:00, or ...Z for UTC) "+
			"or a local time with no offset (e.g. 2026-07-01T14:00:00), interpreted as %s",
		name, value, loc,
	)
}

// ValidateRange checks that end > start and that the range does not exceed
// MaxRangeDays days (which also indirectly bounds recurrence expansion).
func ValidateRange(start, end time.Time) error {
	if !end.After(start) {
		return fmt.Errorf("end date (%s) must be after start date (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	if end.Sub(start) > MaxRangeDays*24*time.Hour {
		return fmt.Errorf("date range exceeds %d days (maximum allowed)", MaxRangeDays)
	}
	return nil
}
