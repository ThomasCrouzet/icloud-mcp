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

// newUID génère un UID d'événement via crypto/rand (16 octets hex), pas de
// dépendance google/uuid (interdite par la spec).
func newUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("génération UID : %w", err)
	}
	return fmt.Sprintf("%s@icloud-mcp", hex.EncodeToString(buf)), nil
}

// buildEventCalendar construit le VCALENDAR complet d'un nouvel événement.
func buildEventCalendar(uid string, ne *NewEvent) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//icloud-mcp//FR")

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetText(ical.PropSummary, ne.Title)
	if ne.Location != "" {
		ev.Props.SetText(ical.PropLocation, ne.Location)
	}
	if ne.Notes != "" {
		ev.Props.SetText(ical.PropDescription, ne.Notes)
	}
	ev.Props.SetDateTime(ical.PropDateTimeStart, ne.StartTime.UTC())
	ev.Props.SetDateTime(ical.PropDateTimeEnd, ne.EndTime.UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())

	if ne.AlarmMinutesBefore > 0 {
		alarm := ical.NewComponent(ical.CompAlarm)
		alarm.Props.SetText(ical.PropAction, "DISPLAY")
		alarm.Props.SetText(ical.PropDescription, "Rappel")
		trigger := ical.NewProp(ical.PropTrigger)
		trigger.Value = fmt.Sprintf("-PT%dM", ne.AlarmMinutesBefore) // valeur DURATION brute, pas SetText
		alarm.Props.Set(trigger)
		ev.Children = append(ev.Children, alarm)
	}

	cal.Children = append(cal.Children, ev.Component)
	return cal
}

// findMasterVEvent retourne le VEVENT « maître » (sans RECURRENCE-ID) d'un
// objet calendrier. Les éventuels VEVENT d'override (exceptions
// RECURRENCE-ID) sont ignorés, update_event ne modifie que le maître.
func findMasterVEvent(cal *ical.Calendar) (*ical.Event, error) {
	if cal == nil {
		return nil, fmt.Errorf("objet calendrier sans données")
	}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		vevent := ical.NewEvent()
		vevent.Component = child
		if p := vevent.Props.Get(ical.PropRecurrenceID); p != nil {
			continue // override, pas le maître
		}
		return vevent, nil
	}
	return nil, fmt.Errorf("aucun VEVENT maître trouvé dans l'objet")
}

// setSequence pose SEQUENCE en tant que propriété INTEGER (le type par
// défaut de go-ical pour cette propriété), ne PAS utiliser SetText, qui
// ajouterait un paramètre VALUE=TEXT superflu et sémantiquement incorrect.
func setSequence(vevent *ical.Event, n int) {
	prop := ical.NewProp(ical.PropSequence)
	prop.Value = strconv.Itoa(n)
	vevent.Props.Set(prop)
}

// setEventDateProp pose une date de début/fin en préservant le format
// all-day (date pure, 8 caractères) si la propriété existante était déjà en
// date pure, ne jamais convertir un événement all-day en datetime lors
// d'une mise à jour.
func setEventDateProp(vevent *ical.Event, name string, t time.Time) {
	existing := vevent.Props.Get(name)
	if existing != nil && len(existing.Value) == 8 {
		vevent.Props.SetDate(name, t.UTC())
		return
	}
	vevent.Props.SetDateTime(name, t.UTC())
}

// parseCalendarObject extrait les Event d'un objet CalDAV. Un objet peut
// contenir PLUSIEURS VEVENT (master + exceptions RECURRENCE-ID) : on itère
// tous les enfants VEVENT et on sépare master/overrides.
func parseCalendarObject(obj *extcaldav.CalendarObject) (*Event, []Event, error) {
	if obj.Data == nil {
		return nil, nil, fmt.Errorf("objet calendrier sans données (path=%s)", obj.Path)
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
			continue // deux masters dans le même objet : anomalie, on garde le premier
		}
		master = ev
		exDates = evExDates
	}

	if master == nil {
		return nil, nil, fmt.Errorf("aucun VEVENT maître trouvé dans l'objet (path=%s)", obj.Path)
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
			return nil, false, nil, fmt.Errorf("DTSTART invalide (uid=%s) : %w", e.UID, derr)
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
			return nil, false, nil, fmt.Errorf("DTEND invalide (uid=%s) : %w", e.UID, derr)
		}
		e.EndTime = t
	} else if durProp := vevent.Props.Get(ical.PropDuration); durProp != nil {
		// DTEND absent mais DURATION présente (RFC 5545 §3.6.1 : DTEND et
		// DURATION sont mutuellement exclusives, DURATION est l'alternative
		// valide). Sans cette dérivation, EndTime resterait zéro : l'event
		// disparaîtrait de search (eventOverlaps toujours faux) ou
		// produirait une durée négative en récurrence.
		dur, derr := durProp.Duration()
		if derr != nil {
			return nil, false, nil, fmt.Errorf("DURATION invalide (uid=%s) : %w", e.UID, derr)
		}
		e.EndTime = e.StartTime.Add(dur)
	} else if e.AllDay {
		// Ni DTEND ni DURATION, événement all-day (DTSTART;VALUE=DATE seul) :
		// un jour complet par convention.
		e.EndTime = e.StartTime.Add(24 * time.Hour)
	} else {
		// Ni DTEND ni DURATION, événement daté : durée nulle plutôt que
		// zéro-time (qui casserait eventOverlaps).
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
			return nil, false, nil, fmt.Errorf("EXDATE invalide (uid=%s) : %w", e.UID, derr)
		}
		exDates = append(exDates, dates...)
	}

	return e, isOverride, exDates, nil
}

// parseExDateProp parse une propriété EXDATE, qui peut porter plusieurs
// dates séparées par des virgules (RFC 5545 §3.8.5.1). go-ical
// Prop.DateTime ne gère qu'une seule valeur : on découpe manuellement.
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

// parseICalDateTimeValue parse une valeur date/date-time iCalendar brute
// (formats "20060102", "20060102T150405Z" ou "20060102T150405" + TZID).
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
