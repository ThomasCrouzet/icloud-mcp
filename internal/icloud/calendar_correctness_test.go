package icloud

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestHighFrequencyUnreachableSelectorsFailBeforeIteration(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []string{
		"FREQ=HOURLY;INTERVAL=2;BYHOUR=1;COUNT=1",
		"FREQ=MINUTELY;INTERVAL=60;BYMINUTE=30;COUNT=1",
		"FREQ=SECONDLY;INTERVAL=60;BYSECOND=30;COUNT=1",
	}
	for _, rule := range tests {
		t.Run(rule, func(t *testing.T) {
			master := Event{
				UID: "unreachable", StartTime: start, EndTime: start.Add(time.Minute), Recurrence: rule,
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := ExpandOccurrences(master, nil, start, start.Add(24*time.Hour), 10)
				done <- err
			}()
			select {
			case err := <-done:
				requireICloudCode(t, err, CodeProtocolError)
			case <-time.After(2 * time.Second):
				t.Fatal("recurrence validation entered a non-returning iterator call")
			}
		})
	}

	if err := validateRRULEForStart("FREQ=HOURLY;INTERVAL=2;BYHOUR=1;COUNT=1", start.Add(time.Hour)); err != nil {
		t.Fatalf("reachable selector rejected: %v", err)
	}
}

func TestCalendarSelectorsFailBeforeUnboundedIteratorScan(t *testing.T) {
	start := time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC) // Thursday.
	tests := []string{
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30;COUNT=1",
		"FREQ=YEARLY;INTERVAL=4;BYMONTH=2;BYMONTHDAY=29;COUNT=1",
		"FREQ=MONTHLY;INTERVAL=12;BYMONTH=2;COUNT=1",
		"FREQ=DAILY;INTERVAL=7;BYDAY=FR;COUNT=1",
		"FREQ=DAILY;BYHOUR=9;BYSETPOS=2;COUNT=1",
		"FREQ=MONTHLY;BYDAY=6MO;COUNT=1",
		"FREQ=YEARLY;BYMONTH=2;BYYEARDAY=366;COUNT=1",
		"FREQ=YEARLY;BYMONTH=2;BYWEEKNO=53;COUNT=1",
		"FREQ=HOURLY;BYMONTH=2;BYMONTHDAY=30;COUNT=1",
		"FREQ=YEARLY;BYEASTER=0;COUNT=1",
		"FREQ=YEARLY;BYEASTER=1000;COUNT=1",
	}
	for _, recurrence := range tests {
		t.Run(recurrence, func(t *testing.T) {
			master := Event{UID: "unsafe-date-selector", StartTime: start, EndTime: start.Add(time.Hour), Recurrence: recurrence}
			done := make(chan error, 1)
			go func() {
				_, _, err := ExpandOccurrences(master, nil, start, start.Add(24*time.Hour), 10)
				done <- err
			}()
			select {
			case err := <-done:
				if code := AsICloudError(err); code == nil || code.Code != CodeProtocolError {
					t.Fatalf("error = %v, want protocol_error", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("recurrence validation entered a non-returning iterator call")
			}
		})
	}
}

func TestCalendarSelectorPreflightAcceptsReachableRules(t *testing.T) {
	start := time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC) // Thursday.
	tests := []struct {
		start time.Time
		rule  string
	}{
		{start: time.Date(2025, time.January, 1, 9, 0, 0, 0, time.UTC), rule: "FREQ=YEARLY;INTERVAL=3;BYMONTH=2;BYMONTHDAY=29;COUNT=1"},
		{start: time.Date(2000, time.February, 29, 9, 0, 0, 0, time.UTC), rule: "FREQ=YEARLY;INTERVAL=100;COUNT=2"},
		{start: time.Date(2026, time.January, 31, 9, 0, 0, 0, time.UTC), rule: "FREQ=MONTHLY;INTERVAL=5;COUNT=2"},
		{start: start, rule: "FREQ=DAILY;INTERVAL=7;BYDAY=TH;COUNT=2"},
		{start: start, rule: "FREQ=DAILY;BYMONTHDAY=1;COUNT=2"},
		{start: start, rule: "FREQ=WEEKLY;INTERVAL=2;BYDAY=FR;COUNT=2"},
		{start: start, rule: "FREQ=YEARLY;BYMONTH=2;BYYEARDAY=60;COUNT=2"},
		{start: start, rule: "FREQ=DAILY;BYHOUR=9,10;BYSETPOS=2;COUNT=2"},
		{start: start, rule: "FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=2"},
	}
	for _, test := range tests {
		t.Run(test.rule, func(t *testing.T) {
			if err := validateRRULEForStart(test.rule, test.start); err != nil {
				t.Fatalf("reachable rule rejected: %v", err)
			}
		})
	}
}

func TestRecurrenceSelectorProductsAreBoundedBeforeIteration(t *testing.T) {
	start := time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC)
	adversarial := "FREQ=YEARLY;BYDAY=MO;" +
		"BYHOUR=0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23;" +
		"BYMINUTE=0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,59;" +
		"BYSECOND=0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27"
	master := Event{UID: "selector-product", StartTime: start, EndTime: start.Add(time.Hour), Recurrence: adversarial}
	began := time.Now()
	_, _, err := ExpandOccurrences(master, nil, start, start.AddDate(1, 0, 0), 10)
	requireICloudCode(t, err, CodePayloadTooLarge)
	if elapsed := time.Since(began); elapsed > 500*time.Millisecond {
		t.Fatalf("selector product preflight took %v", elapsed)
	}

	duplicates := strings.Repeat("0,", 100) + "0"
	duplicateRule := "FREQ=DAILY;COUNT=1;BYHOUR=" + duplicates + ";BYMINUTE=" + duplicates + ";BYSECOND=" + duplicates
	if err := validateRRULEForStart(duplicateRule, start); err != nil {
		t.Fatalf("semantically duplicate selectors were not safely normalized: %v", err)
	}
	master.Recurrence = duplicateRule
	out, _, err := ExpandOccurrences(master, nil, start, start.AddDate(0, 0, 1), 10)
	if err != nil || len(out) != 1 {
		t.Fatalf("duplicate selector expansion out=%d err=%v", len(out), err)
	}
}

func TestRecurrenceWritePreflightHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC)
	err := validateRRULEForStartContext(ctx, "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=2", start)
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled selector preflight error = %v", err)
	}
}

func TestRawICalendarPreflightBudgets(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "component depth",
			body: "BEGIN:VCALENDAR\r\n" + strings.Repeat("BEGIN:X-NESTED\r\n", maxRemoteComponentDepth) +
				strings.Repeat("END:X-NESTED\r\n", maxRemoteComponentDepth) + "END:VCALENDAR\r\n",
		},
		{
			name: "component count",
			body: "BEGIN:VCALENDAR\r\n" + strings.Repeat("BEGIN:X-ITEM\r\nEND:X-ITEM\r\n", maxRemoteComponents) +
				"END:VCALENDAR\r\n",
		},
		{
			name: "property count",
			body: "BEGIN:VCALENDAR\r\n" + strings.Repeat("X-PROP:value\r\n", maxRemotePropertiesPerComponent+1) +
				"END:VCALENDAR\r\n",
		},
		{
			name: "logical line count",
			body: "BEGIN:VCALENDAR\r\n" + strings.Repeat("\r\n", maxRemoteLogicalLines) + "END:VCALENDAR\r\n",
		},
		{
			name: "logical line bytes",
			body: "BEGIN:VCALENDAR\r\nX-LONG:" + strings.Repeat("x", maxRemoteLogicalLineBytes) +
				"\r\nEND:VCALENDAR\r\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeRemoteCalendar(strings.NewReader(test.body))
			requireICloudCode(t, err, CodePayloadTooLarge)
		})
	}
}

func TestVTimezoneUsesPrecisePreTransitionWallTimes(t *testing.T) {
	tests := []struct {
		zone     string
		event    time.Time
		dayStart string
		dayFrom  string
		dayTo    string
		stdStart string
		stdFrom  string
		stdTo    string
		dayRule  string
		stdRule  string
	}{
		{
			zone:     "Europe/Paris",
			event:    time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			dayStart: "20260329T020000", dayFrom: "+0100", dayTo: "+0200",
			stdStart: "20251026T030000", stdFrom: "+0200", stdTo: "+0100",
			dayRule: "BYMONTH=3;BYDAY=-1SU", stdRule: "BYMONTH=10;BYDAY=-1SU",
		},
		{
			zone:     "Australia/Lord_Howe",
			event:    time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			dayStart: "20251005T020000", dayFrom: "+1030", dayTo: "+1100",
			stdStart: "20250406T020000", stdFrom: "+1100", stdTo: "+1030",
			dayRule: "BYMONTH=10;BYDAY=1SU", stdRule: "BYMONTH=4;BYDAY=1SU",
		},
	}

	for _, test := range tests {
		t.Run(test.zone, func(t *testing.T) {
			loc, err := time.LoadLocation(test.zone)
			if err != nil {
				t.Fatal(err)
			}
			vtz := buildVTimezone(loc, test.event.In(loc), test.event.Add(time.Hour).In(loc))
			if vtz == nil {
				t.Fatal("missing VTIMEZONE")
			}
			observances := make(map[string]*ical.Component)
			for _, child := range vtz.Children {
				observances[child.Name] = child
			}
			assertTimezoneObservance(t, observances[ical.CompTimezoneDaylight], test.dayStart, test.dayFrom, test.dayTo, test.event)
			assertTimezoneObservance(t, observances[ical.CompTimezoneStandard], test.stdStart, test.stdFrom, test.stdTo, test.event)
			if got := observances[ical.CompTimezoneDaylight].Props.Get(ical.PropRecurrenceRule).Value; !strings.Contains(got, test.dayRule) {
				t.Fatalf("DAYLIGHT RRULE = %q, want %q", got, test.dayRule)
			}
			if got := observances[ical.CompTimezoneStandard].Props.Get(ical.PropRecurrenceRule).Value; !strings.Contains(got, test.stdRule) {
				t.Fatalf("STANDARD RRULE = %q, want %q", got, test.stdRule)
			}
		})
	}
}

func TestRecurringTimezoneRejectsMovingAnnualTransitions(t *testing.T) {
	casablanca, err := time.LoadLocation("Africa/Casablanca")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 15, 10, 0, 0, 0, casablanca)
	if recurringTimezoneStable(casablanca, start) {
		t.Fatal("moving Morocco transitions were treated as a stable annual rule")
	}
	result := ValidateEventInput(&EventInput{
		Title: "Recurring", StartTime: start, EndTime: start.Add(time.Hour),
		Timezone: "Africa/Casablanca", Recurrence: "FREQ=WEEKLY;COUNT=10",
	}, time.UTC)
	if result.OK || !strings.Contains(strings.Join(result.Errors, " "), "stable annual") {
		t.Fatalf("validation result = %+v", result)
	}
}

func assertTimezoneObservance(t *testing.T, component *ical.Component, start, from, to string, event time.Time) {
	t.Helper()
	if component == nil {
		t.Fatal("missing timezone observance")
	}
	dtstart := component.Props.Get(ical.PropDateTimeStart)
	if dtstart == nil || dtstart.Value != start {
		t.Fatalf("DTSTART = %+v, want %s", dtstart, start)
	}
	if strings.HasSuffix(dtstart.Value, "Z") || dtstart.Params.Get(ical.PropTimezoneID) != "" {
		t.Fatalf("DTSTART is not floating: %+v", dtstart)
	}
	if got := component.Props.Get(ical.PropTimezoneOffsetFrom); got == nil || got.Value != from {
		t.Fatalf("TZOFFSETFROM = %+v, want %s", got, from)
	}
	if got := component.Props.Get(ical.PropTimezoneOffsetTo); got == nil || got.Value != to {
		t.Fatalf("TZOFFSETTO = %+v, want %s", got, to)
	}
	parsed, err := time.Parse("20060102T150405", dtstart.Value)
	if err != nil || !parsed.Before(event) {
		t.Fatalf("observance DTSTART %q does not apply before event %v", dtstart.Value, event)
	}
}

func TestBuildAllDayRecurrenceUsesDateValues(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	rule, exdates, err := structuredToRRULE(&StructuredRecurrence{
		Frequency: "daily", Until: "2026-07-31", Exceptions: []string{"2026-07-10T00:00:00"},
	}, paris)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	calendar := buildEventCalendar("all-day", &NewEvent{
		Title: "All day", StartTime: start, EndTime: start.Add(24 * time.Hour), AllDay: true,
		Recurrence: rule, ExDates: exdates,
	})
	master, err := findMasterVEvent(calendar)
	if err != nil {
		t.Fatal(err)
	}
	if got := master.Props.Get(ical.PropRecurrenceRule).Value; !strings.Contains(got, "UNTIL=20260731") || strings.Contains(got, "UNTIL=20260731T") {
		t.Fatalf("all-day RRULE = %q", got)
	}
	exdate := master.Props.Get(ical.PropExceptionDates)
	if exdate == nil || exdate.Value != "20260710" || !isDateOnlyProp(exdate) {
		t.Fatalf("all-day EXDATE = %+v", exdate)
	}
}

func TestMovedOverrideOutsideRangeIsNotReturned(t *testing.T) {
	master := Event{
		UID: "moved", StartTime: mustParse(t, "2026-07-01T10:00:00Z"),
		EndTime: mustParse(t, "2026-07-01T11:00:00Z"), Recurrence: "FREQ=DAILY;COUNT=3",
	}
	override := Event{
		UID: "moved", RecurrenceID: mustParse(t, "2026-07-02T10:00:00Z"),
		StartTime: mustParse(t, "2026-07-10T10:00:00Z"), EndTime: mustParse(t, "2026-07-10T11:00:00Z"),
	}
	events, _, err := ExpandOccurrences(master, []Event{override}, mustParse(t, "2026-07-02T00:00:00Z"), mustParse(t, "2026-07-03T00:00:00Z"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("moved override outside range was returned: %+v", events)
	}
}

func TestUpdateEndExcludesDurationAndStartOnlyPreservesOverrideDuration(t *testing.T) {
	t.Run("explicit end replaces duration", func(t *testing.T) {
		event := ical.NewEvent()
		event.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
		duration := ical.NewProp(ical.PropDuration)
		duration.Value = "PT2H"
		event.Props.Set(duration)
		end := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
		if err := applyFieldUpdate(event, &EventUpdate{EndTime: &end}); err != nil {
			t.Fatal(err)
		}
		if event.Props.Get(ical.PropDuration) != nil || event.Props.Get(ical.PropDateTimeEnd) == nil {
			t.Fatalf("end update left conflicting properties: %+v", event.Props)
		}
	})

	t.Run("duration-only override keeps own duration", func(t *testing.T) {
		calendar := ical.NewCalendar()
		master := ical.NewEvent()
		master.Props.SetText(ical.PropUID, "duration-override")
		master.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
		master.Props.SetDateTime(ical.PropDateTimeEnd, time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC))
		rule := ical.NewProp(ical.PropRecurrenceRule)
		rule.Value = "FREQ=DAILY;COUNT=3"
		master.Props.Set(rule)
		override := ical.NewEvent()
		override.Props.SetText(ical.PropUID, "duration-override")
		override.Props.SetDateTime(ical.PropRecurrenceID, time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC))
		override.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC))
		duration := ical.NewProp(ical.PropDuration)
		duration.Value = "PT2H"
		override.Props.Set(duration)
		calendar.Children = append(calendar.Children, master.Component, override.Component)

		newStart := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
		if err := applyOccurrenceUpdate(calendar, master, time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC), &EventUpdate{StartTime: &newStart}); err != nil {
			t.Fatal(err)
		}
		if override.Props.Get(ical.PropDateTimeEnd) != nil {
			t.Fatalf("start-only update added DTEND: %+v", override.Props)
		}
		if got := override.Props.Get(ical.PropDuration); got == nil || got.Value != "PT2H" {
			t.Fatalf("override duration = %+v, want PT2H", got)
		}
	})
}

func TestOccurrenceDeleteDryRunRequiresRecurringMaster(t *testing.T) {
	mock := newMockCalDAV(t)
	mock.objects["uid-simple-1"] = mockObject{path: testHomeCalendar + "uid-simple-1.ics", ics: icsSimpleEvent}
	recurrenceID := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err := mock.client().DeleteEvent(context.Background(), testHomeCalendar, "uid-simple-1", &DeleteOptions{
		Scope: ScopeOccurrence, RecurrenceID: &recurrenceID, DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "RRULE") {
		t.Fatalf("dry-run occurrence delete error = %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.puts) != 0 || len(mock.deletes) != 0 {
		t.Fatalf("dry run mutated: puts=%d deletes=%d", len(mock.puts), len(mock.deletes))
	}
}

func TestStrictEventDurationValidation(t *testing.T) {
	for _, value := range []string{"P1W", "P1D", "PT1H", "P1DT2H3M4S", "+PT1H", "-PT15M"} {
		if _, err := parseICalDuration(value); err != nil {
			t.Errorf("valid duration %q: %v", value, err)
		}
	}
	for _, value := range []string{"pt1h", " PT1H", "PT1H ", "PT", "P1DT", "P1H", "P1D2D", "PT1S1M", "P1WT1H"} {
		if _, err := parseICalDuration(value); err == nil {
			t.Errorf("invalid duration %q was accepted", value)
		}
	}

	for _, test := range []struct {
		name     string
		duration string
		withEnd  bool
		allDay   bool
	}{
		{name: "zero", duration: "PT0S"},
		{name: "negative", duration: "-PT1H"},
		{name: "both end and duration", duration: "PT1H", withEnd: true},
		{name: "all-day time duration", duration: "PT24H", allDay: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calendar := ical.NewCalendar()
			event := ical.NewEvent()
			event.Props.SetText(ical.PropUID, "duration")
			start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			if test.allDay {
				event.Props.SetDate(ical.PropDateTimeStart, start)
			} else {
				event.Props.SetDateTime(ical.PropDateTimeStart, start)
			}
			duration := ical.NewProp(ical.PropDuration)
			duration.Value = test.duration
			event.Props.Set(duration)
			if test.withEnd {
				event.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
			}
			calendar.Children = append(calendar.Children, event.Component)
			requireICloudCode(t, validateRemoteCalendar(calendar), CodeProtocolError)
		})
	}
}

func TestUnsupportedRecurrenceFormsFailClosed(t *testing.T) {
	for _, property := range []string{
		"RDATE:20260703T100000Z\r\n",
		"RECURRENCE-ID;RANGE=THISANDFUTURE:20260702T100000Z\r\n",
	} {
		body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:unsupported\r\n" +
			"DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\nRRULE:FREQ=DAILY;COUNT=3\r\n" +
			property + "END:VEVENT\r\nEND:VCALENDAR\r\n"
		calendar, err := decodeRemoteCalendar(strings.NewReader(body))
		if err == nil {
			err = validateRemoteCalendar(calendar)
		}
		requireICloudCode(t, err, CodeProtocolError)
	}
}

func TestDAVStatusParsingIsExactAndPropfindIsPreflighted(t *testing.T) {
	if !isOKStatus("HTTP/1.1 200 OK") {
		t.Fatal("valid DAV status was rejected")
	}
	for _, status := range []string{
		"garbage 200 OK",
		"HTTP/1.1 200",
		"HTTP/1.1 2000 OK",
		"HTTP/1.1 0200 OK",
		"HTTP/1.1 200 OK\nHTTP/1.1 404 Not Found",
	} {
		if isOKStatus(status) {
			t.Errorf("malformed DAV status accepted: %q", status)
		}
	}

	body := `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:test">` +
		strings.Repeat(`<X:n>`, maxReportXMLDepth) + strings.Repeat(`</X:n>`, maxReportXMLDepth) +
		`</D:multistatus>`
	client := initializedSecurityClient(calendarDoerFunc(func(req *http.Request) (*http.Response, error) {
		return calendarTestResponse(req, http.StatusMultiStatus, nil, strings.NewReader(body)), nil
	}))
	_, err := client.propfind(context.Background(), "https://caldav.icloud.com/home/", "0", propfindPrincipalBody, "/home/")
	requireICloudCode(t, err, CodePayloadTooLarge)
}

func TestCancellationAndBackoffReturnTypedTimeouts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sleep(ctx, time.Hour)
	requireICloudCode(t, err, CodeTimeout)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep error lost cancellation cause: %v", err)
	}

	guarded := NewGuardedService(&countingService{}, 0, time.Millisecond)
	err = guarded.retry(context.Background(), "read", func() error {
		return context.DeadlineExceeded
	})
	requireICloudCode(t, err, CodeTimeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry error lost deadline cause: %v", err)
	}
}

func TestAggregateRecurrenceWorkBudget(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	remaining := int64(1)
	master := Event{
		UID: "budget", StartTime: start, EndTime: start.Add(time.Minute),
		Recurrence: "FREQ=MINUTELY;COUNT=3",
	}
	_, _, err := expandOccurrencesContext(context.Background(), master, nil, start, start.Add(time.Hour), 10, &remaining)
	requireICloudCode(t, err, CodePayloadTooLarge)

	remaining = 1
	master.Recurrence = "FREQ=DAILY;BYDAY=MO;COUNT=3"
	_, _, err = expandOccurrencesContext(context.Background(), master, nil, start, start.AddDate(0, 1, 0), 10, &remaining)
	requireICloudCode(t, err, CodePayloadTooLarge)
}
