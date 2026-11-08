package icloud

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

const (
	maxRemoteComponents             = 1024
	maxRemoteComponentDepth         = 32
	maxRemoteOverrides              = 512
	maxRemoteProperties             = 10000
	maxRemotePropertiesPerComponent = 1024
	maxRemoteLogicalLines           = 16384
	maxRemotePhysicalLines          = 131072
	maxRemoteLogicalLineBytes       = 1 << 20
	maxRemotePropertyValueBytes     = 1 << 20
	maxRemotePropertyNameBytes      = 128
	maxRemoteParamsPerProperty      = 64
	maxRemoteParamBytes             = 1024
	maxRemoteAlarms                 = 64
	maxRemoteAlarmFieldBytes        = 512
	maxRemoteExDates                = 2000
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
		if ne.AllDay {
			prop.Value = normalizeAllDayUntil(prop.Value)
		}
		ev.Props.Set(prop)
	}
	for _, ex := range ne.ExDates {
		if ne.AllDay {
			p := ical.NewProp(ical.PropExceptionDates)
			p.SetDate(time.Date(ex.Year(), ex.Month(), ex.Day(), 0, 0, 0, 0, time.UTC))
			ev.Props.Add(p)
			continue
		}
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

func normalizeAllDayUntil(rule string) string {
	parts := strings.Split(rule, ";")
	for i, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(key, "UNTIL") || len(value) < 8 {
			continue
		}
		if _, err := time.Parse("20060102", value[:8]); err == nil {
			parts[i] = key + "=" + value[:8]
		}
	}
	return strings.Join(parts, ";")
}

// buildVTimezone builds a minimal VTIMEZONE for loc by sampling offset
// transitions around [from,to] (widened to cover typical DST). Returns nil
// for UTC or when no usable STANDARD/DAYLIGHT component can be built.
func buildVTimezone(loc *time.Location, from, to time.Time) *ical.Component {
	if loc == nil || loc == time.UTC {
		return nil
	}
	// Widen the sample window so yearly DST transitions are observed.
	start := from.UTC().AddDate(-1, 0, 0).Truncate(time.Second)
	end := to.UTC().AddDate(2, 0, 0).Truncate(time.Second)
	if !end.After(start) {
		end = start.AddDate(2, 0, 0)
	}

	type trans struct {
		at             time.Time
		fromOff, toOff int
		name           string
	}
	var transitions []trans
	previousInstant := start
	_, prevOff := previousInstant.In(loc).Zone()
	// Hourly sampling brackets each civil transition. Binary search then finds
	// the first exact second carrying the new offset.
	for sample := start.Add(time.Hour); !sample.After(end); sample = sample.Add(time.Hour) {
		_, off := sample.In(loc).Zone()
		if off != prevOff {
			at := findTimezoneTransition(loc, previousInstant, sample, prevOff)
			name, exactOff := at.In(loc).Zone()
			transitions = append(transitions, trans{
				at: at, fromOff: prevOff, toOff: exactOff, name: name,
			})
			prevOff = exactOff
		}
		previousInstant = sample
	}

	vtz := ical.NewComponent(ical.CompTimezone)
	vtz.Props.SetText(ical.PropTimezoneID, loc.String())

	if len(transitions) == 0 {
		// Fixed-offset zone: single STANDARD component.
		sample := from.In(loc)
		name, off := sample.Zone()
		std := ical.NewComponent(ical.CompTimezoneStandard)
		setFloatingDateTime(std, start.In(loc))
		std.Props.SetText(ical.PropTimezoneOffsetFrom, formatUTCOffset(off))
		std.Props.SetText(ical.PropTimezoneOffsetTo, formatUTCOffset(off))
		if name != "" {
			std.Props.SetText(ical.PropTimezoneName, name)
		}
		vtz.Children = append(vtz.Children, std)
		return vtz
	}

	// Keep the most recent STANDARD and DAYLIGHT-like transitions effective at
	// event start. A future DTSTART would leave the event without an applicable
	// observance even when its RRULE describes the right annual transition.
	var std, day *ical.Component
	cutoff := from.UTC()
	for _, tr := range transitions {
		if tr.at.After(cutoff) {
			continue
		}
		comp := ical.NewComponent(ical.CompTimezoneStandard)
		if tr.toOff > tr.fromOff {
			comp = ical.NewComponent(ical.CompTimezoneDaylight)
		}
		// VTIMEZONE DTSTART is the floating wall time immediately before the
		// transition, so apply the old offset to the precise UTC instant.
		wall := tr.at.Add(time.Duration(tr.fromOff) * time.Second)
		setFloatingDateTime(comp, wall)
		comp.Props.SetText(ical.PropTimezoneOffsetFrom, formatUTCOffset(tr.fromOff))
		comp.Props.SetText(ical.PropTimezoneOffsetTo, formatUTCOffset(tr.toOff))
		if tr.name != "" {
			comp.Props.SetText(ical.PropTimezoneName, tr.name)
		}
		// YEARLY RRULE on the observed month/day-of-week approximates the rule.
		rr := ical.NewProp(ical.PropRecurrenceRule)
		rr.Value = fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%s", int(wall.Month()), byDayToken(wall))
		comp.Props.Set(rr)
		if comp.Name == ical.CompTimezoneDaylight {
			day = comp
		}
		if comp.Name == ical.CompTimezoneStandard {
			std = comp
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

// recurringTimezoneStable reports whether offset increases and decreases keep
// the same annual local rule over the next five years. A minimal two-observance
// VTIMEZONE is unsafe for zones with policy-driven moving transitions.
func recurringTimezoneStable(loc *time.Location, from time.Time) bool {
	if loc == nil || loc == time.UTC {
		return true
	}
	start := from.UTC().AddDate(-1, 0, 0).Truncate(time.Second)
	end := from.UTC().AddDate(5, 0, 0).Truncate(time.Second)
	previous := start
	_, previousOffset := previous.In(loc).Zone()
	signatures := make(map[bool]string, 2)
	for sample := start.Add(time.Hour); !sample.After(end); sample = sample.Add(time.Hour) {
		_, offset := sample.In(loc).Zone()
		if offset == previousOffset {
			previous = sample
			continue
		}
		transition := findTimezoneTransition(loc, previous, sample, previousOffset)
		wall := transition.Add(time.Duration(previousOffset) * time.Second)
		increase := offset > previousOffset
		signature := fmt.Sprintf("%d/%s/%d/%d", wall.Month(), byDayToken(wall), previousOffset, offset)
		if existing, ok := signatures[increase]; ok && existing != signature {
			return false
		}
		signatures[increase] = signature
		previousOffset = offset
		previous = sample
	}
	return true
}

func findTimezoneTransition(loc *time.Location, before, after time.Time, oldOffset int) time.Time {
	low := before.Unix()
	high := after.Unix()
	for high-low > 1 {
		mid := low + (high-low)/2
		_, offset := time.Unix(mid, 0).In(loc).Zone()
		if offset == oldOffset {
			low = mid
		} else {
			high = mid
		}
	}
	return time.Unix(high, 0).UTC()
}

func setFloatingDateTime(component *ical.Component, value time.Time) {
	prop := ical.NewProp(ical.PropDateTimeStart)
	prop.Value = value.Format("20060102T150405")
	component.Props.Set(prop)
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
	if ne.AlarmMinutesBefore > 0 && ne.AlarmMinutesBefore <= maxAlarmMinutes {
		out = append(out, ne.AlarmMinutesBefore)
	}
	for _, a := range ne.Alarms {
		if a.Disable || a.MinutesBefore <= 0 || a.MinutesBefore > maxAlarmMinutes {
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

func incrementSequence(vevent *ical.Event) error {
	if vevent == nil {
		return NewError(CodeProtocolError, 0, "Calendar event has no sequence target", nil)
	}
	var sequence int64
	if prop := vevent.Props.Get(ical.PropSequence); prop != nil {
		parsed, err := strconv.ParseInt(strings.TrimSpace(prop.Value), 10, 32)
		if err != nil || parsed < 0 {
			return NewError(CodeProtocolError, 0, "Calendar event sequence is invalid", nil)
		}
		sequence = parsed
	}
	const maxSequence = int64(1<<31 - 1)
	if sequence >= maxSequence {
		return NewError(CodeProtocolError, 0, "Calendar event sequence cannot be incremented safely", nil)
	}
	setSequence(vevent, int(sequence+1))
	return nil
}

// setEventDateProp sets a start or end property and preserves its value form.
// VALUE=DATE stays DATE. TZID keeps its local wall clock, and UTC stays Z. A
// timed update must not replace TZID with Z. That change shifts wall-clock
// intent across DST and leaves orphan VTIMEZONE components.
// Unknown TZIDs fail closed so wall times are never silently reinterpreted.
func setEventDateProp(vevent *ical.Event, name string, t time.Time) error {
	return setEventDatePropWithForm(vevent, name, t, nil)
}

// setEventDatePropWithForm is setEventDateProp with an optional fallback form.
// The fallback is used when the named property is absent, for example an
// all-day occurrence that inherits the DATE form of the master DTSTART.
func setEventDatePropWithForm(vevent *ical.Event, name string, t time.Time, form *ical.Prop) error {
	existing := vevent.Props.Get(name)
	if existing == nil && form == nil && name == ical.PropDateTimeEnd {
		form = vevent.Props.Get(ical.PropDateTimeStart)
	}
	if existing == nil {
		if form == nil {
			vevent.Props.SetDateTime(name, t.UTC())
			return nil
		}
		prop := *form
		prop.Name = name
		if err := setDatePropLike(&prop, form, t); err != nil {
			return err
		}
		vevent.Props.Set(&prop)
		return nil
	}
	return setDatePropLike(existing, existing, t)
}

func setDatePropLike(prop, reference *ical.Prop, t time.Time) error {
	if isDateOnlyProp(reference) {
		// All-day: keep calendar date components only.
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		prop.SetDate(day)
		return nil
	}
	if tzid := reference.Params.Get(ical.PropTimezoneID); tzid != "" {
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			return NewError(CodeProtocolError, 0, "Calendar event uses an unknown TZID", nil)
		}
		wall := WallClockOf(t.In(loc))
		if !WallClockExists(wall, loc) {
			return NewValidationError("event time does not exist in timezone " + tzid + " because of a daylight-saving gap")
		}
		// go-ical SetDateTime writes TZID from t.Location() for non-UTC.
		prop.SetDateTime(t.In(loc))
		return nil
	}
	if strings.HasSuffix(reference.Value, "Z") {
		prop.Params.Del(ical.PropTimezoneID)
		prop.SetDateTime(t.UTC())
		return nil
	}
	// A DATE-TIME without TZID or Z is floating. Keep it floating and retain
	// any non-timezone parameters rather than converting wall time to UTC.
	prop.Value = t.Format("20060102T150405")
	prop.Params.Del(ical.PropTimezoneID)
	return nil
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
// fail to match on many CalDAV servers (all-day and TZID series). Unknown
// TZIDs fail closed.
func setRecurrenceInstantProp(name string, master *ical.Event, instant time.Time) (*ical.Prop, error) {
	p := ical.NewProp(name)
	dtstart := master.Props.Get(ical.PropDateTimeStart)
	if dtstart != nil && isDateOnlyProp(dtstart) {
		p.SetDate(time.Date(instant.Year(), instant.Month(), instant.Day(), 0, 0, 0, 0, time.UTC))
		return p, nil
	}
	if dtstart != nil {
		if tzid := dtstart.Params.Get(ical.PropTimezoneID); tzid != "" {
			loc, err := time.LoadLocation(tzid)
			if err != nil {
				return nil, NewError(CodeProtocolError, 0, "Calendar event uses an unknown TZID", nil)
			}
			p.SetDateTime(instant.In(loc))
			return p, nil
		}
		if !strings.HasSuffix(dtstart.Value, "Z") {
			p.Value = instant.Format("20060102T150405")
			return p, nil
		}
	}
	p.SetDateTime(instant.UTC())
	return p, nil
}

// eventDurationKind identifies how a VEVENT expresses its duration.
type eventDurationKind int

const (
	eventDurationNone eventDurationKind = iota
	eventDurationEnd
	eventDurationDuration
	eventDurationImplicitAllDay
)

// eventDurationSpec is the parsed duration semantics of a VEVENT. A DURATION
// with day or week components is nominal calendar time, not elapsed hours.
type eventDurationSpec struct {
	kind      eventDurationKind
	allDay    bool
	days      int
	remainder time.Duration
	elapsed   time.Duration
	value     string
}

// eventDurationFromEvent derives the duration semantics from DTEND, then
// DURATION, then the all-day convention. A timed event with neither end nor
// duration has zero duration.
func eventDurationFromEvent(vevent *ical.Event) eventDurationSpec {
	if vevent == nil {
		return eventDurationSpec{}
	}
	stProp := vevent.Props.Get(ical.PropDateTimeStart)
	if stProp == nil {
		return eventDurationSpec{}
	}
	st, err := stProp.DateTime(time.UTC)
	if err != nil {
		return eventDurationSpec{}
	}
	if ep := vevent.Props.Get(ical.PropDateTimeEnd); ep != nil {
		en, e2 := ep.DateTime(time.UTC)
		if e2 == nil && en.After(st) {
			spec := eventDurationSpec{kind: eventDurationEnd, elapsed: en.Sub(st)}
			if isDateOnlyProp(stProp) && isDateOnlyProp(ep) {
				spec.allDay = true
				spec.days = int(en.Sub(st) / (24 * time.Hour))
			}
			return spec
		}
	}
	if dp := vevent.Props.Get(ical.PropDuration); dp != nil {
		if d, derr := parseICalDuration(dp.Value); derr == nil && d > 0 {
			days, remainder, derr := nominalICalDurationParts(dp.Value, d)
			if derr == nil {
				return eventDurationSpec{
					kind:      eventDurationDuration,
					days:      days,
					remainder: remainder,
					elapsed:   d,
					value:     dp.Value,
				}
			}
		}
	}
	if isDateOnlyProp(stProp) {
		return eventDurationSpec{kind: eventDurationImplicitAllDay, allDay: true, days: 1}
	}
	return eventDurationSpec{}
}

// end returns the occurrence end for a given start. A zero time means the
// VEVENT has no end property (zero duration).
func (s eventDurationSpec) end(start time.Time) time.Time {
	switch s.kind {
	case eventDurationEnd:
		if s.allDay {
			return start.AddDate(0, 0, s.days)
		}
		return start.Add(s.elapsed)
	case eventDurationImplicitAllDay:
		return start.AddDate(0, 0, 1)
	default:
		return time.Time{}
	}
}

// applyOccurrenceDuration sets or clears DTEND and DURATION on an override
// according to spec, anchoring the end at start. DURATION stays on the
// VEVENT so nominal days continue to follow calendar dates across DST.
func applyOccurrenceDuration(vevent *ical.Event, spec eventDurationSpec, start time.Time, startForm, endForm *ical.Prop) error {
	switch spec.kind {
	case eventDurationEnd:
		vevent.Props.Del(ical.PropDuration)
		return setEventDatePropWithForm(vevent, ical.PropDateTimeEnd, spec.end(start), endForm)
	case eventDurationDuration:
		vevent.Props.Del(ical.PropDateTimeEnd)
		prop := ical.NewProp(ical.PropDuration)
		prop.Value = spec.value
		vevent.Props.Set(prop)
		return nil
	case eventDurationImplicitAllDay:
		vevent.Props.Del(ical.PropDuration)
		return setEventDatePropWithForm(vevent, ical.PropDateTimeEnd, spec.end(start), startForm)
	default:
		vevent.Props.Del(ical.PropDateTimeEnd)
		vevent.Props.Del(ical.PropDuration)
		return nil
	}
}

// validateRemoteCalendar bounds the decoded shape before any event fields are
// copied or recurrence expansion duplicates a master. Messages deliberately
// identify only the violated class, never a remote property name or value.
func validateRemoteCalendar(cal *ical.Calendar) error {
	if cal == nil || cal.Component == nil {
		return NewError(CodeProtocolError, 0, "Calendar event data has no root component", nil)
	}
	components := []*ical.Component{cal.Component}
	totalComponents := 0
	totalProperties := 0
	overrides := 0
	events := 0
	alarms := 0
	exceptionDates := 0

	for len(components) > 0 {
		component := components[len(components)-1]
		components = components[:len(components)-1]
		if component == nil {
			return NewError(CodeProtocolError, 0, "Calendar event data contains an invalid component", nil)
		}
		if len(component.Name) > maxRemotePropertyNameBytes {
			return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its component-name limit", nil)
		}
		totalComponents++
		if totalComponents > maxRemoteComponents || totalComponents+len(components)+len(component.Children) > maxRemoteComponents {
			return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its component limit", nil)
		}
		components = append(components, component.Children...)

		componentProperties := 0
		for name, props := range component.Props {
			if len(name) > maxRemotePropertyNameBytes {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property-name limit", nil)
			}
			componentProperties += len(props)
			totalProperties += len(props)
			if componentProperties > maxRemotePropertiesPerComponent || totalProperties > maxRemoteProperties {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property limit", nil)
			}
			for i := range props {
				prop := &props[i]
				if len(prop.Name) > maxRemotePropertyNameBytes {
					return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property-name limit", nil)
				}
				if len(prop.Value) > maxRemotePropertyValueBytes {
					return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property-value limit", nil)
				}
				paramCount := 0
				for paramName, values := range prop.Params {
					paramCount += len(values)
					if len(paramName) > maxRemotePropertyNameBytes || paramCount > maxRemoteParamsPerProperty {
						return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property-parameter limit", nil)
					}
					for _, value := range values {
						if len(value) > maxRemoteParamBytes {
							return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its property-parameter limit", nil)
						}
						if paramName == ical.PropTimezoneID && len(value) > MaxUIDLen {
							return NewError(CodePayloadTooLarge, 0, "Calendar timezone identifier exceeded its byte limit", nil)
						}
					}
				}
				if limit := remoteEventFieldLimit(name); limit > 0 && len(prop.Value) > limit {
					return NewError(CodePayloadTooLarge, 0, "Calendar event field exceeded its byte limit", nil)
				}
				if name == ical.PropDuration {
					if _, err := parseICalDuration(prop.Value); err != nil {
						return NewError(CodeProtocolError, 0, "Calendar event duration is invalid", nil)
					}
				}
				if name == ical.PropTrigger {
					trigger := strings.ToUpper(strings.TrimSpace(prop.Value))
					if strings.HasPrefix(trigger, "P") || strings.HasPrefix(trigger, "+P") || strings.HasPrefix(trigger, "-P") {
						if _, err := parseICalDuration(trigger); err != nil {
							return NewError(CodeProtocolError, 0, "Calendar alarm trigger is invalid", nil)
						}
					}
				}
				if name == ical.PropExceptionDates {
					exceptionDates += strings.Count(prop.Value, ",") + 1
					if len(prop.Value) > 64<<10 || exceptionDates > maxRemoteExDates {
						return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its exception-date limit", nil)
					}
				}
			}
		}

		if component.Name == ical.CompEvent {
			if len(component.Props[ical.PropRecurrenceDates]) > 0 {
				return NewError(CodeProtocolError, 0, "Calendar event uses unsupported RDATE recurrence data", nil)
			}
			for i := range component.Props[ical.PropRecurrenceID] {
				if component.Props[ical.PropRecurrenceID][i].Params.Get(ical.ParamRange) != "" {
					return NewError(CodeProtocolError, 0, "Calendar event uses unsupported ranged recurrence data", nil)
				}
			}
			durations := component.Props[ical.PropDuration]
			ends := component.Props[ical.PropDateTimeEnd]
			if len(durations) > 1 || len(ends) > 1 || len(durations) > 0 && len(ends) > 0 {
				return NewError(CodeProtocolError, 0, "Calendar event has conflicting end-time properties", nil)
			}
			if len(durations) == 1 {
				duration, err := parseICalDuration(durations[0].Value)
				if err != nil || duration <= 0 {
					return NewError(CodeProtocolError, 0, "Calendar event duration is invalid", nil)
				}
				if start := component.Props.Get(ical.PropDateTimeStart); isDateOnlyProp(start) && strings.Contains(durations[0].Value, "T") {
					return NewError(CodeProtocolError, 0, "Calendar all-day event duration is invalid", nil)
				}
			}
			events++
			if events > maxRemoteOverrides+1 {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its event-component limit", nil)
			}
			if component.Props.Get(ical.PropRecurrenceID) != nil {
				overrides++
			}
			if overrides > maxRemoteOverrides {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its override limit", nil)
			}
		}
		if component.Name == ical.CompAlarm {
			alarms++
			if alarms > maxRemoteAlarms {
				return NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its alarm limit", nil)
			}
		}
	}
	return nil
}

// validateCalendarObjectIdentity enforces the VEVENT model for get and
// mutation paths. A complete resource has one master, zero or more overrides,
// and one UID. Messages intentionally omit all remote values.
func validateCalendarObjectIdentity(cal *ical.Calendar, expectedUID string) error {
	if cal == nil {
		return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
	}
	events := 0
	masters := 0
	overrides := 0
	uid := ""
	masterRecurring := false
	overrideIDs := make(map[int64]struct{})
	type componentPosition struct {
		component *ical.Component
		direct    bool
	}
	components := make([]componentPosition, 0, len(cal.Children))
	for _, child := range cal.Children {
		components = append(components, componentPosition{component: child, direct: true})
	}
	for len(components) > 0 {
		position := components[len(components)-1]
		components = components[:len(components)-1]
		child := position.component
		if child == nil {
			continue
		}
		for _, nested := range child.Children {
			components = append(components, componentPosition{component: nested})
		}
		if child.Name != ical.CompEvent {
			continue
		}
		if !position.direct {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		events++
		uidProps := child.Props[ical.PropUID]
		if len(uidProps) != 1 || uidProps[0].Value == "" {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		// RFC 5545 section 3.6.1 requires exactly one DTSTART, and DTEND and
		// DURATION are each optional at most once and mutually exclusive.
		// Without this check a missing start becomes Go's zero time and a
		// duplicate is silently selected by Props.Get.
		if len(child.Props[ical.PropDateTimeStart]) != 1 {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid or missing start time", nil)
		}
		if len(child.Props[ical.PropDateTimeEnd]) > 1 || len(child.Props[ical.PropDuration]) > 1 {
			return NewError(CodeProtocolError, 0, "Calendar event data has duplicate time properties", nil)
		}
		if len(child.Props[ical.PropDateTimeEnd]) == 1 && len(child.Props[ical.PropDuration]) == 1 {
			return NewError(CodeProtocolError, 0, "Calendar event data has both an end time and a duration", nil)
		}
		if len(child.Props[ical.PropDateTimeStamp]) > 1 {
			return NewError(CodeProtocolError, 0, "Calendar event data has duplicate time properties", nil)
		}
		uidProp := &uidProps[0]
		if uid == "" {
			uid = uidProp.Value
		} else if uidProp.Value != uid {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		recurrenceIDs := child.Props[ical.PropRecurrenceID]
		if len(recurrenceIDs) > 1 {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		recurrenceRules := child.Props[ical.PropRecurrenceRule]
		if len(recurrenceRules) > 1 || (len(recurrenceIDs) == 1 && len(recurrenceRules) != 0) {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		if len(recurrenceIDs) == 0 {
			masters++
			masterRecurring = len(recurrenceRules) == 1
			continue
		}
		overrides++
		recurrenceID, err := recurrenceIDs[0].DateTime(time.UTC)
		if err != nil {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		key := recurrenceID.UTC().Unix()
		if _, duplicate := overrideIDs[key]; duplicate {
			return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
		}
		overrideIDs[key] = struct{}{}
	}
	if events == 0 || masters != 1 || (overrides != 0 && !masterRecurring) || (expectedUID != "" && uid != expectedUID) {
		return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
	}
	return nil
}

// objectContentFingerprint compares the modeled master VEVENT properties that
// a create request controls. Server-managed fields (DTSTAMP, SEQUENCE,
// CREATED, LAST-MODIFIED) and property order do not participate.
func objectContentFingerprint(cal *ical.Calendar) (string, bool) {
	if cal == nil {
		return "", false
	}
	names := map[string]bool{
		ical.PropUID: true, ical.PropSummary: true, ical.PropLocation: true,
		ical.PropDescription: true, ical.PropDateTimeStart: true, ical.PropDateTimeEnd: true,
		ical.PropDuration: true, ical.PropRecurrenceRule: true, ical.PropExceptionDates: true,
		ical.PropStatus: true, ical.PropTransparency: true, ical.PropURL: true,
	}
	var lines []string
	for _, child := range cal.Children {
		if child == nil || child.Name != ical.CompEvent {
			continue
		}
		if len(child.Props[ical.PropRecurrenceID]) > 0 {
			continue
		}
		for name := range names {
			for _, property := range child.Props[name] {
				entry := name
				for param, values := range property.Params {
					for _, value := range values {
						entry += ";" + param + "=" + value
					}
				}
				lines = append(lines, entry+":"+property.Value)
			}
		}
		for _, alarm := range child.Children {
			if alarm == nil || alarm.Name != ical.CompAlarm {
				continue
			}
			for _, property := range alarm.Props[ical.PropTrigger] {
				lines = append(lines, ical.PropTrigger+":"+property.Value)
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), true
}

func remoteEventFieldLimit(name string) int {
	switch name {
	case ical.PropUID:
		return MaxUIDLen
	case ical.PropSummary:
		return MaxTitleLen
	case ical.PropLocation:
		return MaxLocationLen
	case ical.PropDescription:
		return MaxNotesLen
	case ical.PropURL:
		return MaxURLLen
	case ical.PropRecurrenceRule:
		return 1024
	case ical.PropStatus, ical.PropTransparency:
		return 64
	case ical.PropAction, ical.PropTrigger:
		return maxRemoteAlarmFieldBytes
	default:
		return 0
	}
}

func validateReadEventFields(event Event) error {
	if len(event.UID) > MaxUIDLen || len(event.Title) > MaxTitleLen || len(event.Location) > MaxLocationLen ||
		len(event.Notes) > MaxNotesLen || len(event.URL) > MaxURLLen || len(event.Recurrence) > 1024 ||
		len(event.Timezone) > MaxUIDLen || len(event.Status) > 64 || len(event.Transp) > 64 ||
		len(event.ETag) > maxETagBytes || len(event.Path) > 1024 {
		return NewError(CodePayloadTooLarge, 0, "Calendar event field exceeded its byte limit", nil)
	}
	return nil
}

// parseCalendarObject extracts the Events from a CalDAV object. One object
// may contain SEVERAL VEVENTs (master + RECURRENCE-ID exceptions): iterate
// over every VEVENT child and split master from overrides.
func parseCalendarObject(obj *extcaldav.CalendarObject) (*Event, []Event, error) {
	if obj.Data == nil {
		return nil, nil, NewError(CodeProtocolError, 0, "Calendar event data is missing", nil)
	}
	if err := validateRemoteCalendar(obj.Data); err != nil {
		return nil, nil, err
	}
	if err := validateCalendarObjectIdentity(obj.Data, ""); err != nil {
		return nil, nil, err
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
		master = ev
		exDates = evExDates
	}

	if master == nil {
		return nil, nil, NewError(CodeProtocolError, 0, "Calendar event data has no master event", nil)
	}
	for i := range overrides {
		if overrides[i].AllDay != master.AllDay || overrides[i].IsFloating != master.IsFloating {
			return nil, nil, NewError(CodeProtocolError, 0, "Calendar recurrence override time form does not match the master", nil)
		}
	}
	groupScheduled, err := schedulingIndicators(obj.Data)
	if err != nil {
		return nil, nil, err
	}
	master.IsGroupScheduled = groupScheduled
	for i := range overrides {
		overrides[i].IsGroupScheduled = groupScheduled
	}
	master.exDates = exDates
	return master, overrides, nil
}

// SchedulingNotificationRisk is the fixed warning returned for an object that
// carries ORGANIZER or ATTENDEE. It deliberately names no address.
const SchedulingNotificationRisk = "the event carries scheduling metadata; a scheduling-capable server can notify organizers or attendees about a series or occurrence change"

// schedulingIndicators reports whether the complete calendar object has
// ORGANIZER or ATTENDEE. It validates each cal-address value and fails closed
// on an empty, oversized, control-bearing, or URI-less value. The result is a
// boolean: identities never leave the parser.
func schedulingIndicators(cal *ical.Calendar) (bool, error) {
	if cal == nil {
		return false, nil
	}
	group := false
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		for _, name := range []string{ical.PropOrganizer, ical.PropAttendee} {
			for _, prop := range child.Props[name] {
				value := strings.TrimSpace(prop.Value)
				if value == "" || len(value) > maxRemotePropertyValueBytes ||
					strings.ContainsAny(value, "\r\n\x00") {
					return false, NewError(CodeProtocolError, 0, "Calendar scheduling metadata is malformed", nil)
				}
				parsed, err := url.Parse(value)
				if err != nil || parsed.Scheme == "" {
					return false, NewError(CodeProtocolError, 0, "Calendar scheduling metadata is malformed", nil)
				}
				group = true
			}
		}
	}
	return group, nil
}

func eventText(props ical.Props, name string) (string, error) {
	prop := props.Get(name)
	if prop == nil {
		return "", nil
	}
	textProp := *prop
	// RFC 5545 VALUE tokens ignore case, but go-ical checks them case-sensitively.
	if valueType := prop.Params.Get(ical.ParamValue); valueType != string(ical.ValueText) && strings.EqualFold(valueType, string(ical.ValueText)) {
		textProp.Params = maps.Clone(prop.Params)
		textProp.Params.Set(ical.ParamValue, string(ical.ValueText))
	}
	return textProp.Text()
}

func parseVEvent(vevent *ical.Event, path string) (ev *Event, isOverride bool, exDates []time.Time, err error) {
	e := &Event{Path: path}

	if p := vevent.Props.Get(ical.PropUID); p != nil {
		e.UID = p.Value
	}
	if e.Title, err = eventText(vevent.Props, ical.PropSummary); err != nil {
		return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event title is invalid", nil)
	}
	if e.Location, err = eventText(vevent.Props, ical.PropLocation); err != nil {
		return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event location is invalid", nil)
	}
	if e.Notes, err = eventText(vevent.Props, ical.PropDescription); err != nil {
		return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event notes are invalid", nil)
	}

	if p := vevent.Props.Get(ical.PropDateTimeStart); p != nil {
		parsed, derr := parseEventDateTimeProp(p)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event start time is invalid", nil)
		}
		e.StartTime = parsed.instant
		e.AllDay = parsed.form == formDate
		e.IsFloating = parsed.form == formFloating
		e.DSTAdjusted = parsed.adjusted
		e.wallStart = parsed.wall
		if parsed.tzid != "" {
			e.Timezone = parsed.tzid
		}
	}
	if p := vevent.Props.Get(ical.PropDateTimeEnd); p != nil {
		parsed, derr := parseEventDateTimeProp(p)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event end time is invalid", nil)
		}
		if start := vevent.Props.Get(ical.PropDateTimeStart); start != nil {
			if err := validateDateTimeFormPair(start, p); err != nil {
				return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event start and end time forms do not match", nil)
			}
		}
		e.EndTime = parsed.instant
		e.DSTAdjusted = e.DSTAdjusted || parsed.adjusted
	} else if durProp := vevent.Props.Get(ical.PropDuration); durProp != nil {
		// DTEND absent but DURATION present (RFC 5545 section 3.6.1: DTEND and
		// DURATION are mutually exclusive; DURATION is the valid
		// alternative). Without this derivation EndTime would stay zero: the
		// event would vanish from search (eventOverlaps always false) or
		// produce a negative duration during recurrence expansion.
		dur, derr := parseICalDuration(durProp.Value)
		if derr != nil || dur <= 0 {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event duration is invalid", nil)
		}
		days, remainder, derr := nominalICalDurationParts(durProp.Value, dur)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event duration is invalid", nil)
		}
		e.hasNominalDuration = days != 0
		e.nominalDurationDays = days
		e.nominalDurationRemainder = remainder
		if e.hasNominalDuration {
			e.EndTime = recurrenceOccurrenceEnd(*e, e.StartTime)
		} else {
			e.EndTime = e.StartTime.Add(dur)
		}
	} else if e.AllDay {
		// Neither DTEND nor DURATION on an all-day event (bare
		// DTSTART;VALUE=DATE): one full day by convention.
		e.hasNominalDuration = true
		e.nominalDurationDays = 1
		e.EndTime = e.StartTime.AddDate(0, 0, 1)
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
		parsed, derr := parseEventDateTimeProp(p)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar recurrence identifier is invalid", nil)
		}
		e.RecurrenceID = parsed.instant
		e.IsOverride = true
		isOverride = true
	}

	for _, p := range vevent.Props[ical.PropExceptionDates] {
		prop := p
		// RFC 5545 section 3.8.5.1 requires EXDATE to use the same value
		// type as DTSTART. A DATE value against a DATE-TIME series (or the
		// reverse) parses to an instant that never matches an occurrence, so
		// the exclusion would silently do nothing. Fail closed instead.
		if err := validateExDateForm(vevent.Props.Get(ical.PropDateTimeStart), &prop); err != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar exception date value type does not match DTSTART", nil)
		}
		dates, derr := parseExDateProp(&prop)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar exception date is invalid", nil)
		}
		exDates = append(exDates, dates...)
	}

	return e, isOverride, exDates, nil
}

// iCalDateTimeForm classifies an iCalendar DATE or DATE-TIME value. The four
// forms need different comparison rules: DATE has no time, UTC and TZID values
// denote real instants, and a floating value has no offset at all.
type iCalDateTimeForm int

const (
	formMissing iCalDateTimeForm = iota
	formDate
	formUTC
	formZoned
	formFloating
)

// dateTimeForm classifies one raw value using the property TZID parameter.
func dateTimeForm(value, tzid string) iCalDateTimeForm {
	switch {
	case value == "":
		return formMissing
	case len(value) == 8 && !strings.Contains(value, "T"):
		return formDate
	case strings.HasSuffix(value, "Z"):
		return formUTC
	case tzid != "":
		return formZoned
	default:
		return formFloating
	}
}

// propDateTimeForm classifies a DTSTART or EXDATE property including the
// VALUE parameter. A VALUE=DATE property is always a date even when the raw
// value is malformed.
func propDateTimeForm(p *ical.Prop) iCalDateTimeForm {
	if p == nil {
		return formMissing
	}
	if isDateOnlyProp(p) {
		return formDate
	}
	return dateTimeForm(p.Value, p.Params.Get(ical.PropTimezoneID))
}

// parsedEventDateTime is one DTSTART, DTEND, or RECURRENCE-ID value with its
// original iCalendar form. wall holds the naive local components for a zoned
// or floating value, so recurrence expansion never depends on a normalized
// instant.
type parsedEventDateTime struct {
	instant  time.Time
	wall     time.Time
	form     iCalDateTimeForm
	tzid     string
	adjusted bool
}

// parseEventDateTimeProp parses an event date property without losing its
// form. A floating value keeps its wall-clock components in UTC; no offset is
// invented.
func parseEventDateTimeProp(p *ical.Prop) (parsedEventDateTime, error) {
	if p == nil {
		return parsedEventDateTime{}, fmt.Errorf("missing property")
	}
	form := propDateTimeForm(p)
	tzid := p.Params.Get(ical.PropTimezoneID)
	switch form {
	case formDate:
		t, err := time.ParseInLocation("20060102", p.Value, time.UTC)
		if err != nil {
			return parsedEventDateTime{}, err
		}
		return parsedEventDateTime{instant: t, wall: t, form: form}, nil
	case formUTC:
		t, err := time.Parse("20060102T150405Z", p.Value)
		if err != nil {
			return parsedEventDateTime{}, err
		}
		return parsedEventDateTime{instant: t, wall: t, form: form}, nil
	case formZoned:
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			return parsedEventDateTime{}, fmt.Errorf("unknown TZID %q", tzid)
		}
		wall, err := time.ParseInLocation("20060102T150405", p.Value, time.UTC)
		if err != nil {
			return parsedEventDateTime{}, err
		}
		t, adjusted := ResolveWallClock(wall, loc)
		return parsedEventDateTime{instant: t, wall: wall, form: form, tzid: tzid, adjusted: adjusted}, nil
	case formFloating:
		wall, err := time.ParseInLocation("20060102T150405", p.Value, time.UTC)
		if err != nil {
			return parsedEventDateTime{}, err
		}
		return parsedEventDateTime{instant: wall, wall: wall, form: form}, nil
	default:
		return parsedEventDateTime{}, fmt.Errorf("invalid date-time value")
	}
}

// validateDateTimeFormPair rejects DTSTART/DTEND forms that cannot describe
// one interval. DATE and DATE-TIME never mix. A floating value has no instant,
// so it cannot pair with an absolute value.
func validateDateTimeFormPair(start, end *ical.Prop) error {
	startForm := propDateTimeForm(start)
	endForm := propDateTimeForm(end)
	if startForm == formMissing || endForm == formMissing {
		return fmt.Errorf("missing date-time form")
	}
	if (startForm == formDate) != (endForm == formDate) {
		return fmt.Errorf("DATE and DATE-TIME differ")
	}
	if startForm != formDate && (startForm == formFloating) != (endForm == formFloating) {
		return fmt.Errorf("floating and absolute date-times differ")
	}
	return nil
}

// validateExDateForm rejects an EXDATE whose value type cannot match the
// recurring master DTSTART. DATE and DATE-TIME never match. A floating
// DATE-TIME and an absolute (UTC or TZID) DATE-TIME also never match, because
// the floating value has no instant. Between absolute forms the instant is
// comparable, so a TZID EXDATE against a UTC DTSTART (or the reverse) stays
// valid. Every comma-separated value must use the property form.
func validateExDateForm(start *ical.Prop, ex *ical.Prop) error {
	if ex == nil {
		return nil
	}
	exForm := propDateTimeForm(ex)
	startForm := propDateTimeForm(start)
	if startForm == formMissing {
		return fmt.Errorf("missing DTSTART")
	}
	if exForm == formMissing {
		return fmt.Errorf("empty EXDATE")
	}
	if (startForm == formDate) != (exForm == formDate) {
		return fmt.Errorf("EXDATE value type does not match DTSTART")
	}
	if startForm != formDate && (startForm == formFloating) != (exForm == formFloating) {
		return fmt.Errorf("EXDATE time form does not match DTSTART")
	}
	tzid := ex.Params.Get(ical.PropTimezoneID)
	for _, part := range strings.Split(ex.Value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("empty EXDATE value")
		}
		if form := dateTimeForm(part, tzid); form != exForm {
			return fmt.Errorf("EXDATE value list mixes forms")
		}
	}
	return nil
}

// parseExDateProp parses an EXDATE property, which may carry several
// comma-separated dates (RFC 5545 section 3.8.5.1). go-ical Prop.DateTime only
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
		if len(out) >= maxRemoteAlarms {
			break
		}
	}
	return out
}

// parseTriggerMinutesBefore parses common -PTnM / -PTnH triggers.
func parseTriggerMinutesBefore(v string) int {
	duration, err := parseICalDuration(v)
	if err != nil || duration >= 0 || duration%time.Minute != 0 {
		return 0
	}
	minutes := -(duration / time.Minute)
	if minutes <= 0 || int64(int(minutes)) != int64(minutes) {
		return 0
	}
	return int(minutes)
}

func parseICalDuration(value string) (time.Duration, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return 0, fmt.Errorf("invalid iCalendar duration")
	}
	negative := false
	if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		negative = value[0] == '-'
		value = value[1:]
	}
	if len(value) < 2 || value[0] != 'P' {
		return 0, fmt.Errorf("invalid iCalendar duration")
	}
	value = value[1:]
	if len(value) >= 2 && value[len(value)-1] == 'W' {
		digits := value[:len(value)-1]
		if digits == "" || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, fmt.Errorf("invalid iCalendar duration")
		}
		count, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid iCalendar duration")
		}
		return checkedICalDuration(count, 7*24*time.Hour, negative)
	}
	inTime := false
	seenDay := false
	seenTimePart := false
	lastTimePart := 0
	var total uint64
	const maxDurationMagnitude = uint64(1<<63 - 1)
	for len(value) > 0 {
		if value[0] == 'T' {
			if inTime {
				return 0, fmt.Errorf("invalid iCalendar duration")
			}
			inTime = true
			value = value[1:]
			continue
		}
		digits := 0
		for digits < len(value) && value[digits] >= '0' && value[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits == len(value) {
			return 0, fmt.Errorf("invalid iCalendar duration")
		}
		count, err := strconv.ParseUint(value[:digits], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid iCalendar duration")
		}
		designator := value[digits]
		value = value[digits+1:]
		var unit time.Duration
		switch designator {
		case 'D':
			if inTime || seenDay {
				return 0, fmt.Errorf("invalid iCalendar duration")
			}
			unit = 24 * time.Hour
			seenDay = true
		case 'H':
			if !inTime || lastTimePart >= 1 {
				return 0, fmt.Errorf("invalid iCalendar duration")
			}
			unit = time.Hour
			lastTimePart = 1
			seenTimePart = true
		case 'M':
			if !inTime || lastTimePart >= 2 {
				return 0, fmt.Errorf("invalid iCalendar duration")
			}
			unit = time.Minute
			lastTimePart = 2
			seenTimePart = true
		case 'S':
			if !inTime || lastTimePart >= 3 {
				return 0, fmt.Errorf("invalid iCalendar duration")
			}
			unit = time.Second
			lastTimePart = 3
			seenTimePart = true
		default:
			return 0, fmt.Errorf("invalid iCalendar duration")
		}
		unitMagnitude := uint64(unit)
		if count > maxDurationMagnitude/unitMagnitude {
			return 0, fmt.Errorf("iCalendar duration overflows its runtime representation")
		}
		part := count * unitMagnitude
		if total > maxDurationMagnitude-part {
			return 0, fmt.Errorf("iCalendar duration overflows its runtime representation")
		}
		total += part
	}
	if !seenDay && !seenTimePart || inTime && !seenTimePart {
		return 0, fmt.Errorf("invalid iCalendar duration")
	}
	duration := time.Duration(total)
	if negative {
		duration = -duration
	}
	return duration, nil
}

func nominalICalDurationParts(value string, total time.Duration) (int, time.Duration, error) {
	unsigned := strings.TrimPrefix(value, "+")
	if strings.HasPrefix(unsigned, "-") || !strings.HasPrefix(unsigned, "P") {
		return 0, 0, fmt.Errorf("invalid nominal iCalendar duration")
	}
	var dayCount uint64
	var err error
	if strings.HasSuffix(unsigned, "W") {
		dayCount, err = strconv.ParseUint(unsigned[1:len(unsigned)-1], 10, 64)
		if dayCount > uint64(^uint(0)>>1)/7 {
			return 0, 0, fmt.Errorf("nominal iCalendar duration overflows")
		}
		dayCount *= 7
	} else if dayEnd := strings.IndexByte(unsigned, 'D'); dayEnd >= 0 {
		dayCount, err = strconv.ParseUint(unsigned[1:dayEnd], 10, 64)
	}
	if err != nil || dayCount > uint64(^uint(0)>>1) {
		return 0, 0, fmt.Errorf("invalid nominal iCalendar duration")
	}
	days := int(dayCount)
	dayDuration := time.Duration(days) * 24 * time.Hour
	return days, total - dayDuration, nil
}

func checkedICalDuration(count uint64, unit time.Duration, negative bool) (time.Duration, error) {
	const maxDurationMagnitude = uint64(1<<63 - 1)
	unitMagnitude := uint64(unit)
	if count > maxDurationMagnitude/unitMagnitude {
		return 0, fmt.Errorf("iCalendar duration overflows its runtime representation")
	}
	duration := time.Duration(count * unitMagnitude)
	if negative {
		duration = -duration
	}
	return duration, nil
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
		if tzid == "" {
			// Floating value: keep the wall-clock components in UTC.
			return time.ParseInLocation("20060102T150405", value, time.UTC)
		}
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, fmt.Errorf("unknown TZID %q", tzid)
		}
		wall, err := time.ParseInLocation("20060102T150405", value, time.UTC)
		if err != nil {
			return time.Time{}, err
		}
		resolved, _ := ResolveWallClock(wall, loc)
		return resolved, nil
	}
}
