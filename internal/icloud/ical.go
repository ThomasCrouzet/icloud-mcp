package icloud

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

// newUID generates an event UID using crypto/rand (16 hex-encoded bytes);
// no google/uuid dependency (forbidden by the spec).
func newUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("UID generation: %w", err)
	}
	return fmt.Sprintf("%s@icloud-mcp", hex.EncodeToString(buf)), nil
}

// buildEventCalendar builds the complete VCALENDAR for a new event.
// All-day events use VALUE=DATE; timed events are written as UTC (Z), which
// pins the absolute instant regardless of the reader's timezone database.
// Writing DTSTART/DTEND with TZID plus a VTIMEZONE would preserve local
// wall-clock intent, but only with a full DST transition table: a fixed
// offset is wrong for any occurrence on the other side of a transition.
// That remains unimplemented pending verification against real iCloud.
func buildEventCalendar(uid string, ne *NewEvent) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//icloud-mcp//EN")

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetText(ical.PropSummary, ne.Title)
	if ne.Location != "" {
		ev.Props.SetText(ical.PropLocation, ne.Location)
	}
	if ne.Notes != "" {
		ev.Props.SetText(ical.PropDescription, ne.Notes)
	}

	if ne.AllDay {
		// DATE values: use calendar date in UTC (date components only).
		startDay := time.Date(ne.StartTime.Year(), ne.StartTime.Month(), ne.StartTime.Day(), 0, 0, 0, 0, time.UTC)
		endDay := time.Date(ne.EndTime.Year(), ne.EndTime.Month(), ne.EndTime.Day(), 0, 0, 0, 0, time.UTC)
		if !endDay.After(startDay) {
			endDay = startDay.Add(24 * time.Hour)
		}
		ev.Props.SetDate(ical.PropDateTimeStart, startDay)
		ev.Props.SetDate(ical.PropDateTimeEnd, endDay)
	} else {
		ev.Props.SetDateTime(ical.PropDateTimeStart, ne.StartTime.UTC())
		ev.Props.SetDateTime(ical.PropDateTimeEnd, ne.EndTime.UTC())
	}
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())

	if ne.Recurrence != "" {
		prop := ical.NewProp(ical.PropRecurrenceRule)
		prop.Value = ne.Recurrence
		ev.Props.Set(prop)
	}

	if ne.AlarmMinutesBefore > 0 {
		alarm := ical.NewComponent(ical.CompAlarm)
		alarm.Props.SetText(ical.PropAction, "DISPLAY")
		alarm.Props.SetText(ical.PropDescription, "Reminder")
		trigger := ical.NewProp(ical.PropTrigger)
		trigger.Value = fmt.Sprintf("-PT%dM", ne.AlarmMinutesBefore) // raw DURATION value, not SetText
		alarm.Props.Set(trigger)
		ev.Children = append(ev.Children, alarm)
	}

	cal.Children = append(cal.Children, ev.Component)
	return cal
}

// findMasterVEvent returns the master VEVENT (the one without RECURRENCE-ID)
// of a calendar object. Any override VEVENTs (RECURRENCE-ID exceptions) are
// ignored; update_event only modifies the master.
func findMasterVEvent(cal *ical.Calendar) (*ical.Event, error) {
	if cal == nil {
		return nil, fmt.Errorf("calendar object has no data")
	}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		vevent := ical.NewEvent()
		vevent.Component = child
		if p := vevent.Props.Get(ical.PropRecurrenceID); p != nil {
			continue // override, not the master
		}
		return vevent, nil
	}
	return nil, fmt.Errorf("no master VEVENT found in object")
}

// setSequence sets SEQUENCE as an INTEGER property (go-ical's default type
// for this property). Do NOT use SetText, which would add a superfluous and
// semantically incorrect VALUE=TEXT parameter.
func setSequence(vevent *ical.Event, n int) {
	prop := ical.NewProp(ical.PropSequence)
	prop.Value = strconv.Itoa(n)
	vevent.Props.Set(prop)
}

// setEventDateProp sets a start/end date while preserving the all-day format
// (pure date, 8 characters) when the existing property was already a pure
// date; never convert an all-day event to a datetime during an update.
func setEventDateProp(vevent *ical.Event, name string, t time.Time) {
	existing := vevent.Props.Get(name)
	if existing != nil && len(existing.Value) == 8 {
		vevent.Props.SetDate(name, t.UTC())
		return
	}
	vevent.Props.SetDateTime(name, t.UTC())
}

// parseCalendarObject extracts the Events from a CalDAV object. One object
// may contain SEVERAL VEVENTs (master + RECURRENCE-ID exceptions): iterate
// over every VEVENT child and split master from overrides.
func parseCalendarObject(obj *extcaldav.CalendarObject) (*Event, []Event, error) {
	if obj.Data == nil {
		return nil, nil, fmt.Errorf("calendar object has no data (path=%s)", obj.Path)
	}

	var master *Event
	var overrides []Event
	var exDates []time.Time

	for _, child := range obj.Data.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		vevent := ical.NewEvent()
		vevent.Component = child

		ev, isOverride, evExDates, err := parseVEvent(vevent, obj.Path)
		if err != nil {
			return nil, nil, err
		}
		if isOverride {
			overrides = append(overrides, *ev)
			continue
		}
		if master != nil {
			continue // two masters in the same object: anomaly, keep the first
		}
		master = ev
		exDates = evExDates
	}

	if master == nil {
		return nil, nil, fmt.Errorf("no master VEVENT found in object (path=%s)", obj.Path)
	}
	master.exDates = exDates
	return master, overrides, nil
}

func parseVEvent(vevent *ical.Event, path string) (ev *Event, isOverride bool, exDates []time.Time, err error) {
	e := &Event{Path: path}

	if p := vevent.Props.Get(ical.PropUID); p != nil {
		e.UID = p.Value
	}
	if p := vevent.Props.Get(ical.PropSummary); p != nil {
		e.Title = p.Value
	}
	if p := vevent.Props.Get(ical.PropLocation); p != nil {
		e.Location = p.Value
	}
	if p := vevent.Props.Get(ical.PropDescription); p != nil {
		e.Notes = p.Value
	}

	if p := vevent.Props.Get(ical.PropDateTimeStart); p != nil {
		t, derr := p.DateTime(time.UTC)
		if derr != nil {
			return nil, false, nil, fmt.Errorf("invalid DTSTART (uid=%s): %w", e.UID, derr)
		}
		e.StartTime = t
		e.AllDay = len(p.Value) == 8
		if tzid := p.Params.Get(ical.PropTimezoneID); tzid != "" {
			e.Timezone = tzid
		}
	}
	if p := vevent.Props.Get(ical.PropDateTimeEnd); p != nil {
		t, derr := p.DateTime(time.UTC)
		if derr != nil {
			return nil, false, nil, fmt.Errorf("invalid DTEND (uid=%s): %w", e.UID, derr)
		}
		e.EndTime = t
	} else if durProp := vevent.Props.Get(ical.PropDuration); durProp != nil {
		// DTEND absent but DURATION present (RFC 5545 §3.6.1: DTEND and
		// DURATION are mutually exclusive; DURATION is the valid
		// alternative). Without this derivation EndTime would stay zero: the
		// event would vanish from search (eventOverlaps always false) or
		// produce a negative duration during recurrence expansion.
		dur, derr := durProp.Duration()
		if derr != nil {
			return nil, false, nil, fmt.Errorf("invalid DURATION (uid=%s): %w", e.UID, derr)
		}
		e.EndTime = e.StartTime.Add(dur)
	} else if e.AllDay {
		// Neither DTEND nor DURATION on an all-day event (bare
		// DTSTART;VALUE=DATE): one full day by convention.
		e.EndTime = e.StartTime.Add(24 * time.Hour)
	} else {
		// Neither DTEND nor DURATION on a timed event: zero duration rather
		// than a zero time (which would break eventOverlaps).
		e.EndTime = e.StartTime
	}

	if p := vevent.Props.Get(ical.PropRecurrenceRule); p != nil {
		e.Recurrence = p.Value
	}

	if p := vevent.Props.Get(ical.PropRecurrenceID); p != nil {
		t, derr := p.DateTime(time.UTC)
		if derr == nil {
			e.recurrenceID = t
			isOverride = true
		}
	}

	for _, p := range vevent.Props[ical.PropExceptionDates] {
		prop := p
		dates, derr := parseExDateProp(&prop)
		if derr != nil {
			return nil, false, nil, fmt.Errorf("invalid EXDATE (uid=%s): %w", e.UID, derr)
		}
		exDates = append(exDates, dates...)
	}

	return e, isOverride, exDates, nil
}

// parseExDateProp parses an EXDATE property, which may carry several
// comma-separated dates (RFC 5545 §3.8.5.1). go-ical Prop.DateTime only
// handles a single value, so split manually.
func parseExDateProp(p *ical.Prop) ([]time.Time, error) {
	var out []time.Time
	for _, part := range strings.Split(p.Value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		t, err := parseICalDateTimeValue(part, p.Params.Get(ical.PropTimezoneID))
		if err != nil {
			return nil, err
		}
		out = append(out, t.UTC())
	}
	return out, nil
}

// parseICalDateTimeValue parses a raw iCalendar date/date-time value
// (formats "20060102", "20060102T150405Z", or "20060102T150405" plus TZID).
func parseICalDateTimeValue(value, tzid string) (time.Time, error) {
	switch {
	case len(value) == 8:
		return time.ParseInLocation("20060102", value, time.UTC)
	case strings.HasSuffix(value, "Z"):
		return time.Parse("20060102T150405Z", value)
	default:
		loc := time.UTC
		if tzid != "" {
			if l, err := time.LoadLocation(tzid); err == nil {
				loc = l
			}
		}
		return time.ParseInLocation("20060102T150405", value, loc)
	}
}
