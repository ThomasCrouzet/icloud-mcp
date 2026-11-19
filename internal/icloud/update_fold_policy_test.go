package icloud

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

// foldInstant returns the instant with the given UTC offset for a repeated
// wall clock. The caller picks the offset, so a test can name the first or the
// second occurrence of the fold.
func foldInstant(t *testing.T, loc *time.Location, wall time.Time, wantOffsetSeconds int) time.Time {
	t.Helper()
	base := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC)
	candidate := base.Add(-time.Duration(wantOffsetSeconds) * time.Second)
	local := candidate.In(loc)
	if local.Format("20060102T150405") != wall.Format("20060102T150405") {
		t.Fatalf("%v is not a wall time of %v at offset %d", wall, loc, wantOffsetSeconds)
	}
	if _, offset := local.Zone(); offset != wantOffsetSeconds {
		t.Fatalf("offset = %d, want %d", offset, wantOffsetSeconds)
	}
	return candidate
}

// tzidDayEvent builds a one-event VCALENDAR that spans the whole fold day in
// one named zone. The wide interval keeps a one-sided patch valid, so a fold
// rejection cannot come from the end-after-start check.
func tzidDayEvent(uid, zone string, day time.Time) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:Zone fixture\r\n" +
		"DTSTART;TZID=" + zone + ":" + day.Format("20060102") + "T000000\r\n" +
		"DTEND;TZID=" + zone + ":" + day.Format("20060102") + "T230000\r\n" +
		"DTSTAMP:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

// tzidSeries builds a one-hour weekly named-TZID series.
func tzidSeries(uid, zone string, start, end time.Time) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nSUMMARY:Zone series\r\n" +
		"DTSTART;TZID=" + zone + ":" + start.Format("20060102T150405") + "\r\n" +
		"DTEND;TZID=" + zone + ":" + end.Format("20060102T150405") + "\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=6\r\n" +
		"DTSTAMP:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func assertFoldRejection(t *testing.T, err error, zone string) {
	t.Helper()
	if err == nil {
		t.Fatal("second-fold instant was accepted, want a typed validation error")
	}
	typed := AsICloudError(err)
	if typed == nil || typed.Code != CodeValidation {
		t.Fatalf("error = %v, want code %q", err, CodeValidation)
	}
	if !strings.Contains(typed.Message, zone) {
		t.Fatalf("message = %q, want it to name %s", typed.Message, zone)
	}
	if typed.Details["reconciliation"] == "" {
		t.Fatalf("validation error lost its guidance: %+v", typed)
	}
}

// assertPropInstant requires that an encoded property re-reads as want.
func assertPropInstant(t *testing.T, vevent *ical.Event, property string, want time.Time, body string) {
	t.Helper()
	stored := vevent.Props.Get(property)
	if stored == nil {
		t.Fatalf("encoded event lost %s:\n%s", property, body)
	}
	parsed, err := parseEventDateTimeProp(stored)
	if err != nil {
		t.Fatalf("parse stored %s: %v", property, err)
	}
	if !parsed.instant.Equal(want) {
		t.Fatalf("stored %s re-reads as %s, want %s (value %q)",
			property, parsed.instant.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339), stored.Value)
	}
}

// assertPUTRoundTrip decodes the last PUT and requires that the stored value
// re-reads as the requested instant.
func assertPUTRoundTrip(t *testing.T, m *mockCalDAV, property string, want time.Time) {
	t.Helper()
	m.mu.Lock()
	if len(m.puts) == 0 {
		m.mu.Unlock()
		t.Fatal("no PUT was issued")
	}
	body := m.puts[len(m.puts)-1].body
	m.mu.Unlock()
	cal, err := ical.NewDecoder(strings.NewReader(body)).Decode()
	if err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	vevent, err := findMasterVEvent(cal)
	if err != nil {
		t.Fatalf("findMasterVEvent: %v", err)
	}
	assertPropInstant(t, vevent, property, want, body)
}

// TestUpdateEventRejectsSecondFoldInstantForNamedTZID is the #223 defect: the
// requested second-fold instant serializes as a bare TZID wall time, which
// re-reads as the first fold and moves the event by one hour.
func TestUpdateEventRejectsSecondFoldInstantForNamedTZID(t *testing.T) {
	tests := []struct {
		name         string
		zone         string
		wall         time.Time
		firstOffset  int
		secondOffset int
	}{
		{
			name: "Europe/Paris one hour fold", zone: "Europe/Paris",
			wall:        time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC),
			firstOffset: 2 * 60 * 60, secondOffset: 1 * 60 * 60,
		},
		{
			name: "America/New_York one hour fold", zone: "America/New_York",
			wall:        time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC),
			firstOffset: -(4 * 60 * 60), secondOffset: -(5 * 60 * 60),
		},
		{
			name: "Australia/Lord_Howe half hour fold", zone: "Australia/Lord_Howe",
			wall:        time.Date(2026, 4, 5, 1, 45, 0, 0, time.UTC),
			firstOffset: 11 * 60 * 60, secondOffset: 10*60*60 + 30*60,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loc := mustLoadLocation(t, test.zone)
			first := foldInstant(t, loc, test.wall, test.firstOffset)
			second := foldInstant(t, loc, test.wall, test.secondOffset)
			if !first.Before(second) {
				t.Fatalf("first fold %v is not before second fold %v", first, second)
			}
			fixture := tzidDayEvent("uid-fold", test.zone, test.wall)

			t.Run("start only/second fold rejected", func(t *testing.T) {
				m := newMockCalDAV(t)
				path := testHomeCalendar + "uid-fold.ics"
				m.objects["uid-fold"] = mockObject{path: path, ics: fixture}
				m.etags[path] = `"v1"`
				start := second
				err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-fold",
					&EventUpdate{StartTime: &start})
				assertFoldRejection(t, err, test.zone)
				if bodies := putBodies(m); len(bodies) != 0 {
					t.Fatalf("rejected second-fold start issued %d PUT(s):\n%s", len(bodies), bodies[0])
				}
			})

			t.Run("start only/first fold accepted", func(t *testing.T) {
				m := newMockCalDAV(t)
				path := testHomeCalendar + "uid-fold.ics"
				m.objects["uid-fold"] = mockObject{path: path, ics: fixture}
				m.etags[path] = `"v1"`
				start := first
				if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-fold",
					&EventUpdate{StartTime: &start}); err != nil {
					t.Fatalf("first-fold start update = %v", err)
				}
				assertPUTRoundTrip(t, m, ical.PropDateTimeStart, first)
			})

			t.Run("start and end/second fold rejected", func(t *testing.T) {
				m := newMockCalDAV(t)
				path := testHomeCalendar + "uid-fold.ics"
				m.objects["uid-fold"] = mockObject{path: path, ics: fixture}
				m.etags[path] = `"v1"`
				start, end := second, second.Add(2*time.Hour)
				err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-fold",
					&EventUpdate{StartTime: &start, EndTime: &end})
				assertFoldRejection(t, err, test.zone)
				if bodies := putBodies(m); len(bodies) != 0 {
					t.Fatalf("rejected second-fold end issued %d PUT(s):\n%s", len(bodies), bodies[0])
				}
			})

			t.Run("end only/second fold rejected", func(t *testing.T) {
				m := newMockCalDAV(t)
				path := testHomeCalendar + "uid-fold.ics"
				m.objects["uid-fold"] = mockObject{path: path, ics: fixture}
				m.etags[path] = `"v1"`
				end := second
				err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-fold",
					&EventUpdate{EndTime: &end})
				assertFoldRejection(t, err, test.zone)
				if bodies := putBodies(m); len(bodies) != 0 {
					t.Fatalf("rejected second-fold end issued %d PUT(s):\n%s", len(bodies), bodies[0])
				}
			})

			t.Run("end only/first fold accepted", func(t *testing.T) {
				m := newMockCalDAV(t)
				path := testHomeCalendar + "uid-fold.ics"
				m.objects["uid-fold"] = mockObject{path: path, ics: fixture}
				m.etags[path] = `"v1"`
				end := first
				if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-fold",
					&EventUpdate{EndTime: &end}); err != nil {
					t.Fatalf("first-fold end update = %v", err)
				}
				assertPUTRoundTrip(t, m, ical.PropDateTimeEnd, first)
			})
		})
	}
}

// TestUpdateEventAcceptsRepresentableNamedTZIDInstants keeps normal hours, both
// fold instants on a UTC event, and floating values working with exact
// instants.
func TestUpdateEventAcceptsRepresentableNamedTZIDInstants(t *testing.T) {
	paris := mustLoadLocation(t, "Europe/Paris")
	// The series spans October and November so a single-bound patch always
	// keeps a valid interval.
	normalFixture := tzidSeries("uid-paris-series", "Europe/Paris",
		time.Date(2026, 10, 1, 0, 0, 0, 0, paris), time.Date(2026, 12, 31, 23, 0, 0, 0, paris))

	t.Run("normal hour on a named TZID series", func(t *testing.T) {
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-paris-series.ics"
		m.objects["uid-paris-series"] = mockObject{path: path, ics: normalFixture}
		m.etags[path] = `"v1"`
		start := time.Date(2026, 10, 27, 12, 0, 0, 0, paris)
		end := start.Add(time.Hour)
		if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-paris-series",
			&EventUpdate{StartTime: &start, EndTime: &end}); err != nil {
			t.Fatalf("normal hour update = %v", err)
		}
		puts := putBodies(m)
		if len(puts) != 1 {
			t.Fatalf("puts = %d, want 1", len(puts))
		}
		cal, err := ical.NewDecoder(strings.NewReader(puts[0])).Decode()
		if err != nil {
			t.Fatal(err)
		}
		vevent, err := findMasterVEvent(cal)
		if err != nil {
			t.Fatal(err)
		}
		assertPropInstant(t, vevent, ical.PropDateTimeStart, start, puts[0])
		assertPropInstant(t, vevent, ical.PropDateTimeEnd, end, puts[0])
	})

	t.Run("winter hour after the fold", func(t *testing.T) {
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-paris-series.ics"
		m.objects["uid-paris-series"] = mockObject{path: path, ics: normalFixture}
		m.etags[path] = `"v1"`
		start := time.Date(2026, 11, 2, 9, 0, 0, 0, paris)
		if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-paris-series",
			&EventUpdate{StartTime: &start}); err != nil {
			t.Fatalf("winter hour update = %v", err)
		}
		assertPUTRoundTrip(t, m, ical.PropDateTimeStart, start)
	})

	t.Run("UTC event keeps the second-fold instant", func(t *testing.T) {
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-simple-1.ics"
		m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
		m.etags[path] = `"v1"`
		// A Paris second-fold instant is representable in a UTC event, so the
		// check must not reject it.
		start := foldInstant(t, paris, time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC), 3600)
		end := start.Add(time.Hour)
		if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1",
			&EventUpdate{StartTime: &start, EndTime: &end}); err != nil {
			t.Fatalf("UTC event update = %v", err)
		}
		puts := putBodies(m)
		if len(puts) != 1 {
			t.Fatalf("puts = %d, want 1", len(puts))
		}
		cal, err := ical.NewDecoder(strings.NewReader(puts[0])).Decode()
		if err != nil {
			t.Fatal(err)
		}
		vevent, err := findMasterVEvent(cal)
		if err != nil {
			t.Fatal(err)
		}
		assertPropInstant(t, vevent, ical.PropDateTimeStart, start, puts[0])
		assertPropInstant(t, vevent, ical.PropDateTimeEnd, end, puts[0])
	})

	t.Run("floating value keeps its wall clock", func(t *testing.T) {
		floating := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
			"BEGIN:VEVENT\r\nUID:uid-floating-1\r\nSUMMARY:Floating\r\n" +
			"DTSTART:20261026T023000\r\nDTEND:20261026T033000\r\n" +
			"DTSTAMP:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-floating-1.ics"
		m.objects["uid-floating-1"] = mockObject{path: path, ics: floating}
		m.etags[path] = `"v1"`
		start := time.Date(2026, 10, 26, 2, 30, 0, 0, time.UTC)
		if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-floating-1",
			&EventUpdate{StartTime: &start}); err != nil {
			t.Fatalf("floating update = %v", err)
		}
		if bodies := putBodies(m); len(bodies) != 1 || !strings.Contains(bodies[0], "DTSTART:20261026T023000") {
			t.Fatalf("floating value changed form:\n%s", bodies)
		}
	})
}

// TestUpdateEventDerivedOccurrenceEndFold covers a start-only occurrence update
// whose derived end lands on the second fold.
func TestUpdateEventDerivedOccurrenceEndFold(t *testing.T) {
	paris := mustLoadLocation(t, "Europe/Paris")
	wall := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC)
	first := foldInstant(t, paris, wall, 7200)
	// The series holds one hour from local 00:00 on the fold day.
	fixture := tzidSeries("uid-paris-fold", "Europe/Paris",
		time.Date(2026, 10, 25, 0, 0, 0, 0, paris), time.Date(2026, 10, 25, 1, 0, 0, 0, paris))
	recID := time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC) // 00:00 Paris, pre-fold

	t.Run("derived end in the second fold is rejected", func(t *testing.T) {
		// Moving the start to the first-fold 02:30 derives an end one hour
		// later, which is the second-fold 02:30. A bare TZID value cannot
		// express it.
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-paris-fold.ics"
		m.objects["uid-paris-fold"] = mockObject{path: path, ics: fixture}
		m.etags[path] = `"v1"`
		start := first
		err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-paris-fold",
			&EventUpdate{StartTime: &start, Scope: ScopeOccurrence, RecurrenceID: &recID})
		assertFoldRejection(t, err, "Europe/Paris")
		if bodies := putBodies(m); len(bodies) != 0 {
			t.Fatalf("rejected occurrence issued %d PUT(s)", len(bodies))
		}
	})

	t.Run("derived end outside the fold is accepted", func(t *testing.T) {
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-paris-fold.ics"
		m.objects["uid-paris-fold"] = mockObject{path: path, ics: fixture}
		m.etags[path] = `"v1"`
		start := first.Add(5 * time.Hour) // 07:30 Paris wall, unambiguous
		if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-paris-fold",
			&EventUpdate{StartTime: &start, Scope: ScopeOccurrence, RecurrenceID: &recID}); err != nil {
			t.Fatalf("unambiguous occurrence update = %v", err)
		}
		assertOverridePutRoundTrip(t, m, ical.PropDateTimeStart, start)
	})
}

// TestUpdateEventRejectsFoldInMixedForms proves only the named-TZID property is
// fold-constrained: a UTC end keeps its instant.
func TestUpdateEventRejectsFoldInMixedForms(t *testing.T) {
	paris := mustLoadLocation(t, "Europe/Paris")
	mixed := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:uid-paris-utc-end\r\nSUMMARY:Mixed forms\r\n" +
		"DTSTART;TZID=Europe/Paris:20261025T000000\r\n" +
		"DTEND:20261025T080000Z\r\n" +
		"DTSTAMP:20260101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	second := foldInstant(t, paris, time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC), 3600)

	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-paris-utc-end.ics"
	m.objects["uid-paris-utc-end"] = mockObject{path: path, ics: mixed}
	m.etags[path] = `"v1"`
	err := m.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-paris-utc-end",
		&EventUpdate{StartTime: &second})
	assertFoldRejection(t, err, "Europe/Paris")
	if bodies := putBodies(m); len(bodies) != 0 {
		t.Fatalf("rejected mixed-form update issued %d PUT(s)", len(bodies))
	}

	m2 := newMockCalDAV(t)
	path2 := testHomeCalendar + "uid-paris-utc-end.ics"
	m2.objects["uid-paris-utc-end"] = mockObject{path: path2, ics: mixed}
	m2.etags[path2] = `"v1"`
	end := second.Add(2 * time.Hour)
	if err := m2.client().UpdateEvent(context.Background(), testHomeCalendar, "uid-paris-utc-end",
		&EventUpdate{EndTime: &end}); err != nil {
		t.Fatalf("UTC end update = %v", err)
	}
	assertPUTRoundTrip(t, m2, ical.PropDateTimeEnd, end)
}

// TestSetDatePropLikeFoldPolicy pins the writer contract directly: the second
// fold of a named TZID is rejected, and the first fold round-trips.
func TestSetDatePropLikeFoldPolicy(t *testing.T) {
	paris := mustLoadLocation(t, "Europe/Paris")
	wall := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC)
	first := foldInstant(t, paris, wall, 7200)
	second := foldInstant(t, paris, wall, 3600)

	prop := ical.NewProp(ical.PropDateTimeStart)
	prop.Params.Set(ical.PropTimezoneID, "Europe/Paris")
	prop.SetDateTime(time.Date(2026, 10, 25, 1, 0, 0, 0, paris))

	err := setDatePropLike(prop, prop, second)
	if typed := AsICloudError(err); typed == nil || typed.Code != CodeValidation {
		t.Fatalf("second fold = %v, want validation", err)
	}
	if err := setDatePropLike(prop, prop, first); err != nil {
		t.Fatalf("first fold = %v", err)
	}
	// The project reader applies the RFC 5545 first-fold policy. go-ical's
	// DateTime delegates to ParseInLocation, which selects the second fold.
	reRead, err := parseEventDateTimeProp(prop)
	if err != nil {
		t.Fatal(err)
	}
	if !reRead.instant.Equal(first) {
		t.Fatalf("re-read %s, want %s", reRead.instant.UTC().Format(time.RFC3339), first.UTC().Format(time.RFC3339))
	}
}

// assertOverridePutRoundTrip requires that the detached override of the last
// PUT re-reads the requested instant.
func assertOverridePutRoundTrip(t *testing.T, m *mockCalDAV, property string, want time.Time) {
	t.Helper()
	m.mu.Lock()
	if len(m.puts) == 0 {
		m.mu.Unlock()
		t.Fatal("no PUT was issued")
	}
	body := m.puts[len(m.puts)-1].body
	m.mu.Unlock()
	cal, err := ical.NewDecoder(strings.NewReader(body)).Decode()
	if err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent || child.Props.Get(ical.PropRecurrenceID) == nil {
			continue
		}
		assertPropInstant(t, &ical.Event{Component: child}, property, want, body)
		return
	}
	t.Fatalf("PUT has no override VEVENT:\n%s", body)
}
