package icloud

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func newRecurringCalendarWithDuration(t *testing.T, uid, duration string, start time.Time) (*ical.Calendar, *ical.Event) {
	t.Helper()
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, uid)
	master.Props.SetText(ical.PropSummary, "Recurring")
	master.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	master.Props.SetDateTime(ical.PropDateTimeStart, start)
	if duration != "" {
		prop := ical.NewProp(ical.PropDuration)
		prop.Value = duration
		master.Props.Set(prop)
	}
	rrule := ical.NewProp(ical.PropRecurrenceRule)
	rrule.Value = "FREQ=DAILY;COUNT=5"
	master.Props.Set(rrule)
	cal.Children = append(cal.Children, master.Component)
	return cal, master
}

func decodeEvents(t *testing.T, cal *ical.Calendar) (*Event, []Event) {
	t.Helper()
	var body strings.Builder
	if err := ical.NewEncoder(&body).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := ical.NewDecoder(strings.NewReader(body.String())).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var master *Event
	var overrides []Event
	for _, child := range decoded.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		vevent := ical.NewEvent()
		vevent.Component = child
		ev, isOverride, _, err := parseVEvent(vevent, "/cal/event.ics")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if isOverride {
			overrides = append(overrides, *ev)
			continue
		}
		master = ev
	}
	if master == nil {
		t.Fatal("decoded calendar has no master")
	}
	return master, overrides
}

func TestOccurrenceDurationPreservesNominalDaysAcrossDST(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		duration     string
		start        time.Time
		recID        time.Time
		newStart     *time.Time
		wantStart    time.Time
		wantEnd      time.Time
		wantDuration string
	}{
		{
			name:      "spring title-only P1D",
			duration:  "P1D",
			start:     time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			recID:     time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			wantStart: time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			wantEnd:   time.Date(2026, 3, 29, 10, 0, 0, 0, paris),
		},
		{
			name:      "spring start-only P1D",
			duration:  "P1D",
			start:     time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			recID:     time.Date(2026, 3, 29, 10, 0, 0, 0, paris),
			newStart:  ptrTime(time.Date(2026, 3, 28, 10, 0, 0, 0, paris)),
			wantStart: time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			wantEnd:   time.Date(2026, 3, 29, 10, 0, 0, 0, paris),
		},
		{
			name:      "autumn start-only P1D",
			duration:  "P1D",
			start:     time.Date(2026, 10, 24, 10, 0, 0, 0, paris),
			recID:     time.Date(2026, 10, 25, 10, 0, 0, 0, paris),
			newStart:  ptrTime(time.Date(2026, 10, 24, 10, 0, 0, 0, paris)),
			wantStart: time.Date(2026, 10, 24, 10, 0, 0, 0, paris),
			wantEnd:   time.Date(2026, 10, 25, 10, 0, 0, 0, paris),
		},
		{
			name:      "elapsed PT24H stays elapsed",
			duration:  "PT24H",
			start:     time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			recID:     time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			wantStart: time.Date(2026, 3, 28, 10, 0, 0, 0, paris),
			wantEnd:   time.Date(2026, 3, 29, 11, 0, 0, 0, paris),
		},
		{
			name:      "P1W title-only",
			duration:  "P1W",
			start:     time.Date(2026, 3, 25, 9, 0, 0, 0, paris),
			recID:     time.Date(2026, 3, 25, 9, 0, 0, 0, paris),
			wantStart: time.Date(2026, 3, 25, 9, 0, 0, 0, paris),
			wantEnd:   time.Date(2026, 4, 1, 9, 0, 0, 0, paris),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cal, master := newRecurringCalendarWithDuration(t, "dst-"+test.name, test.duration, test.start)
			title := "Renamed"
			up := &EventUpdate{Title: &title, StartTime: test.newStart}
			if err := applyOccurrenceUpdate(cal, master, test.recID, up); err != nil {
				t.Fatalf("applyOccurrenceUpdate: %v", err)
			}
			decodedMaster, overrides := decodeEvents(t, cal)
			if len(overrides) != 1 {
				t.Fatalf("overrides = %d, want 1", len(overrides))
			}
			got := overrides[0]
			if !got.StartTime.Equal(test.wantStart) {
				t.Errorf("start = %v, want %v", got.StartTime, test.wantStart)
			}
			if !got.EndTime.Equal(test.wantEnd) {
				t.Errorf("end = %v, want %v", got.EndTime, test.wantEnd)
			}
			if got.RecurrenceID.IsZero() {
				t.Error("override lost recurrenceId")
			}
			if !decodedMaster.StartTime.Equal(test.start) {
				t.Errorf("master start changed to %v", decodedMaster.StartTime)
			}
			if test.duration != "" {
				prop := findOverrideProp(t, cal, ical.PropDuration)
				if prop == nil || prop.Value != test.duration {
					t.Errorf("override DURATION = %+v, want %q", prop, test.duration)
				}
				if findOverrideProp(t, cal, ical.PropDateTimeEnd) != nil {
					t.Error("override mixes DTEND with DURATION")
				}
			}
		})
	}
}

func TestOccurrenceAllDayWithoutEndKeepsDateBounds(t *testing.T) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	master := ical.NewEvent()
	master.Props.SetText(ical.PropUID, "all-day-no-end")
	master.Props.SetText(ical.PropSummary, "All day")
	master.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	master.Props.SetDate(ical.PropDateTimeStart, time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC))
	rrule := ical.NewProp(ical.PropRecurrenceRule)
	rrule.Value = "FREQ=DAILY;COUNT=3"
	master.Props.Set(rrule)
	cal.Children = append(cal.Children, master.Component)

	title := "Renamed all day"
	recID := time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC)
	if err := applyOccurrenceUpdate(cal, master, recID, &EventUpdate{Title: &title}); err != nil {
		t.Fatalf("applyOccurrenceUpdate: %v", err)
	}
	_, overrides := decodeEvents(t, cal)
	if len(overrides) != 1 {
		t.Fatalf("overrides = %d", len(overrides))
	}
	got := overrides[0]
	if !got.AllDay {
		t.Error("override is not all-day")
	}
	if !got.StartTime.Equal(time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", got.StartTime)
	}
	if !got.EndTime.Equal(time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %v, want next DATE", got.EndTime)
	}
	end := findOverrideProp(t, cal, ical.PropDateTimeEnd)
	if end == nil || end.Value != "20260329" {
		t.Fatalf("DTEND = %+v, want DATE 20260329", end)
	}
}

func TestOccurrenceTimedWithoutEndKeepsZeroDuration(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	cal, master := newRecurringCalendarWithDuration(t, "timed-no-end", "", start)
	title := "Renamed"
	recID := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	if err := applyOccurrenceUpdate(cal, master, recID, &EventUpdate{Title: &title}); err != nil {
		t.Fatalf("applyOccurrenceUpdate: %v", err)
	}
	_, overrides := decodeEvents(t, cal)
	if len(overrides) != 1 {
		t.Fatalf("overrides = %d", len(overrides))
	}
	got := overrides[0]
	if !got.StartTime.Equal(recID) {
		t.Errorf("start = %v, want %v", got.StartTime, recID)
	}
	if !got.EndTime.Equal(got.StartTime) {
		t.Errorf("end = %v, want zero duration", got.EndTime)
	}
	if findOverrideProp(t, cal, ical.PropDateTimeEnd) != nil || findOverrideProp(t, cal, ical.PropDuration) != nil {
		t.Fatal("zero-duration override gained an end or duration")
	}
}

func TestUpdateEventOccurrenceDurationUsesConditionalPut(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	cal, _ := newRecurringCalendarWithDuration(t, "uid-dst-duration", "P1D",
		time.Date(2026, 3, 28, 10, 0, 0, 0, paris))
	var body strings.Builder
	if err := ical.NewEncoder(&body).Encode(cal); err != nil {
		t.Fatal(err)
	}
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-dst-duration.ics"
	m.objects["uid-dst-duration"] = mockObject{path: path, ics: body.String()}
	m.etags[path] = `"v1"`

	title := "Renamed by protocol"
	recID := time.Date(2026, 3, 28, 10, 0, 0, 0, paris)
	if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-dst-duration", &EventUpdate{
		Title:        &title,
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
	}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	m.mu.Lock()
	if len(m.puts) != 1 {
		m.mu.Unlock()
		t.Fatalf("puts = %d", len(m.puts))
	}
	put := m.puts[0]
	m.mu.Unlock()
	if put.ifMatch != `"v1"` {
		t.Errorf("If-Match = %q, want strong ETag", put.ifMatch)
	}
	if !strings.Contains(put.body, "DURATION:P1D") {
		t.Fatalf("PUT lost nominal DURATION:\n%s", put.body)
	}
	decoded, err := ical.NewDecoder(strings.NewReader(put.body)).Decode()
	if err != nil {
		t.Fatalf("decode PUT: %v", err)
	}
	var found bool
	for _, child := range decoded.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		prop := child.Props.Get(ical.PropRecurrenceID)
		if prop == nil {
			continue
		}
		vevent := ical.NewEvent()
		vevent.Component = child
		ev, isOverride, _, err := parseVEvent(vevent, path)
		if err != nil {
			t.Fatal(err)
		}
		if !isOverride {
			continue
		}
		found = true
		if !ev.StartTime.Equal(recID) {
			t.Errorf("override start = %v, want %v", ev.StartTime, recID)
		}
		if !ev.EndTime.Equal(time.Date(2026, 3, 29, 10, 0, 0, 0, paris)) {
			t.Errorf("override end = %v, want next civil day 10:00", ev.EndTime)
		}
	}
	if !found {
		t.Fatal("PUT contains no override")
	}
}

func findOverrideProp(t *testing.T, cal *ical.Calendar, name string) *ical.Prop {
	t.Helper()
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent || child.Props.Get(ical.PropRecurrenceID) == nil {
			continue
		}
		return child.Props.Get(name)
	}
	return nil
}

func ptrTime(t time.Time) *time.Time { return &t }
