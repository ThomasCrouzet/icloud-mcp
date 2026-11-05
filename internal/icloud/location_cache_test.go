package icloud

import (
	"context"
	"testing"
	"time"
)

func TestValidatedSearchLocationAvoidsWideReportLookup(t *testing.T) {
	m := newMockCalDAV(t)
	imported := testHomeCalendar + "imported-name.ics"
	m.objects["uid-simple-1"] = mockObject{path: imported, ics: icsSimpleEvent}
	client := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	if _, err := client.SearchEvents(context.Background(), testHomeCalendar, start, end, nil); err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	m.mu.Lock()
	m.lastReportBody = nil
	m.gets = nil
	m.mu.Unlock()

	detail, err := client.GetEvent(context.Background(), testHomeCalendar, "uid-simple-1")
	if err != nil {
		t.Fatalf("GetEvent with cached location: %v", err)
	}
	if detail.UID != "uid-simple-1" {
		t.Fatalf("UID = %q", detail.UID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastReportBody != nil {
		t.Fatal("cached location still used the wide REPORT fallback")
	}
	found := false
	for _, path := range m.gets {
		if path == imported {
			found = true
		}
	}
	if !found {
		t.Fatalf("cached GET paths = %v, want imported path", m.gets)
	}
}

func TestStaleLocationEvictsAndFallsBack(t *testing.T) {
	m := newMockCalDAV(t)
	imported := testHomeCalendar + "imported-name.ics"
	m.objects["uid-simple-1"] = mockObject{path: imported, ics: icsSimpleEvent}
	client := m.client()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	if _, err := client.SearchEvents(context.Background(), testHomeCalendar, start, end, nil); err != nil {
		t.Fatal(err)
	}
	delete(m.objects, "uid-simple-1")
	m.mu.Lock()
	m.lastReportBody = nil
	m.mu.Unlock()
	if _, err := client.GetEvent(context.Background(), testHomeCalendar, "uid-simple-1"); err == nil {
		t.Fatal("stale cached location unexpectedly returned an event")
	}
	if _, ok := client.locations.get(testHomeCalendar, "uid-simple-1"); ok {
		t.Fatal("stale cached location was not evicted")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastReportBody == nil {
		t.Fatal("stale location did not fall back to the bounded REPORT")
	}
}

func TestEventLocationCacheIsolatesCalendarsAndBoundsEntries(t *testing.T) {
	cache := newEventLocationCache()
	cache.put("/cal/a/", "same", "/cal/a/one.ics")
	cache.put("/cal/b/", "same", "/cal/b/two.ics")
	if path, ok := cache.get("/cal/a/", "same"); !ok || path != "/cal/a/one.ics" {
		t.Fatalf("calendar A path = %q, %v", path, ok)
	}
	if path, ok := cache.get("/cal/b/", "same"); !ok || path != "/cal/b/two.ics" {
		t.Fatalf("calendar B path = %q, %v", path, ok)
	}
	cache.put("/cal/a/", "escape", "/cal/b/two.ics")
	if _, ok := cache.get("/cal/a/", "escape"); ok {
		t.Fatal("cache accepted a resource outside its calendar")
	}
	for index := 0; index < maxCachedEventLocations+10; index++ {
		cache.put("/cal/a/", string(rune('a'+index%26))+time.Duration(index).String(), "/cal/a/event.ics")
	}
	if len(cache.entries) > maxCachedEventLocations {
		t.Fatalf("cache entries = %d, want at most %d", len(cache.entries), maxCachedEventLocations)
	}
}
