package icloud

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestApplyOccurrenceUpdate_StartOnlyKeepsDuration(t *testing.T) {
	masterStart := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)
	masterEnd := masterStart.Add(time.Hour)
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "uid-occ")
	master.Props.SetText(ical.PropSummary, "Weekly")
	master.Props.SetDateTime(ical.PropDateTimeStart, masterStart)
	master.Props.SetDateTime(ical.PropDateTimeEnd, masterEnd)
	rr := ical.NewProp(ical.PropRecurrenceRule)
	rr.Value = "FREQ=WEEKLY;COUNT=4"
	master.Props.Set(rr)
	cal.Children = append(cal.Children, master.Component)

	recID := time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)
	newStart := time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC)
	up := &EventUpdate{StartTime: &newStart}
	if err := applyOccurrenceUpdate(cal, master, recID, up); err != nil {
		t.Fatalf("applyOccurrenceUpdate: %v", err)
	}
	var override *ical.Component
	for _, ch := range cal.Children {
		if ch.Name != ical.CompEvent {
			continue
		}
		if p := ch.Props.Get(ical.PropRecurrenceID); p != nil {
			override = ch
			break
		}
	}
	if override == nil {
		t.Fatal("expected override VEVENT")
	}
	ov := &ical.Event{Component: override}
	st, err := ov.Props.Get(ical.PropDateTimeStart).DateTime(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	en, err := ov.Props.Get(ical.PropDateTimeEnd).DateTime(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Equal(newStart) {
		t.Errorf("start = %v, want %v", st, newStart)
	}
	if !en.Equal(newStart.Add(time.Hour)) {
		t.Errorf("end = %v, want start+1h %v", en, newStart.Add(time.Hour))
	}
}

func TestExpandOccurrences_ExposesRecurrenceIDClearsRRULE(t *testing.T) {
	master := Event{
		UID:        "uid-daily",
		Title:      "Daily",
		StartTime:  mustParse(t, "2026-07-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-07-01T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=3",
	}
	out, _, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-07-31T00:00:00Z"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("len=%d", len(out))
	}
	for i, ev := range out {
		if ev.Recurrence != "" {
			t.Errorf("occ %d still has RRULE %q", i, ev.Recurrence)
		}
		if ev.RecurrenceID.IsZero() {
			t.Errorf("occ %d missing recurrenceId", i)
		}
		if !ev.RecurrenceID.Equal(ev.StartTime) {
			t.Errorf("occ %d recurrenceId %v != start %v", i, ev.RecurrenceID, ev.StartTime)
		}
		if ev.IsOverride {
			t.Errorf("occ %d should not be override", i)
		}
	}
}

func TestBuildEventCalendar_RecurringTimezone(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	cal := buildEventCalendar("u@x", &NewEvent{
		Title:      "Standup",
		StartTime:  time.Date(2026, 7, 1, 10, 0, 0, 0, paris),
		EndTime:    time.Date(2026, 7, 1, 10, 30, 0, 0, paris),
		Recurrence: "FREQ=WEEKLY;COUNT=10",
		Timezone:   "Europe/Paris",
	})
	var buf strings.Builder
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "VTIMEZONE") {
		t.Errorf("missing VTIMEZONE:\n%s", body)
	}
	if !strings.Contains(body, "Europe/Paris") {
		t.Errorf("missing TZID:\n%s", body)
	}
	if strings.Contains(body, "DTSTART:20260701T080000Z") {
		t.Errorf("recurring local series must not force UTC Z:\n%s", body)
	}
}
