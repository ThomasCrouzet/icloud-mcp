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

// ParseRFC3339 parses an RFC3339 date/time with a deliberately pedagogical
// error message (the calling agent must be able to correct its input from
// the message alone).
func ParseRFC3339(name, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s (%q): expected RFC3339 format, e.g. 2026-07-01T00:00:00Z: %w", name, value, err)
	}
	return t, nil
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
