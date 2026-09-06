package icloud

import (
	"context"
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
	MaxURLLen      = 2000
	MaxRangeDays   = 366 // bounds the search_events window (and thus expansion)
	MaxResults     = 400 // hard limit from the spec
)

// ValidateCalendarPath checks a path-absolute CalDAV path. The path must be
// nonempty, bounded, and start with one '/'. It must not contain a
// scheme-relative form, directory traversal, URL metadata, or control
// characters. The traversal check includes percent-encoded forms. URL metadata
// includes userinfo, query, and fragment markers.
//
// Scheme-relative inputs like "//evil.example/x" would otherwise pass a naive
// "starts with /" check and rewrite the host under url.ResolveReference.
func ValidateCalendarPath(path string) error {
	if path == "" {
		return fmt.Errorf("calendar path cannot be empty")
	}
	if len(path) > 1024 {
		return fmt.Errorf("calendar path is too long (max 1024 UTF-8 bytes)")
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
		return fmt.Errorf("UID is too long (max %d UTF-8 bytes)", MaxUIDLen)
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

// ValidateIfMatchETag requires one strong RFC entity-tag in its exact quoted
// wire form. Empty is allowed because callers can use the tag read from GET.
func ValidateIfMatchETag(etag string) error {
	if etag == "" {
		return nil
	}
	if _, err := parseStrongETag(etag); err != nil {
		return NewValidationError("etag must be exactly one valid strong quoted entity-tag from get_event or search_events")
	}
	return nil
}

// ValidateTextField checks the UTF-8 byte length of a free-text field. These
// fields include title, location, notes, and query. It rejects NUL characters
// but permits newlines because notes can span lines. During TEXT encoding,
// go-ical escapes \n, semicolons, commas, and backslashes. Thus, these fields
// cannot inject an iCalendar property and need no manual escaping here.
func ValidateTextField(name, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("%s too long (max %d UTF-8 bytes, got %d)", name, max, len(value))
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
//     "...Z" for UTC): parsed literally. The caller deliberately selects this
//     offset. The parser always preserves it, including "Z". It never treats
//     this value as a local time from the user.
//   - A local wall-clock time with NO offset ("2026-07-01T14:00:00"):
//     interpreted in defaultLoc (nil defaults to UTC).
//
// The no-offset form prevents errors when an LLM converts a stated local hour.
// An incident on 2026-07-12 involved a "Deep clean" event from 10:00 to 14:00
// in Europe/Paris. The calling agent sent start=2026-07-12T10:00:00Z and
// end=2026-07-12T14:00:00Z, which specify literal UTC. iCloud displayed the
// event two hours late because CEST is UTC+2. A bare local time lets the server
// resolve the DST-aware offset through defaultLoc.
//
// See ICLOUD_MCP_DEFAULT_TZ in internal/config. The tool description recommends
// this form for the time that the user stated. It reserves an explicit offset
// for a different timezone, such as a call with someone abroad.
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
		"invalid %s: expected RFC3339 with an explicit offset (e.g. 2026-07-01T14:00:00+02:00, or ...Z for UTC) "+
			"or a local time with no offset (e.g. 2026-07-01T14:00:00), interpreted as %s",
		name, loc,
	)
}

// ParseRecurrenceID parses recurrence_id for scope=occurrence. Accepts:
//   - YYYY-MM-DD (all-day series: UTC midnight on that calendar date)
//   - the same forms as ParseDateTime for timed series
//
// Prefer YYYY-MM-DD for all-day masters. In a non-UTC DEFAULT_TZ, a bare local
// midnight can shift to the previous UTC day. That shift can miss the
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

// ValidateRRULE checks an RRULE without the "RRULE:" prefix. Write paths accept
// only parseable rules with bounded frequency. SECONDLY and MINUTELY require
// COUNT or UNTIL. HOURLY also requires one when both values are absent. These
// limits prevent the creation of an infinite high-frequency series.
func ValidateRRULE(rule string) error {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return fmt.Errorf("RRULE cannot be empty")
	}
	if len(rule) > 1024 {
		return fmt.Errorf("RRULE is too long (max 1024 UTF-8 bytes)")
	}
	if strings.HasPrefix(strings.ToUpper(rule), "RRULE:") {
		return fmt.Errorf("RRULE must not include the RRULE: prefix; pass only the value (e.g. FREQ=WEEKLY;COUNT=10)")
	}
	ropt, err := rrule.StrToROption(strings.ToUpper(rule))
	if err != nil {
		return fmt.Errorf("invalid RRULE: %w", err)
	}
	if !normalizeRecurrenceSelectorLists(ropt) {
		return fmt.Errorf("RRULE has excessive selector cardinality")
	}
	if len(ropt.Byeaster) != 0 {
		return fmt.Errorf("RRULE BYEASTER is not supported")
	}
	for i := range ropt.Byweekday {
		if ropt.Byweekday[i].N() != 0 && ropt.Freq != rrule.YEARLY && ropt.Freq != rrule.MONTHLY {
			return fmt.Errorf("RRULE ordinal BYDAY is supported only for MONTHLY or YEARLY frequency")
		}
	}
	if ropt.Freq >= rrule.HOURLY && hasRecurrenceDateSelectors(ropt) {
		return fmt.Errorf("RRULE calendar selectors are not supported with sub-daily frequency")
	}
	if _, err := rrule.NewRRule(*ropt); err != nil {
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

func validateRRULEForStart(rule string, start time.Time) error {
	return validateRRULEForStartContext(context.Background(), rule, start)
}

func validateRRULEForStartContext(ctx context.Context, rule string, start time.Time) error {
	if err := ValidateRRULE(rule); err != nil {
		return err
	}
	opt, err := rrule.StrToROption(strings.ToUpper(strings.TrimSpace(rule)))
	if err != nil {
		return fmt.Errorf("invalid RRULE: %w", err)
	}
	if !normalizeRecurrenceSelectorLists(opt) {
		return fmt.Errorf("RRULE has excessive selector cardinality")
	}
	opt.Dtstart = start
	seriesRemaining := maxRecurrenceExpansionWork
	aggregateRemaining := maxRecurrenceSearchWork
	safety, _, _ := checkRecurrenceSelectorSafety(ctx, opt, &seriesRemaining, &aggregateRemaining)
	switch safety {
	case recurrenceSelectorsCanceled:
		return fmt.Errorf("RRULE selector validation was canceled")
	case recurrenceSelectorsUnsupported:
		return fmt.Errorf("RRULE uses an unsupported unsafe selector combination")
	case recurrenceSelectorsUnreachable:
		return fmt.Errorf("RRULE has selectors that INTERVAL can never reach from DTSTART")
	case recurrenceSelectorsExcessive:
		return fmt.Errorf("RRULE requires excessive internal selector work")
	}
	return nil
}
