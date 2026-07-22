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
		IfMatchETag: "stale",
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
		IfMatchETag:  "e1",
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
	m.objects["client-uid-1"] = mockObject{path: path, ics: icsSimpleEvent}
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
	// Use simple event as master; occurrence update should still produce a PUT
	// with RECURRENCE-ID when scope=occurrence.
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	m.etags[path] = `"e1"`
	c := m.client()
	recID := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	newTitle := "Only this day"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{
		Title:        &newTitle,
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
		IfMatchETag:  "e1",
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

func TestBuildEventCalendar_AllDayAndRRULE(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cal := buildEventCalendar("u@x", &NewEvent{
		Title: "Holiday", AllDay: true,
		StartTime: start, EndTime: start.Add(24 * time.Hour),
		Recurrence: "FREQ=YEARLY;COUNT=3",
		Status:     "CONFIRMED",
	})
	master, err := findMasterVEvent(cal)
	if err != nil {
		t.Fatal(err)
	}
	if p := master.Props.Get("RRULE"); p == nil {
		t.Error("missing RRULE")
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
	if got := FormatEventTime(d, true); got != "2026-07-01" {
		t.Errorf("got %q", got)
	}
	if FormatEventTime(time.Time{}, false) != "" {
		t.Error("zero")
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
	if _, err := ParseWorkingHours("25:00"); err == nil {
		t.Fatal("expected error")
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
