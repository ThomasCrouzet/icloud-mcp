package icloud

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// Input validation bounds, enforced on the MCP handler side (before any
// network call) and re-checked on Client methods (defense in depth).
const (
	MaxTitleLen    = 500
	MaxLocationLen = 1000
	MaxNotesLen    = 4000
	MaxQueryLen    = 200
	MaxUIDLen      = 255
	MaxRangeDays   = 366 // bounds the search_events window (and thus expansion)
	MaxResults     = 400 // hard limit from the spec
)

// ValidateCalendarPath checks that a calendar path is a path-absolute CalDAV
// path: non-empty, starts with a single '/', no scheme-relative form (//host),
// no directory traversal (including percent-encoded), no userinfo/query/
// fragment markers, no control characters, bounded length.
//
// Scheme-relative inputs like "//evil.example/x" would otherwise pass a naive
// "starts with /" check and rewrite the host under url.ResolveReference.
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
	// Reject scheme-relative URLs (//host/...) and backslash variants.
	// Note: '@' is allowed in path segments (event UIDs often contain '@');
	// host rewrite via userinfo only applies to scheme-relative refs, already
	// rejected above.
	if strings.HasPrefix(path, "//") || strings.Contains(path, "\\") {
		return fmt.Errorf("calendar path must be path-absolute (no host or scheme)")
	}
	// Reject query/fragment markers that change URL semantics under ResolveReference.
	if strings.ContainsAny(path, "?#") {
		return fmt.Errorf("calendar path contains invalid characters")
	}
	if strings.ContainsAny(path, "\x00\n\r") {
		return fmt.Errorf("calendar path contains invalid characters")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("calendar path contains a directory traversal sequence ('..')")
	}
	// Percent-decoded form must also be free of ".." and scheme-relative shape.
	decoded := path
	if d, err := url.PathUnescape(path); err == nil {
		decoded = d
	}
	if strings.Contains(decoded, "..") {
		return fmt.Errorf("calendar path contains a directory traversal sequence ('..')")
	}
	if strings.HasPrefix(decoded, "//") || strings.Contains(decoded, "\\") {
		return fmt.Errorf("calendar path must be path-absolute (no host or scheme)")
	}
	if strings.ContainsAny(decoded, "?#\x00\n\r") {
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
	// Path separators and backslash must never appear: the UID becomes a
	// path segment (<uid>.ics). Control characters are also rejected.
	if strings.ContainsAny(uid, "\x00\n\r/%\\") {
		return fmt.Errorf("UID contains invalid characters")
	}
	for _, r := range uid {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("UID contains invalid characters")
		}
	}
	return nil
}

// ValidateIfMatchETag rejects client-supplied concurrency tokens that would
// disable optimistic locking. Empty is allowed (callers fail closed later).
// "*" would become If-Match: * (any representation), last-writer-wins.
func ValidateIfMatchETag(etag string) error {
	if etag == "" {
		return nil
	}
	if etag == "*" {
		return NewValidationError(`etag "*" is not allowed; pass the opaque etag from get_event or search_events`)
	}
	if strings.ContainsAny(etag, "\x00\r\n") {
		return NewValidationError("etag contains invalid characters")
	}
	if len(etag) > 512 {
		return NewValidationError("etag is too long")
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
// 2026-07-12, asked to create a "Deep clean" event from 10:00 to 14:00
// (Europe/Paris), the calling agent sent start=2026-07-12T10:00:00Z /
// end=2026-07-12T14:00:00Z, i.e. literal UTC. iCloud rendered that 2h later
// than intended (CEST = UTC+2) once displayed in the user's Europe/Paris
// calendar. Accepting a bare local time and resolving the DST-aware offset
// server-side (via defaultLoc, see ICLOUD_MCP_DEFAULT_TZ in internal/config)
// removes that arithmetic from the agent's job entirely; the tool description
// steers callers toward this form for "the time the user said" and reserves
// the explicit-offset form for a deliberately different timezone (e.g. a call
// with someone abroad).
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

// ParseRecurrenceID parses recurrence_id for scope=occurrence. Accepts:
//   - YYYY-MM-DD (all-day series: UTC midnight on that calendar date)
//   - the same forms as ParseDateTime for timed series
//
// Prefer YYYY-MM-DD for all-day masters: a bare local midnight in a non-UTC
// DEFAULT_TZ would otherwise shift to the previous UTC day and miss the
// RECURRENCE-ID match.
func ParseRecurrenceID(name, value string, defaultLoc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("invalid %s: empty", name)
	}
	if len(value) == 10 {
		if t, err := time.ParseInLocation("2006-01-02", value, time.UTC); err == nil {
			return t, nil
		}
	}
	return ParseDateTime(name, value, defaultLoc)
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

// ValidateRRULE checks that an RRULE string (without the "RRULE:" prefix) is
// parseable and not pathologically unbounded for write paths. COUNT/UNTIL
// is required when FREQ is SECONDLY or MINUTELY (or when neither COUNT nor
// UNTIL is set and FREQ is HOURLY) so a create cannot plant an infinite
// high-frequency series.
func ValidateRRULE(rule string) error {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return fmt.Errorf("RRULE cannot be empty")
	}
	if len(rule) > 1024 {
		return fmt.Errorf("RRULE is too long (max 1024 characters)")
	}
	if strings.HasPrefix(strings.ToUpper(rule), "RRULE:") {
		return fmt.Errorf("RRULE must not include the RRULE: prefix; pass only the value (e.g. FREQ=WEEKLY;COUNT=10)")
	}
	ropt, err := rrule.StrToROption(rule)
	if err != nil {
		return fmt.Errorf("invalid RRULE: %w", err)
	}
	freq := strings.ToUpper(ropt.Freq.String())
	hasBound := ropt.Count > 0 || !ropt.Until.IsZero()
	switch freq {
	case "SECONDLY", "MINUTELY":
		if !hasBound {
			return fmt.Errorf("RRULE with FREQ=%s requires COUNT or UNTIL (unbounded high-frequency series rejected)", freq)
		}
	case "HOURLY":
		if !hasBound {
			return fmt.Errorf("RRULE with FREQ=HOURLY requires COUNT or UNTIL")
		}
	}
	return nil
}
