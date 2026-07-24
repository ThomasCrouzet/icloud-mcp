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
// All-day events use VALUE=DATE. Timed non-recurring events are written as
// UTC (Z). Timed recurring events (or any timed event with an explicit
// Timezone) use TZID + a generated VTIMEZONE so wall-clock RRULEs stay
// correct across DST transitions.
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

	loc := resolveWriteLocation(ne)
	if ne.AllDay {
		// DATE values: use calendar date in UTC (date components only).
		startDay := time.Date(ne.StartTime.Year(), ne.StartTime.Month(), ne.StartTime.Day(), 0, 0, 0, 0, time.UTC)
		endDay := time.Date(ne.EndTime.Year(), ne.EndTime.Month(), ne.EndTime.Day(), 0, 0, 0, 0, time.UTC)
		if !endDay.After(startDay) {
			endDay = startDay.Add(24 * time.Hour)
		}
		ev.Props.SetDate(ical.PropDateTimeStart, startDay)
		ev.Props.SetDate(ical.PropDateTimeEnd, endDay)
	} else if loc != nil && loc != time.UTC {
		ev.Props.SetDateTime(ical.PropDateTimeStart, ne.StartTime.In(loc))
		ev.Props.SetDateTime(ical.PropDateTimeEnd, ne.EndTime.In(loc))
		if vtz := buildVTimezone(loc, ne.StartTime, ne.EndTime); vtz != nil {
			cal.Children = append(cal.Children, vtz)
		}
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
	for _, ex := range ne.ExDates {
		if loc != nil && loc != time.UTC && !ne.AllDay {
			p := ical.NewProp(ical.PropExceptionDates)
			p.SetDateTime(ex.In(loc))
			ev.Props.Add(p)
			continue
		}
		p := ical.NewProp(ical.PropExceptionDates)
		p.Value = ex.UTC().Format("20060102T150405Z")
		ev.Props.Add(p)
	}
	if s := strings.ToUpper(strings.TrimSpace(ne.Status)); s != "" {
		ev.Props.SetText(ical.PropStatus, s)
	}
	if s := strings.ToUpper(strings.TrimSpace(ne.Transparency)); s != "" {
		ev.Props.SetText(ical.PropTransparency, s)
	}
	if ne.URL != "" {
		ev.Props.SetText(ical.PropURL, ne.URL)
	}

	alarmMinutes := collectCreateAlarms(ne)
	for _, mins := range alarmMinutes {
		alarm := ical.NewComponent(ical.CompAlarm)
		alarm.Props.SetText(ical.PropAction, "DISPLAY")
		alarm.Props.SetText(ical.PropDescription, "Reminder")
		trigger := ical.NewProp(ical.PropTrigger)
		trigger.Value = fmt.Sprintf("-PT%dM", mins) // raw DURATION value, not SetText
		alarm.Props.Set(trigger)
		ev.Children = append(ev.Children, alarm)
	}

	cal.Children = append(cal.Children, ev.Component)
	return cal
}

// resolveWriteLocation picks the IANA location used for timed create writes.
// Explicit NewEvent.Timezone wins; otherwise a non-UTC StartTime location is
// used when the event is recurring (wall-clock RRULE). Single timed events
// without Timezone stay UTC Z.
func resolveWriteLocation(ne *NewEvent) *time.Location {
	if ne == nil || ne.AllDay {
		return time.UTC
	}
	if tz := strings.TrimSpace(ne.Timezone); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	if ne.Recurrence != "" {
		if loc := ne.StartTime.Location(); loc != nil && loc != time.UTC {
			return loc
		}
	}
	return time.UTC
}

// buildVTimezone builds a minimal VTIMEZONE for loc by sampling offset
// transitions around [from,to] (widened to cover typical DST). Returns nil
// for UTC or when no usable STANDARD/DAYLIGHT component can be built.
func buildVTimezone(loc *time.Location, from, to time.Time) *ical.Component {
	if loc == nil || loc == time.UTC {
		return nil
	}
	// Widen the sample window so yearly DST transitions are observed.
	start := from.UTC().AddDate(-1, 0, 0)
	end := to.UTC().AddDate(2, 0, 0)
	if !end.After(start) {
		end = start.AddDate(2, 0, 0)
	}

	type trans struct {
		at             time.Time
		fromOff, toOff int
		name           string
	}
	var transitions []trans
	prev := start.In(loc)
	_, prevOff := prev.Zone()
	// Hourly sample is enough to catch civil DST jumps.
	for t := start.Add(time.Hour); !t.After(end); t = t.Add(time.Hour) {
		local := t.In(loc)
		name, off := local.Zone()
		if off != prevOff {
			transitions = append(transitions, trans{
				at: local, fromOff: prevOff, toOff: off, name: name,
			})
			prevOff = off
		}
	}

	vtz := ical.NewComponent(ical.CompTimezone)
	vtz.Props.SetText(ical.PropTimezoneID, loc.String())

	if len(transitions) == 0 {
		// Fixed-offset zone: single STANDARD component.
		sample := from.In(loc)
		name, off := sample.Zone()
		std := ical.NewComponent(ical.CompTimezoneStandard)
		std.Props.SetDateTime(ical.PropDateTimeStart, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
		std.Props.SetText(ical.PropTimezoneOffsetFrom, formatUTCOffset(off))
		std.Props.SetText(ical.PropTimezoneOffsetTo, formatUTCOffset(off))
		if name != "" {
			std.Props.SetText(ical.PropTimezoneName, name)
		}
		vtz.Children = append(vtz.Children, std)
		return vtz
	}

	// Keep the most recent STANDARD and DAYLIGHT-like transitions (offset
	// decrease vs increase). Enough for go-ical encode and iCloud RRULE.
	var std, day *ical.Component
	for i := len(transitions) - 1; i >= 0; i-- {
		tr := transitions[i]
		comp := ical.NewComponent(ical.CompTimezoneStandard)
		if tr.toOff > tr.fromOff {
			comp = ical.NewComponent(ical.CompTimezoneDaylight)
		}
		// DTSTART is floating local wall time of the transition.
		wall := time.Date(tr.at.Year(), tr.at.Month(), tr.at.Day(), tr.at.Hour(), tr.at.Minute(), tr.at.Second(), 0, time.UTC)
		comp.Props.SetDateTime(ical.PropDateTimeStart, wall)
		comp.Props.SetText(ical.PropTimezoneOffsetFrom, formatUTCOffset(tr.fromOff))
		comp.Props.SetText(ical.PropTimezoneOffsetTo, formatUTCOffset(tr.toOff))
		if tr.name != "" {
			comp.Props.SetText(ical.PropTimezoneName, tr.name)
		}
		// YEARLY RRULE on the observed month/day-of-week approximates the rule.
		rr := ical.NewProp(ical.PropRecurrenceRule)
		rr.Value = fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%s", int(tr.at.Month()), byDayToken(tr.at))
		comp.Props.Set(rr)
		if comp.Name == ical.CompTimezoneDaylight && day == nil {
			day = comp
		}
		if comp.Name == ical.CompTimezoneStandard && std == nil {
			std = comp
		}
		if std != nil && day != nil {
			break
		}
	}
	if day != nil {
		vtz.Children = append(vtz.Children, day)
	}
	if std != nil {
		vtz.Children = append(vtz.Children, std)
	}
	if len(vtz.Children) == 0 {
		return nil
	}
	return vtz
}

func formatUTCOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	return fmt.Sprintf("%s%02d%02d", sign, h, m)
}

func byDayToken(t time.Time) string {
	// Nth weekday of month: 1SU .. 4SU or -1SU for last.
	day := t.Day()
	nth := (day-1)/7 + 1
	names := []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}
	wd := names[int(t.Weekday())]
	// If another same weekday exists next week in this month, keep nth;
	// otherwise emit -1 for last-of-month rules (common DST pattern).
	next := t.AddDate(0, 0, 7)
	if next.Month() != t.Month() {
		return "-1" + wd
	}
	return fmt.Sprintf("%d%s", nth, wd)
}

func collectCreateAlarms(ne *NewEvent) []int {
	var out []int
	if ne.AlarmMinutesBefore > 0 {
		out = append(out, ne.AlarmMinutesBefore)
	}
	for _, a := range ne.Alarms {
		if a.Disable || a.MinutesBefore <= 0 {
			continue
		}
		out = append(out, a.MinutesBefore)
		if len(out) >= MaxAlarms {
			break
		}
	}
	return out
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

// setEventDateProp sets a start/end property while preserving the existing
// value form: VALUE=DATE stays DATE, TZID stays TZID with local wall clock,
// UTC (Z) stays Z. Never strip TZID to Z on a timed update (that shifts
// wall-clock intent across DST and leaves orphan VTIMEZONE components).
func setEventDateProp(vevent *ical.Event, name string, t time.Time) {
	existing := vevent.Props.Get(name)
	if existing == nil {
		vevent.Props.SetDateTime(name, t.UTC())
		return
	}
	if isDateOnlyProp(existing) {
		// All-day: keep calendar date components only.
		lt := t.UTC()
		day := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, time.UTC)
		vevent.Props.SetDate(name, day)
		return
	}
	if tzid := existing.Params.Get(ical.PropTimezoneID); tzid != "" {
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			// Unknown TZID: fall back to UTC rather than inventing a zone.
			vevent.Props.SetDateTime(name, t.UTC())
			return
		}
		// go-ical SetDateTime writes TZID from t.Location() for non-UTC.
		vevent.Props.SetDateTime(name, t.In(loc))
		return
	}
	// UTC Z or floating local without TZID: pin as UTC Z.
	vevent.Props.SetDateTime(name, t.UTC())
}

// isDateOnlyProp reports whether p is a VALUE=DATE (all-day) property.
func isDateOnlyProp(p *ical.Prop) bool {
	if p == nil {
		return false
	}
	if p.ValueType() == ical.ValueDate {
		return true
	}
	return len(p.Value) == 8 && !strings.Contains(p.Value, "T")
}

// setRecurrenceInstantProp builds a RECURRENCE-ID or EXDATE property whose
// value form matches the master DTSTART (DATE / TZID / Z). Mismatched forms
// fail to match on many CalDAV servers (all-day and TZID series).
func setRecurrenceInstantProp(name string, master *ical.Event, instant time.Time) *ical.Prop {
	p := ical.NewProp(name)
	dtstart := master.Props.Get(ical.PropDateTimeStart)
	if dtstart != nil && isDateOnlyProp(dtstart) {
		lt := instant.UTC()
		p.SetDate(time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, time.UTC))
		return p
	}
	if dtstart != nil {
		if tzid := dtstart.Params.Get(ical.PropTimezoneID); tzid != "" {
			if loc, err := time.LoadLocation(tzid); err == nil {
				p.SetDateTime(instant.In(loc))
				return p
			}
		}
	}
	p.SetDateTime(instant.UTC())
	return p
}

// masterEventDuration returns the master's duration from DTEND, else DURATION,
// else 24h for all-day, else 1h for timed (occurrence override default).
func masterEventDuration(master *ical.Event) time.Duration {
	if master == nil {
		return time.Hour
	}
	stProp := master.Props.Get(ical.PropDateTimeStart)
	if stProp == nil {
		return time.Hour
	}
	st, err := stProp.DateTime(time.UTC)
	if err != nil {
		return time.Hour
	}
	if ep := master.Props.Get(ical.PropDateTimeEnd); ep != nil {
		if en, e2 := ep.DateTime(time.UTC); e2 == nil {
			if d := en.Sub(st); d > 0 {
				return d
			}
		}
	}
	if dp := master.Props.Get(ical.PropDuration); dp != nil {
		if d, derr := dp.Duration(); derr == nil && d > 0 {
			return d
		}
	}
	if isDateOnlyProp(stProp) {
		return 24 * time.Hour
	}
	return time.Hour
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
	if p := vevent.Props.Get(ical.PropStatus); p != nil {
		e.Status = p.Value
	}
	if p := vevent.Props.Get(ical.PropTransparency); p != nil {
		e.Transp = p.Value
	}
	if p := vevent.Props.Get(ical.PropURL); p != nil {
		e.URL = p.Value
	}

	if p := vevent.Props.Get(ical.PropRecurrenceID); p != nil {
		t, derr := p.DateTime(time.UTC)
		if derr == nil {
			e.RecurrenceID = t
			e.IsOverride = true
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

// parseAlarms extracts VALARM children from the master VEVENT of cal.
func parseAlarms(cal *ical.Calendar) []AlarmInfo {
	if cal == nil {
		return nil
	}
	master, err := findMasterVEvent(cal)
	if err != nil {
		return nil
	}
	var out []AlarmInfo
	for _, ch := range master.Children {
		if ch.Name != ical.CompAlarm {
			continue
		}
		info := AlarmInfo{}
		if p := ch.Props.Get(ical.PropAction); p != nil {
			info.Action = p.Value
		}
		if p := ch.Props.Get(ical.PropTrigger); p != nil {
			info.Trigger = p.Value
			info.MinutesBefore = parseTriggerMinutesBefore(p.Value)
		}
		out = append(out, info)
	}
	return out
}

// parseTriggerMinutesBefore parses common -PTnM / -PTnH triggers.
func parseTriggerMinutesBefore(v string) int {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "-PT") {
		return 0
	}
	rest := strings.TrimPrefix(v, "-PT")
	if strings.HasSuffix(rest, "M") {
		n, err := strconv.Atoi(strings.TrimSuffix(rest, "M"))
		if err == nil {
			return n
		}
	}
	if strings.HasSuffix(rest, "H") {
		n, err := strconv.Atoi(strings.TrimSuffix(rest, "H"))
		if err == nil {
			return n * 60
		}
	}
	return 0
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
