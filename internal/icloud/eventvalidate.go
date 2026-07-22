package icloud

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// MaxAlarms caps VALARM components on create/update.
const MaxAlarms = 5

// AllowedStatus values for VEVENT STATUS.
var AllowedStatus = map[string]bool{
	"": true, "TENTATIVE": true, "CONFIRMED": true, "CANCELLED": true,
}

// AllowedTransparency values for VEVENT TRANSP.
var AllowedTransparency = map[string]bool{
	"": true, "OPAQUE": true, "TRANSPARENT": true,
}

// StructuredRecurrence is a safer alternative to a raw RRULE string for create.
type StructuredRecurrence struct {
	Frequency  string   `json:"frequency"` // daily|weekly|monthly|yearly
	Interval   int      `json:"interval,omitempty"`
	ByDay      []string `json:"by_day,omitempty"` // MO,TU,...
	Count      int      `json:"count,omitempty"`
	Until      string   `json:"until,omitempty"`      // RFC3339 or date
	Exceptions []string `json:"exceptions,omitempty"` // EXDATE list as datetime strings
}

// AlarmSpec describes a VALARM DISPLAY trigger.
type AlarmSpec struct {
	// MinutesBefore > 0: trigger -PT{n}M. When Disable is true, no alarm.
	MinutesBefore int  `json:"minutes_before,omitempty"`
	Disable       bool `json:"disable,omitempty"`
}

// EventInput is the shared create/validate shape (network-free validation).
type EventInput struct {
	Title          string
	Location       string
	Notes          string
	StartTime      time.Time
	EndTime        time.Time
	AllDay         bool
	Timezone       string
	Status         string
	Transparency   string
	URL            string
	Recurrence     string // raw RRULE without prefix
	Structured     *StructuredRecurrence
	Alarms         []AlarmSpec
	AlarmMinutes   int // legacy single-alarm field
	ClientUID      string
	IdempotencyKey string
}

// ValidationResult is returned by ValidateEventInput.
type ValidationResult struct {
	OK         bool             `json:"ok"`
	Errors     []string         `json:"errors,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
	Normalized *NormalizedEvent `json:"normalized,omitempty"`
}

// NormalizedEvent is a redacted, normalized view safe to return to agents.
type NormalizedEvent struct {
	Title        string `json:"title"`
	Start        string `json:"start"`
	End          string `json:"end"`
	AllDay       bool   `json:"allDay,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Location     string `json:"location,omitempty"`
	Notes        string `json:"notes,omitempty"`
	Status       string `json:"status,omitempty"`
	Transparency string `json:"transparency,omitempty"`
	URL          string `json:"url,omitempty"`
	Recurrence   string `json:"recurrence,omitempty"`
	AlarmMinutes []int  `json:"alarmMinutes,omitempty"`
	UID          string `json:"uid,omitempty"`
}

// ValidateEventInput validates create/update-shaped input without network I/O.
func ValidateEventInput(in *EventInput, defaultLoc *time.Location) ValidationResult {
	var errs, warns []string
	if in == nil {
		return ValidationResult{OK: false, Errors: []string{"event cannot be nil"}}
	}
	if err := ValidateTextField("title", in.Title, MaxTitleLen); err != nil {
		errs = append(errs, err.Error())
	}
	if strings.TrimSpace(in.Title) == "" {
		errs = append(errs, "title cannot be empty")
	}
	if err := ValidateTextField("location", in.Location, MaxLocationLen); err != nil {
		errs = append(errs, err.Error())
	}
	if err := ValidateTextField("notes", in.Notes, MaxNotesLen); err != nil {
		errs = append(errs, err.Error())
	}
	if in.StartTime.IsZero() || in.EndTime.IsZero() {
		errs = append(errs, "start and end are required")
	} else if err := ValidateRange(in.StartTime, in.EndTime); err != nil {
		errs = append(errs, err.Error())
	}
	status := strings.ToUpper(strings.TrimSpace(in.Status))
	if !AllowedStatus[status] {
		errs = append(errs, fmt.Sprintf("status must be one of TENTATIVE, CONFIRMED, CANCELLED (got %q)", in.Status))
	}
	transp := strings.ToUpper(strings.TrimSpace(in.Transparency))
	if !AllowedTransparency[transp] {
		errs = append(errs, fmt.Sprintf("transparency must be OPAQUE or TRANSPARENT (got %q)", in.Transparency))
	}
	if in.URL != "" {
		if err := validateEventURL(in.URL); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			errs = append(errs, fmt.Sprintf("invalid timezone %q", in.Timezone))
		}
	}
	rrule := strings.TrimSpace(in.Recurrence)
	if in.Structured != nil {
		built, exdates, err := structuredToRRULE(in.Structured, defaultLoc)
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			if rrule != "" && rrule != built {
				errs = append(errs, "cannot supply both rrule and structured recurrence with different values")
			}
			rrule = built
			if len(exdates) > 0 {
				warns = append(warns, fmt.Sprintf("%d exception date(s) will be written as EXDATE", len(exdates)))
			}
		}
	}
	if rrule != "" {
		if err := ValidateRRULE(rrule); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if in.ClientUID != "" {
		if err := ValidateUID(in.ClientUID); err != nil {
			errs = append(errs, "client uid: "+err.Error())
		}
	}
	if in.IdempotencyKey != "" {
		if err := ValidateUID(in.IdempotencyKey); err != nil {
			errs = append(errs, "idempotency_key: "+err.Error())
		}
	}
	alarms := collectAlarms(in)
	if len(alarms) > MaxAlarms {
		errs = append(errs, fmt.Sprintf("at most %d alarms allowed", MaxAlarms))
	}
	for _, a := range alarms {
		if a < 0 || a > maxAlarmMinutes {
			errs = append(errs, fmt.Sprintf("alarm minutes_before must be between 0 and %d", maxAlarmMinutes))
		}
	}
	if w := AmbiguousDSTWarning(in.StartTime, defaultLoc); w != "" {
		warns = append(warns, w)
	}

	res := ValidationResult{OK: len(errs) == 0, Errors: errs, Warnings: warns}
	if res.OK {
		tz := in.Timezone
		if tz == "" && defaultLoc != nil {
			tz = defaultLoc.String()
		}
		uid := in.ClientUID
		if uid == "" {
			uid = in.IdempotencyKey
		}
		res.Normalized = &NormalizedEvent{
			Title:        in.Title,
			Start:        FormatEventTime(in.StartTime, in.AllDay),
			End:          FormatEventTime(in.EndTime, in.AllDay),
			AllDay:       in.AllDay,
			Timezone:     tz,
			Location:     in.Location,
			Notes:        in.Notes,
			Status:       status,
			Transparency: transp,
			URL:          in.URL,
			Recurrence:   rrule,
			AlarmMinutes: alarms,
			UID:          uid,
		}
	}
	return res
}

const maxAlarmMinutes = 40320 // 4 weeks

func collectAlarms(in *EventInput) []int {
	var out []int
	if in.AlarmMinutes > 0 {
		out = append(out, in.AlarmMinutes)
	}
	for _, a := range in.Alarms {
		if a.Disable {
			continue
		}
		if a.MinutesBefore > 0 {
			out = append(out, a.MinutesBefore)
		}
	}
	return out
}

func validateEventURL(raw string) error {
	if len(raw) > 2000 {
		return fmt.Errorf("url too long (max 2000 characters)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("url must include a host")
	}
	return nil
}

func structuredToRRULE(s *StructuredRecurrence, defaultLoc *time.Location) (string, []time.Time, error) {
	if s == nil {
		return "", nil, nil
	}
	freq := strings.ToUpper(strings.TrimSpace(s.Frequency))
	switch freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	default:
		return "", nil, fmt.Errorf("recurrence frequency must be daily, weekly, monthly, or yearly")
	}
	interval := s.Interval
	if interval < 0 {
		return "", nil, fmt.Errorf("recurrence interval cannot be negative")
	}
	if interval == 0 {
		interval = 1
	}
	if interval > 366 {
		return "", nil, fmt.Errorf("recurrence interval too large (max 366)")
	}
	if s.Count > 0 && s.Until != "" {
		return "", nil, fmt.Errorf("recurrence cannot set both count and until")
	}
	if s.Count < 0 || s.Count > 2000 {
		if s.Count != 0 {
			return "", nil, fmt.Errorf("recurrence count must be between 1 and 2000")
		}
	}
	parts := []string{fmt.Sprintf("FREQ=%s", freq)}
	if interval > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", interval))
	}
	if len(s.ByDay) > 0 {
		for _, d := range s.ByDay {
			up := strings.ToUpper(strings.TrimSpace(d))
			switch up {
			case "MO", "TU", "WE", "TH", "FR", "SA", "SU":
			default:
				return "", nil, fmt.Errorf("invalid by_day %q", d)
			}
		}
		parts = append(parts, "BYDAY="+strings.ToUpper(strings.Join(s.ByDay, ",")))
	}
	if s.Count > 0 {
		parts = append(parts, fmt.Sprintf("COUNT=%d", s.Count))
	}
	if s.Until != "" {
		t, err := ParseDateTime("until", s.Until, defaultLoc)
		if err != nil {
			// try date only
			if td, e2 := time.ParseInLocation("2006-01-02", s.Until, time.UTC); e2 == nil {
				t = td
			} else {
				return "", nil, fmt.Errorf("invalid recurrence until: %w", err)
			}
		}
		parts = append(parts, "UNTIL="+t.UTC().Format("20060102T150405Z"))
	}
	var ex []time.Time
	for _, e := range s.Exceptions {
		t, err := ParseDateTime("exception", e, defaultLoc)
		if err != nil {
			return "", nil, fmt.Errorf("invalid recurrence exception: %w", err)
		}
		ex = append(ex, t)
	}
	rule := strings.Join(parts, ";")
	if err := ValidateRRULE(rule); err != nil {
		return "", nil, err
	}
	return rule, ex, nil
}
