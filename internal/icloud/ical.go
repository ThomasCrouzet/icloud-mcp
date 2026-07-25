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
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
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
	if strings.HasSuffix(existing.Value, "Z") {
		vevent.Props.SetDateTime(name, t.UTC())
		return
	}
	// A DATE-TIME without TZID or Z is floating. Keep it floating and retain
	// any non-timezone parameters rather than converting wall time to UTC.
	prop := *existing
	prop.Value = t.Format("20060102T150405")
	prop.Params.Del(ical.PropTimezoneID)
	vevent.Props.Set(&prop)
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
		p.SetDate(time.Date(instant.Year(), instant.Month(), instant.Day(), 0, 0, 0, 0, time.UTC))
		return p
	}
	if dtstart != nil {
		if tzid := dtstart.Params.Get(ical.PropTimezoneID); tzid != "" {
			if loc, err := time.LoadLocation(tzid); err == nil {
				p.SetDateTime(instant.In(loc))
				return p
			}
		}
		if !strings.HasSuffix(dtstart.Value, "Z") {
			p.Value = instant.Format("20060102T150405")
			return p
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
		if d, derr := parseICalDuration(dp.Value); derr == nil && d > 0 {
			return d
		}
	}
	if isDateOnlyProp(stProp) {
		return 24 * time.Hour
	}
	return time.Hour
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

// validateCalendarObjectIdentity enforces the VEVENT object model relied on by
// get and mutation paths: one master, zero or more overrides, and one UID for
// the complete resource. Messages intentionally omit all remote values.
func validateCalendarObjectIdentity(cal *ical.Calendar, expectedUID string) error {
	if cal == nil {
		return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
	}
	events := 0
	masters := 0
	uid := ""
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
		if len(recurrenceIDs) == 0 {
			masters++
		}
	}
	if events == 0 || masters != 1 || (expectedUID != "" && uid != expectedUID) {
		return NewError(CodeProtocolError, 0, "Calendar event data has an invalid VEVENT identity", nil)
	}
	return nil
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
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event start time is invalid", nil)
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
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar event end time is invalid", nil)
		}
		e.EndTime = t
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
		t, derr := p.DateTime(time.UTC)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar recurrence identifier is invalid", nil)
		}
		e.RecurrenceID = t
		e.IsOverride = true
		isOverride = true
	}

	for _, p := range vevent.Props[ical.PropExceptionDates] {
		prop := p
		dates, derr := parseExDateProp(&prop)
		if derr != nil {
			return nil, false, nil, NewError(CodeProtocolError, 0, "Calendar exception date is invalid", nil)
		}
		exDates = append(exDates, dates...)
	}

	return e, isOverride, exDates, nil
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
		loc := time.UTC
		if tzid != "" {
			if l, err := time.LoadLocation(tzid); err == nil {
				loc = l
			}
		}
		return time.ParseInLocation("20060102T150405", value, loc)
	}
}
