package icloud

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

// foldInstantOf returns the instant with the given UTC offset for a repeated
// wall clock.
func foldInstantOf(t *testing.T, loc *time.Location, wall time.Time, offsetSeconds int) time.Time {
	t.Helper()
	base := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC)
	candidate := base.Add(-time.Duration(offsetSeconds) * time.Second)
	local := candidate.In(loc)
	if local.Format("20060102T150405") != wall.Format("20060102T150405") {
		t.Fatalf("%v is not a wall time of %v at offset %d", wall, loc, offsetSeconds)
	}
	if _, offset := local.Zone(); offset != offsetSeconds {
		t.Fatalf("offset = %d, want %d", offset, offsetSeconds)
	}
	return candidate
}

// createPutBodies returns the captured PUT bodies.
func createPutBodies(m *mockCalDAV) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	bodies := make([]string, 0, len(m.puts))
	for _, put := range m.puts {
		bodies = append(bodies, put.body)
	}
	return bodies
}

// TestCreateEventRejectsSecondFoldEnd is the FIND-1 defect: create_event
// guarded only the start, so a second-fold end was serialized as a bare TZID
// wall time that re-read at a different instant.
func TestCreateEventRejectsSecondFoldEnd(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	wall := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC)
	first := foldInstantOf(t, paris, wall, 2*60*60)
	second := foldInstantOf(t, paris, wall, 1*60*60)
	if !first.Before(second) {
		t.Fatalf("first fold %v is not before second fold %v", first, second)
	}

	t.Run("second fold end is rejected before PUT", func(t *testing.T) {
		m := newMockCalDAV(t)
		uid, err := m.client().CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
			Title: "Fold end", StartTime: first, EndTime: second, Timezone: "Europe/Paris",
		})
		typed := AsICloudError(err)
		if typed == nil || typed.Code != CodeValidation {
			t.Fatalf("second-fold end = %v (uid %q), want code %q", err, uid, CodeValidation)
		}
		if !strings.Contains(typed.Message, "Europe/Paris") {
			t.Fatalf("message = %q, want it to name Europe/Paris", typed.Message)
		}
		if typed.Details["reconciliation"] == "" {
			t.Fatalf("validation error lost its guidance: %+v", typed)
		}
		if bodies := createPutBodies(m); len(bodies) != 0 {
			t.Fatalf("rejected end issued %d PUT(s):\n%s", len(bodies), bodies[0])
		}
	})

	t.Run("second fold start is rejected before PUT", func(t *testing.T) {
		m := newMockCalDAV(t)
		_, err := m.client().CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
			Title: "Fold start", StartTime: second, EndTime: second.Add(time.Hour), Timezone: "Europe/Paris",
		})
		if typed := AsICloudError(err); typed == nil || typed.Code != CodeValidation {
			t.Fatalf("second-fold start = %v, want validation", err)
		}
		if bodies := createPutBodies(m); len(bodies) != 0 {
			t.Fatalf("rejected start issued %d PUT(s)", len(bodies))
		}
	})

	t.Run("first fold bounds round-trip", func(t *testing.T) {
		m := newMockCalDAV(t)
		// Two hours after the first fold is 03:30 Paris, unambiguous. One hour
		// would land on the second fold.
		end := first.Add(2 * time.Hour)
		if _, err := m.client().CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
			Title: "First fold", StartTime: first, EndTime: end, Timezone: "Europe/Paris",
		}); err != nil {
			t.Fatalf("first-fold create = %v", err)
		}
		bodies := createPutBodies(m)
		if len(bodies) != 1 {
			t.Fatalf("puts = %d, want 1", len(bodies))
		}
		cal, err := ical.NewDecoder(strings.NewReader(bodies[0])).Decode()
		if err != nil {
			t.Fatalf("decode PUT body: %v", err)
		}
		vevent, err := findMasterVEvent(cal)
		if err != nil {
			t.Fatalf("findMasterVEvent: %v", err)
		}
		for property, want := range map[string]time.Time{ical.PropDateTimeStart: first, ical.PropDateTimeEnd: end} {
			stored := vevent.Props.Get(property)
			if stored == nil {
				t.Fatalf("PUT lost %s:\n%s", property, bodies[0])
			}
			parsed, err := parseEventDateTimeProp(stored)
			if err != nil {
				t.Fatalf("parse %s: %v", property, err)
			}
			if !parsed.instant.Equal(want) {
				t.Fatalf("%s re-reads as %s, want %s", property,
					parsed.instant.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
			}
		}
	})

	t.Run("normal hour and UTC event are accepted", func(t *testing.T) {
		m := newMockCalDAV(t)
		start := time.Date(2026, 10, 26, 9, 0, 0, 0, paris)
		if _, err := m.client().CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
			Title: "Normal", StartTime: start, EndTime: start.Add(time.Hour), Timezone: "Europe/Paris",
		}); err != nil {
			t.Fatalf("normal-hour create = %v", err)
		}

		m2 := newMockCalDAV(t)
		// A UTC write keeps the absolute instant, so a second-fold instant is
		// representable.
		if _, err := m2.client().CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
			Title: "UTC", StartTime: first, EndTime: second.Add(time.Hour),
		}); err != nil {
			t.Fatalf("UTC create with a fold instant = %v", err)
		}
	})
}

// TestCreateEventAllDayIgnoresFoldCheck keeps an all-day write independent of
// the timezone fold rule, because it stores date components only.
func TestCreateEventAllDayIgnoresFoldCheck(t *testing.T) {
	m := newMockCalDAV(t)
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	if _, err := m.client().CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title: "All day", StartTime: day, EndTime: day.Add(24 * time.Hour), AllDay: true, Timezone: "Europe/Paris",
	}); err != nil {
		t.Fatalf("all-day create = %v", err)
	}
	bodies := createPutBodies(m)
	if len(bodies) != 1 || !strings.Contains(bodies[0], "VALUE=DATE:20261025") {
		t.Fatalf("all-day PUT lost the DATE form:\n%s", bodies)
	}
}

// TestValidateNamedTZIDRoundTrip pins the shared helper contract.
func TestValidateNamedTZIDRoundTrip(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC).Add(-2 * time.Hour)
	second := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC).Add(-time.Hour)

	if err := ValidateNamedTZIDRoundTrip("start", first, paris); err != nil {
		t.Fatalf("first fold = %v", err)
	}
	if err := ValidateNamedTZIDRoundTrip("start", second, paris); err == nil {
		t.Fatal("second fold was accepted")
	} else if typed := AsICloudError(err); typed == nil || typed.Code != CodeValidation {
		t.Fatalf("error = %v, want validation", err)
	}
	// UTC has no fold, so any instant round-trips.
	if err := ValidateNamedTZIDRoundTrip("start", second, time.UTC); err != nil {
		t.Fatalf("UTC = %v", err)
	}
}
