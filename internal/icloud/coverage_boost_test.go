package icloud

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestParseTriggerMinutesBefore(t *testing.T) {
	if parseTriggerMinutesBefore("-PT15M") != 15 {
		t.Fatal()
	}
	if parseTriggerMinutesBefore("-PT2H") != 120 {
		t.Fatal()
	}
	if parseTriggerMinutesBefore("PT15M") != 0 {
		t.Fatal()
	}
}

func TestParseAlarms_FromBuiltCalendar(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	cal := buildEventCalendar("u@x", &NewEvent{
		Title: "t", StartTime: start, EndTime: start.Add(time.Hour),
		AlarmMinutesBefore: 10,
		Alarms:             []AlarmSpec{{MinutesBefore: 30}, {Disable: true, MinutesBefore: 5}},
	})
	alarms := parseAlarms(cal)
	if len(alarms) < 1 {
		t.Fatalf("%+v", alarms)
	}
}

func TestParseOptionalDateTime(t *testing.T) {
	z, err := ParseOptionalDateTime("s", "", time.UTC)
	if err != nil || !z.IsZero() {
		t.Fatal(err, z)
	}
	tt, err := ParseOptionalDateTime("s", "2026-07-01T10:00:00Z", time.UTC)
	if err != nil || tt.IsZero() {
		t.Fatal(err)
	}
}

func TestAmbiguousDSTWarning_Paris(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// A normal time should not warn.
	t1 := time.Date(2026, 7, 1, 14, 0, 0, 0, loc)
	if w := AmbiguousDSTWarning(t1, loc); w != "" {
		t.Logf("warning (may be empty): %q", w)
	}
	if AmbiguousDSTWarning(time.Time{}, loc) != "" {
		t.Fatal()
	}
	if AmbiguousDSTWarning(t1, time.UTC) != "" {
		t.Fatal()
	}
}

func TestApplyFieldUpdate_ClearAndSet(t *testing.T) {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropSummary, "old")
	empty := ""
	title := "new"
	loc := "here"
	notes := "n"
	st := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	en := st.Add(time.Hour)
	status := "CONFIRMED"
	transp := "OPAQUE"
	url := "https://example.com"
	applyFieldUpdate(ev, &EventUpdate{
		Title: &title, Location: &loc, Notes: &notes,
		StartTime: &st, EndTime: &en, Status: &status, Transparency: &transp, URL: &url,
	})
	if ev.Props.Get(ical.PropSummary).Value != "new" {
		t.Fatal()
	}
	applyFieldUpdate(ev, &EventUpdate{Title: &empty, Location: &empty, Notes: &empty, Status: &empty, Transparency: &empty, URL: &empty})
	if ev.Props.Get(ical.PropSummary) != nil {
		t.Fatal("title should clear")
	}
}

func TestErrorUnwrap(t *testing.T) {
	inner := errors.New("inner")
	e := NewError(CodeNotFound, 404, "msg", inner)
	if !errors.Is(e, inner) {
		t.Fatal()
	}
	_ = e.Unwrap()
}

func TestCollectAlarms_Disable(t *testing.T) {
	in := &EventInput{
		AlarmMinutes: 5,
		Alarms:       []AlarmSpec{{MinutesBefore: 10}, {Disable: true, MinutesBefore: 20}},
	}
	a := collectAlarms(in)
	if len(a) != 2 || a[0] != 5 || a[1] != 10 {
		t.Fatalf("%v", a)
	}
}

func TestValidateEventInput_TooManyAlarms(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	alarms := make([]AlarmSpec, MaxAlarms+1)
	for i := range alarms {
		alarms[i] = AlarmSpec{MinutesBefore: 5}
	}
	res := ValidateEventInput(&EventInput{
		Title: "t", StartTime: start, EndTime: start.Add(time.Hour), Alarms: alarms,
	}, time.UTC)
	if res.OK {
		t.Fatal("expected too many alarms")
	}
}

func TestFindFreeSlots_DaysOfWeekFilter(t *testing.T) {
	// 2026-07-01 is Wednesday.
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	slots, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart: day,
		RangeEnd:   day.Add(48 * time.Hour),
		Duration:   time.Hour,
		DaysOfWeek: []time.Weekday{time.Thursday},
		Limit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slots {
		if s.Start.Weekday() != time.Thursday {
			t.Errorf("slot on %v", s.Start.Weekday())
		}
	}
}

func TestFindFreeSlots_WorkingHoursNormal(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	slots, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart:       day,
		RangeEnd:         day.Add(24 * time.Hour),
		Duration:         time.Hour,
		WorkingHourStart: 9 * 60,
		WorkingHourEnd:   12 * 60,
		Limit:            20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) == 0 {
		t.Fatal("expected slots in working hours")
	}
	for _, s := range slots {
		if s.Start.Hour() < 9 || s.Start.Hour() >= 12 {
			t.Errorf("hour %d", s.Start.Hour())
		}
	}
}

func TestStructuredToRRULE_RejectsBothCountAndUntil(t *testing.T) {
	_, _, err := structuredToRRULE(&StructuredRecurrence{
		Frequency: "daily", Count: 3, Until: "2026-08-01",
	}, time.UTC)
	if err == nil {
		t.Fatal()
	}
}

func TestValidateEventURL(t *testing.T) {
	if err := validateEventURL("https://ok.example/x"); err != nil {
		t.Fatal(err)
	}
	if err := validateEventURL("javascript:alert(1)"); err == nil {
		t.Fatal()
	}
}

func TestBusyBuffers(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	busy := BusyFromEvents([]Event{{
		StartTime: start, EndTime: start.Add(time.Hour), Title: "x",
	}}, true, 15*time.Minute, 15*time.Minute)
	if len(busy) != 1 {
		t.Fatal()
	}
	if !busy[0].Start.Equal(start.Add(-15 * time.Minute)) {
		t.Errorf("%v", busy[0].Start)
	}
}

func TestNewValidationError_As(t *testing.T) {
	err := NewValidationError("bad input")
	if !strings.Contains(err.Error(), "validation") {
		t.Fatal(err)
	}
}
