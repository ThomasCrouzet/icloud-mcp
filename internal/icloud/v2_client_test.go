package icloud

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestClient_SearchEvents_ExpandRecurrenceFalse(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-recur-1.ics"
	m.objects["uid-recur-1"] = mockObject{path: path, ics: icsRecurringWithExdate}
	c := m.client()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(40 * 24 * time.Hour)

	expanded, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end, &SearchOptions{ExpandRecurrence: true})
	if err != nil {
		t.Fatal(err)
	}
	masters, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end, &SearchOptions{ExpandRecurrence: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(masters.Events) != 1 {
		t.Fatalf("masters=%d want 1", len(masters.Events))
	}
	if masters.Events[0].Recurrence == "" {
		t.Error("master should keep RRULE")
	}
	if len(expanded.Events) <= len(masters.Events) {
		t.Fatalf("expanded=%d should exceed masters=%d", len(expanded.Events), len(masters.Events))
	}
}

func TestClient_GetEvent_ByUID(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	m.etags[path] = `"etag-1"`
	c := m.client()

	d, err := c.GetEvent(context.Background(), testHomeCalendar, "uid-simple-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.UID != "uid-simple-1" || d.Title != "Team meeting" {
		t.Fatalf("%+v", d)
	}
	if d.Path != "" {
		t.Errorf("path must be cleared: %q", d.Path)
	}
	if d.ETag == "" {
		t.Error("expected etag")
	}
}

func TestClient_GetEvent_NotFound(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()
	_, err := c.GetEvent(context.Background(), testHomeCalendar, "missing-uid")
	if err == nil {
		t.Fatal("expected not found")
	}
	if AsICloudError(err) == nil || AsICloudError(err).Code != CodeNotFound {
		t.Errorf("want CodeNotFound, got %v", err)
	}
}

func TestClient_DeleteEvent_DryRunNoDELETE(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	c := m.client()

	res, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-simple-1", &DeleteOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Title != "Team meeting" {
		t.Fatalf("%+v", res)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deletes) != 0 || len(m.puts) != 0 {
		t.Fatalf("dry_run must not mutate: deletes=%v puts=%d", m.deletes, len(m.puts))
	}
}

func TestClient_DeleteEvent_IfMatch412(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	m.etags[path] = `"current"`
	c := m.client()

	_, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-simple-1", &DeleteOptions{
		IfMatchETag: `"stale"`,
	})
	if err == nil {
		t.Fatal("expected 412")
	}
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeConcurrentModification {
		t.Errorf("want concurrent_modification, got %v", err)
	}
}

func TestClient_DeleteEvent_OccurrenceAddsEXDATE(t *testing.T) {
	m := newMockCalDAV(t)
	// icsRecurringWithExdate uses UID uid-recur-1.
	path := testHomeCalendar + "uid-recur-1.ics"
	m.objects["uid-recur-1"] = mockObject{path: path, ics: icsRecurringWithExdate}
	m.etags[path] = `"e1"`
	c := m.client()

	recID := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	res, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-1", &DeleteOptions{
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
		IfMatchETag:  `"e1"`,
	})
	if err != nil {
		t.Fatalf("delete occurrence: %v", err)
	}
	if res.Scope != string(ScopeOccurrence) {
		t.Errorf("scope=%q", res.Scope)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deletes) != 0 {
		t.Errorf("occurrence delete must not DELETE the series resource: %v", m.deletes)
	}
	if len(m.puts) != 1 {
		t.Fatalf("want one PUT with EXDATE, got %d", len(m.puts))
	}
	if !strings.Contains(m.puts[0].body, "EXDATE") {
		t.Errorf("expected EXDATE in PUT:\n%s", m.puts[0].body)
	}
}

func TestClient_CreateEvent_ClientUIDConflict(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "client-uid-1.ics"
	m.objects["client-uid-1"] = mockObject{path: path, ics: strings.Replace(icsSimpleEvent, "UID:uid-simple-1", "UID:client-uid-1", 1)}
	c := m.client()

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err := c.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title: "x", StartTime: start, EndTime: start.Add(time.Hour),
		ClientUID: "client-uid-1",
	})
	if err == nil {
		t.Fatal("expected conflict")
	}
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeConflict {
		t.Errorf("want conflict, got %v", err)
	}
}

// TestClient_CreateEvent_IfNoneMatchConflict covers the race where the
// pre-PUT GET misses an existing object but If-None-Match: * still blocks
// overwrite (412 -> conflict).
func TestClient_CreateEvent_IfNoneMatchConflict(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "race-uid.ics"
	// Path known only via etags (no GET body): simulates existence for
	// If-None-Match without a successful GetCalendarObject.
	m.etags[path] = `"existing"`
	c := m.client()

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err := c.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title: "x", StartTime: start, EndTime: start.Add(time.Hour),
		ClientUID: "race-uid",
	})
	if err == nil {
		t.Fatal("expected conflict from If-None-Match")
	}
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeConflict {
		t.Errorf("want conflict, got %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 || m.puts[0].ifNoneMatch != "*" {
		t.Fatalf("expected one PUT with If-None-Match=*, got %+v", m.puts)
	}
}

func TestClient_CreateEvent_EnrichedFields(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	uid, err := c.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title: "Enriched", StartTime: start, EndTime: start.Add(time.Hour),
		Status: "CONFIRMED", Transparency: "OPAQUE",
		URL: "https://example.com/meet", AlarmMinutesBefore: 15,
		Alarms: []AlarmSpec{{MinutesBefore: 60}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if uid == "" {
		t.Fatal("empty uid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts=%d", len(m.puts))
	}
	body := m.puts[0].body
	for _, want := range []string{"STATUS:CONFIRMED", "TRANSP:OPAQUE", "example.com/meet", "BEGIN:VALARM"} {
		if !strings.Contains(body, want) {
			t.Errorf("PUT body missing %q:\n%s", want, body)
		}
	}
}

func TestClient_UpdateEvent_OccurrenceScope(t *testing.T) {
	m := newMockCalDAV(t)
	// Recurring master required for scope=occurrence.
	path := testHomeCalendar + "uid-recur-1.ics"
	m.objects["uid-recur-1"] = mockObject{path: path, ics: icsRecurringWithExdate}
	m.etags[path] = `"e1"`
	c := m.client()
	recID := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	newTitle := "Only this day"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-recur-1", &EventUpdate{
		Title:        &newTitle,
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
		IfMatchETag:  `"e1"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts=%d", len(m.puts))
	}
	if !strings.Contains(m.puts[0].body, "RECURRENCE-ID") {
		t.Errorf("expected RECURRENCE-ID override in PUT:\n%s", m.puts[0].body)
	}
	if !strings.Contains(m.puts[0].body, "Only this day") {
		t.Errorf("expected override title in PUT")
	}
}

func TestClient_UpdateEvent_InvalidStatusTransparencyURL_NoPUT(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	m.etags[path] = `"e1"`
	c := m.client()

	badStatus := "MAYBE"
	badTransp := "BUSY"
	badURL := "ftp://evil.example/x"
	for _, tc := range []struct {
		name string
		up   *EventUpdate
	}{
		{"status", &EventUpdate{Status: &badStatus}},
		{"transparency", &EventUpdate{Transparency: &badTransp}},
		{"url", &EventUpdate{URL: &badURL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", tc.up)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 0 {
		t.Fatalf("invalid update must not PUT: puts=%d", len(m.puts))
	}
	// Discovery may not even run, but any GET/PUT for the object is forbidden.
	// Ensure no mutating PUT was recorded (primary invariant).
}

func TestClient_UpdateEvent_ValidStatusURL(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	m.etags[path] = `"e1"`
	c := m.client()
	status := "tentative"
	url := "https://example.com/x"
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{
		Status: &status, URL: &url,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts=%d", len(m.puts))
	}
	if !strings.Contains(m.puts[0].body, "STATUS:TENTATIVE") {
		t.Errorf("PUT missing STATUS:TENTATIVE:\n%s", m.puts[0].body)
	}
	if !strings.Contains(m.puts[0].body, "example.com/x") {
		t.Errorf("PUT missing URL:\n%s", m.puts[0].body)
	}
}

func TestClient_CreateEvent_RejectsBadURL(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, err := c.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title: "x", StartTime: start, EndTime: start.Add(time.Hour),
		URL: "ftp://evil.example/",
	})
	if err == nil {
		t.Fatal("expected URL rejection")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 0 {
		t.Fatalf("bad URL must not PUT: %d", len(m.puts))
	}
}

func TestPublicCodeMapping(t *testing.T) {
	if PublicCode(CodeAuthenticationRefused) != CodeAuthentication {
		t.Error("auth mapping")
	}
	if PublicCode(CodeForbidden) != CodeAuthorization {
		t.Error("authz mapping")
	}
	if PublicCode(CodeServerUnavailable) != CodeUnavailable {
		t.Error("unavailable mapping")
	}
}

func TestFormatEventTime_AllDay(t *testing.T) {
	d := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	if got := FormatEventTime(d, true, time.UTC); got != "2026-07-01" {
		t.Errorf("got %q", got)
	}
	if FormatEventTime(time.Time{}, false, time.UTC) != "" {
		t.Error("zero")
	}
}

func TestFormatEventTime_DefaultTZOffset(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// 18:00 UTC is 20:00 in Paris (CEST, +02:00) on 2026-07-25.
	utc := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	got := FormatEventTime(utc, false, paris)
	if got != "2026-07-25T20:00:00+02:00" {
		t.Fatalf("got %q, want 2026-07-25T20:00:00+02:00", got)
	}
	if strings.HasSuffix(got, "Z") {
		t.Fatalf("timed output must not use bare Z: %q", got)
	}
}

func TestNormalizeAllDayBounds(t *testing.T) {
	s := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	a, b := NormalizeAllDayBounds(s, s)
	if !b.After(a) {
		t.Fatalf("%v %v", a, b)
	}
}

func TestParseWorkingHours(t *testing.T) {
	m, err := ParseWorkingHours("09:30")
	if err != nil || m != 9*60+30 {
		t.Fatalf("%d %v", m, err)
	}
	for _, invalid := range []string{"25:00", "9:30", "09:3", "09:30junk", "09:30:00", "+9:30"} {
		if _, err := ParseWorkingHours(invalid); err == nil {
			t.Errorf("ParseWorkingHours(%q) accepted an invalid value", invalid)
		}
	}
}

func TestGuardedService_GetEvent(t *testing.T) {
	inner := &MockService{Detail: &EventDetail{Event: Event{UID: "u", Title: "t"}}}
	g := NewGuardedService(inner, 0, time.Millisecond)
	d, err := g.GetEvent(context.Background(), "/cal/", "u")
	if err != nil || d.UID != "u" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestGuardedService_DeleteDryRunNotRetried(t *testing.T) {
	inner := &MockService{DeletedTitle: "x"}
	g := NewGuardedService(inner, 2, time.Millisecond)
	res, err := g.DeleteEvent(context.Background(), "/cal/", "u", &DeleteOptions{DryRun: true})
	if err != nil || !res.DryRun {
		t.Fatalf("%+v %v", res, err)
	}
	if inner.DeleteCallCount != 1 {
		t.Errorf("calls=%d", inner.DeleteCallCount)
	}
	if inner.MutationCount() != 0 {
		t.Error("dry run mutated")
	}
}

func TestClassifyStatus_423And409(t *testing.T) {
	e := classifyStatus(423)
	if e.Code != CodeConflict || !e.Retryable {
		t.Errorf("%+v", e)
	}
	e = classifyStatus(409)
	if e.Code != CodeConflict {
		t.Errorf("%+v", e)
	}
}

func TestClient_CreateEvent_ReconcilesImportedClientUID(t *testing.T) {
	m := newMockCalDAV(t)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	request := &NewEvent{
		Title: "Imported meeting", StartTime: start, EndTime: start.Add(time.Hour),
		ClientUID: "client-uid-imported",
	}
	existingICS := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:client-uid-imported\r\nSUMMARY:Imported meeting\r\n" +
		"DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\nDTSTAMP:20260701T000000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	m.objects["client-uid-imported"] = mockObject{path: testHomeCalendar + "imported-name.ics", ics: existingICS}
	client := m.client()

	uid, err := client.CreateEvent(context.Background(), testHomeCalendar, request)
	if err != nil || uid != "client-uid-imported" {
		t.Fatalf("reconciled create = %q, %v", uid, err)
	}
	m.mu.Lock()
	puts := len(m.puts)
	m.mu.Unlock()
	if puts != 0 {
		t.Fatalf("reconciled create issued %d PUTs", puts)
	}

	different := *request
	different.Title = "Different meeting"
	_, err = client.CreateEvent(context.Background(), testHomeCalendar, &different)
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeConflict {
		t.Fatalf("different content = %v, want conflict", err)
	}
	m.mu.Lock()
	puts = len(m.puts)
	m.mu.Unlock()
	if puts != 0 {
		t.Fatalf("conflicting create issued %d PUTs", puts)
	}
}

func TestClient_UpdateEvent_SeriesStartMoveWithAnchorsFailsClosed(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-recur-1"] = mockObject{path: testHomeCalendar + "uid-recur-1.ics", ics: icsRecurringWithExdate}
	client := m.client()
	start := time.Date(2026, 7, 6, 19, 0, 0, 0, time.UTC)
	err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-recur-1", &EventUpdate{StartTime: &start})
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeConflict {
		t.Fatalf("series move = %v, want conflict", err)
	}
	m.mu.Lock()
	puts := len(m.puts)
	m.mu.Unlock()
	if puts != 0 {
		t.Fatalf("unsafe series move issued %d PUTs", puts)
	}

	title := "Renamed only"
	if err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-recur-1", &EventUpdate{Title: &title}); err != nil {
		t.Fatalf("title-only series update = %v", err)
	}
}

func TestClient_UpdateEvent_SeriesStartMoveWithoutAnchorsSucceeds(t *testing.T) {
	m := newMockCalDAV(t)
	series := strings.Replace(icsRecurringWithExdate, "EXDATE:20260713T180000Z\r\n", "", 1)
	m.objects["uid-recur-1"] = mockObject{path: testHomeCalendar + "uid-recur-1.ics", ics: series}
	client := m.client()
	start := time.Date(2026, 7, 6, 17, 0, 0, 0, time.UTC)
	if err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-recur-1", &EventUpdate{StartTime: &start}); err != nil {
		t.Fatalf("series move without anchors = %v", err)
	}
	m.mu.Lock()
	puts := len(m.puts)
	m.mu.Unlock()
	if puts != 1 {
		t.Fatalf("series move issued %d PUTs, want 1", puts)
	}
}

func TestClient_OccurrenceTargetsAreValidated(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-recur-1.ics"
	m.objects["uid-recur-1"] = mockObject{path: path, ics: icsRecurringWithExdate}
	m.etags[path] = `"e1"`
	client := m.client()

	missing := time.Date(2026, 7, 7, 18, 0, 0, 0, time.UTC)
	title := "Phantom"
	err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-recur-1", &EventUpdate{
		Title: &title, Scope: ScopeOccurrence, RecurrenceID: &missing,
	})
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeNotFound {
		t.Fatalf("update nonexistent occurrence = %v", err)
	}
	_, err = client.DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-1", &DeleteOptions{
		Scope: ScopeOccurrence, RecurrenceID: &missing,
	})
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeNotFound {
		t.Fatalf("delete nonexistent occurrence = %v", err)
	}

	excluded := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	result, err := client.DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-1", &DeleteOptions{
		Scope: ScopeOccurrence, RecurrenceID: &excluded,
	})
	if err != nil {
		t.Fatalf("delete excluded occurrence = %v", err)
	}
	if result.WouldMutate || !result.AlreadyExcluded {
		t.Fatalf("excluded delete = %#v", result)
	}
	m.mu.Lock()
	puts := len(m.puts)
	m.mu.Unlock()
	if puts != 0 {
		t.Fatalf("invalid occurrence targets issued %d PUTs", puts)
	}
}

func TestClient_DeleteEvent_DryRunRejectsStaleCallerETag(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-recur-1.ics"
	m.objects["uid-recur-1"] = mockObject{path: path, ics: icsRecurringWithExdate}
	m.etags[path] = `"v2"`
	client := m.client()

	series := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	for _, scope := range []MutationScope{ScopeSeries, ScopeOccurrence} {
		opts := &DeleteOptions{Scope: scope, DryRun: true, IfMatchETag: `"v1"`}
		if scope == ScopeOccurrence {
			opts.RecurrenceID = &series
		}
		_, err := client.DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-1", opts)
		if ie := AsICloudError(err); ie == nil || ie.Code != CodeConcurrentModification {
			t.Fatalf("%s dry run with stale ETag = %v", scope, err)
		}
	}

	matching := &DeleteOptions{Scope: ScopeSeries, DryRun: true, IfMatchETag: `"v2"`}
	result, err := client.DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-1", matching)
	if err != nil || !result.DryRun || !result.WouldMutate {
		t.Fatalf("matching dry run = %#v, %v", result, err)
	}
	m.mu.Lock()
	deletes := len(m.deletes)
	puts := len(m.puts)
	m.mu.Unlock()
	if deletes != 0 || puts != 0 {
		t.Fatalf("dry run mutated: deletes=%d puts=%d", deletes, puts)
	}
}

func TestClient_UpdateEvent_AllDayDateForm(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-allday-1.ics"
	m.objects["uid-allday-1"] = mockObject{path: path, ics: icsAllDay}
	client := m.client()

	start := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	if err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-allday-1", &EventUpdate{
		StartTime: &start, EndTime: &end, StartDateOnly: true, EndDateOnly: true,
	}); err != nil {
		t.Fatalf("DATE update = %v", err)
	}
	m.mu.Lock()
	body := m.puts[0].body
	m.mu.Unlock()
	if !strings.Contains(body, "DTSTART;VALUE=DATE:20260713") || !strings.Contains(body, "DTEND;VALUE=DATE:20260714") {
		t.Fatalf("DATE form was not preserved:\n%s", body)
	}

	timedStart := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	before := len(m.puts)
	if err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-allday-1", &EventUpdate{
		StartTime: &timedStart, StartDateOnly: true,
	}); err == nil {
		t.Fatal("DATE-only start on a timed event was accepted")
	}
	if len(m.puts) != before {
		t.Fatal("invalid DATE form issued a PUT")
	}
}

func TestClient_FloatingOccurrenceRoundTrip(t *testing.T) {
	floating := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:floating-series\r\nSUMMARY:Floating\r\n" +
		"DTSTART:20260701T090000\r\nDTEND:20260701T100000\r\nDTSTAMP:20260701T000000Z\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			m := newMockCalDAV(t)
			path := testHomeCalendar + "floating-series.ics"
			m.objects["floating-series"] = mockObject{path: path, ics: floating}
			client := m.client()
			recID, err := ParseDateTime("recurrence_id", "2026-07-01T09:00:00", paris)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "update" {
				title := "Updated"
				if err := client.UpdateEvent(context.Background(), testHomeCalendar, "floating-series", &EventUpdate{
					Title: &title, Scope: ScopeOccurrence, RecurrenceID: &recID,
				}); err != nil {
					t.Fatalf("floating update = %v", err)
				}
				m.mu.Lock()
				body := m.puts[0].body
				m.mu.Unlock()
				if !strings.Contains(body, "RECURRENCE-ID:20260701T090000") || strings.Contains(body, "RECURRENCE-ID:20260701T070000") {
					t.Fatalf("floating RECURRENCE-ID shifted:\n%s", body)
				}
			} else {
				if _, err := client.DeleteEvent(context.Background(), testHomeCalendar, "floating-series", &DeleteOptions{
					Scope: ScopeOccurrence, RecurrenceID: &recID,
				}); err != nil {
					t.Fatalf("floating delete = %v", err)
				}
				m.mu.Lock()
				body := m.puts[0].body
				m.mu.Unlock()
				if !strings.Contains(body, "EXDATE:20260701T090000") || strings.Contains(body, "EXDATE:20260701T070000") {
					t.Fatalf("floating EXDATE shifted:\n%s", body)
				}
			}
		})
	}
}

func TestClient_SearchEventsWidensRemoteWindowForNonUTCQuery(t *testing.T) {
	floating := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\n" +
		"UID:floating-search\r\nSUMMARY:Floating\r\nDTSTART:20260701T123000\r\nDTEND:20260701T124500\r\n" +
		"DTSTAMP:20260701T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	m := newMockCalDAV(t)
	m.objects["floating-search"] = mockObject{path: testHomeCalendar + "floating-search.ics", ics: floating}
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC).In(paris)
	end := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC).In(paris)
	result, err := m.client().SearchEvents(context.Background(), testHomeCalendar, start, end, nil)
	if err != nil {
		t.Fatalf("SearchEvents() error = %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].UID != "floating-search" {
		t.Fatalf("floating result = %#v", result.Events)
	}
	m.mu.Lock()
	report := string(m.lastReportBody)
	m.mu.Unlock()
	widened := start.Add(-26 * time.Hour).UTC().Format(icalTimeLayout)
	if !strings.Contains(report, `start="`+widened+`"`) {
		t.Fatalf("REPORT did not widen the remote window:\n%s", report)
	}
}

func TestClient_CreateEventReconciliationRejectsDifferentAlarmSemantics(t *testing.T) {
	m := newMockCalDAV(t)
	existing := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\n" +
		"UID:alarm-uid\r\nSUMMARY:Alarm event\r\nDTSTAMP:20260701T000000Z\r\n" +
		"DTSTART:20260701T090000Z\r\nDTEND:20260701T100000Z\r\n" +
		"BEGIN:VALARM\r\nACTION:EMAIL\r\nTRIGGER:-PT15M\r\nATTENDEE:mailto:private@example.test\r\n" +
		"SUMMARY:Reminder\r\nDESCRIPTION:Private\r\nEND:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	m.objects["alarm-uid"] = mockObject{path: testHomeCalendar + "alarm.ics", ics: existing}
	client := m.client()
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	_, err := client.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title: "Alarm event", StartTime: start, EndTime: start.Add(time.Hour),
		AlarmMinutesBefore: 15, ClientUID: "alarm-uid",
	})
	if ie := AsICloudError(err); ie == nil || ie.Code != CodeConflict {
		t.Fatalf("alarm semantics reconciliation = %v", err)
	}
	m.mu.Lock()
	puts := len(m.puts)
	m.mu.Unlock()
	if puts != 0 {
		t.Fatalf("different alarm semantics issued %d PUTs", puts)
	}
}
