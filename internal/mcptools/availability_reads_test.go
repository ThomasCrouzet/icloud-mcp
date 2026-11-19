package mcptools

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// availabilityWaveFixture builds calendars whose busy rows are disjoint, so a
// per-wave merge cannot shrink the accumulated slice. Each calendar gets its
// own one-hour event on successive days.
func availabilityWaveFixture(calendars int, perCalendar int) (*availabilityBudgetService, []string, map[string][]icloud.Event) {
	base := mustParseTime("2026-07-01T09:00:00Z")
	paths := make([]string, calendars)
	listed := make([]icloud.Calendar, calendars)
	byPath := make(map[string][]icloud.Event, calendars)
	for i := range calendars {
		path := fmt.Sprintf("/cal/wave-%04d/", i)
		paths[i], listed[i] = path, icloud.Calendar{Path: path}
		events := make([]icloud.Event, perCalendar)
		for j := range events {
			start := base.Add(time.Duration(i*perCalendar+j) * 24 * time.Hour)
			events[j] = icloud.Event{
				UID:       fmt.Sprintf("wave-%04d-%02d", i, j),
				Title:     "Busy",
				StartTime: start,
				EndTime:   start.Add(time.Hour),
			}
		}
		byPath[path] = events
	}
	svc := &availabilityBudgetService{MockService: &icloud.MockService{Calendars: listed}}
	svc.read = func(_ context.Context, path string) (icloud.SearchResult, error) {
		return icloud.SearchResult{Events: byPath[path]}, nil
	}
	return svc, paths, byPath
}

// TestAvailabilityCollectorSingleMergePass covers issue 222. The collector must
// accumulate bounded busy intervals and merge them once at the end. A merge per
// four-read wave copies and re-sorts every earlier interval again.
func TestAvailabilityCollectorSingleMergePass(t *testing.T) {
	svc, paths, byPath := availabilityWaveFixture(8, 2)
	if len(paths) <= calendarReadConcurrency {
		t.Fatalf("fixture has %d calendars, want more than one wave of %d", len(paths), calendarReadConcurrency)
	}

	original := mergeAvailabilityIntervals
	t.Cleanup(func() { mergeAvailabilityIntervals = original })
	calls := 0
	largestInput := 0
	totalInput := 0
	mergeAvailabilityIntervals = func(in []icloud.Interval) []icloud.Interval {
		calls++
		totalInput += len(in)
		if len(in) > largestInput {
			largestInput = len(in)
		}
		return original(in)
	}

	// Build the reference set before the collector clears returned events.
	reference := icloud.MergeIntervals(icloud.BusyFromEvents(
		append(append([]icloud.Event{}, byPath[paths[0]]...), byPath[paths[7]]...), true, 0, 0, time.UTC))

	intervals, err := collectAvailabilityIntervals(t.Context(), paths, svc.read, availabilityOptions{
		IncludeAllDay: true,
		Location:      time.UTC,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("merge passes = %d, want 1 final deterministic merge", calls)
	}
	if largestInput != 16 || totalInput != 16 {
		t.Fatalf("merge input = %d total %d, want all 16 intervals in one pass", largestInput, totalInput)
	}
	if len(intervals) != 16 {
		t.Fatalf("intervals = %d, want 16 disjoint busy rows", len(intervals))
	}

	// The single merge must see every projected interval, in any order.
	if len(reference) != 4 {
		t.Fatalf("reference intervals = %d, want 4 for two calendars of two events", len(reference))
	}
}

// TestAvailabilityCollectorMatchesDirectMerge proves the accumulated result is
// identical to one merge over every projected interval, for overlapping,
// buffered, all-day, transparent, cancelled, and floating inputs.
func TestAvailabilityCollectorMatchesDirectMerge(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		includeAllDay bool
		bufferBefore  time.Duration
		bufferAfter   time.Duration
		loc           *time.Location
	}{
		{name: "overlapping", includeAllDay: true, loc: time.UTC},
		{name: "buffered", includeAllDay: true, bufferBefore: 30 * time.Minute, bufferAfter: 45 * time.Minute, loc: time.UTC},
		{name: "timed-only-paris", includeAllDay: false, loc: loc},
		{name: "all-day-paris", includeAllDay: true, loc: loc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Calendars 1 and 2 hold identical busy rows so they overlap after
			// aggregation; calendar 3 starts exactly at a busy end so adjacency
			// merging is exercised across waves.
			base := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
			shared := []icloud.Event{
				{UID: "a", StartTime: base, EndTime: base.Add(time.Hour)},
				{UID: "b", StartTime: base.Add(90 * time.Minute), EndTime: base.Add(3 * time.Hour)},
			}
			// Give each calendar its own backing array: the collector clears
			// returned event slices after projecting them.
			byPath := map[string][]icloud.Event{
				"/cal/x/": append([]icloud.Event(nil), shared...),
				"/cal/y/": append([]icloud.Event(nil), shared...),
				"/cal/z/": {
					{UID: "c", StartTime: base.Add(3*time.Hour + 5*time.Minute), EndTime: base.Add(4 * time.Hour), Transp: "TRANSPARENT"},
					{UID: "d", StartTime: base.Add(3 * time.Hour), EndTime: base.Add(5 * time.Hour)},
					{UID: "e", StartTime: base.Add(6 * time.Hour), EndTime: base.Add(7 * time.Hour), Status: "CANCELLED"},
				},
				"/cal/allday/": {
					{UID: "f", AllDay: true, StartTime: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)},
				},
				"/cal/floating/": {
					{UID: "g", IsFloating: true, StartTime: time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)},
				},
			}
			paths := []string{"/cal/x/", "/cal/y/", "/cal/z/", "/cal/allday/", "/cal/floating/"}
			listed := make([]icloud.Calendar, len(paths))
			for i, path := range paths {
				listed[i] = icloud.Calendar{Path: path}
			}
			svc := &availabilityBudgetService{MockService: &icloud.MockService{Calendars: listed}}
			svc.read = func(_ context.Context, path string) (icloud.SearchResult, error) {
				return icloud.SearchResult{Events: byPath[path]}, nil
			}

			opts := availabilityOptions{
				IncludeAllDay: tc.includeAllDay,
				BufferBefore:  tc.bufferBefore,
				BufferAfter:   tc.bufferAfter,
				Location:      tc.loc,
			}
			var allEvents []icloud.Event
			for _, path := range paths {
				allEvents = append(allEvents, byPath[path]...)
			}
			want := icloud.MergeIntervals(icloud.BusyFromEvents(allEvents, opts.IncludeAllDay, opts.BufferBefore, opts.BufferAfter, opts.Location))

			got, err := collectAvailabilityIntervals(t.Context(), paths, svc.read, opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("intervals = %v, want %v", got, want)
			}
			for i := range want {
				if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
					t.Fatalf("interval %d = %v, want %v", i, got[i], want[i])
				}
			}
		})
	}
}

// TestAvailabilityCollectorIntervalBudget covers the explicit interval-count
// budget: the collector refuses an accumulation beyond the event budget instead
// of retaining an unbounded slice.
func TestAvailabilityCollectorIntervalBudget(t *testing.T) {
	svc, paths, _ := availabilityWaveFixture(8, 2)
	original := maxAvailabilityIntervals
	t.Cleanup(func() { maxAvailabilityIntervals = original })
	maxAvailabilityIntervals = 8
	if _, err := collectAvailabilityIntervals(t.Context(), paths, svc.read, availabilityOptions{
		IncludeAllDay: true, Location: time.UTC,
	}); err == nil {
		t.Fatal("collector accepted more intervals than its budget")
	} else if typed := icloud.AsICloudError(err); typed == nil || typed.Code != icloud.CodePayloadTooLarge {
		t.Fatalf("error = %v, want payload_too_large", err)
	}
}

// TestAvailabilityCollectorCancellationDiscardsIntervals proves cancellation
// after some waves still returns no partial availability.
func TestAvailabilityCollectorCancellationDiscardsIntervals(t *testing.T) {
	svc, paths, _ := availabilityWaveFixture(1024, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var reads atomic.Int64
	svc.read = func(ctx context.Context, path string) (icloud.SearchResult, error) {
		if reads.Add(1) == int64(calendarReadConcurrency)+1 {
			cancel()
		}
		return icloud.SearchResult{Events: []icloud.Event{{
			UID:       "late",
			StartTime: mustParseTime("2026-07-01T09:00:00Z"),
			EndTime:   mustParseTime("2026-07-01T10:00:00Z"),
		}}}, nil
	}
	intervals, err := collectAvailabilityIntervals(ctx, paths, svc.read, availabilityOptions{
		IncludeAllDay: true, Location: time.UTC,
	})
	if err == nil {
		t.Fatal("cancellation returned no error")
	}
	if intervals != nil {
		t.Fatalf("cancellation retained intervals: %v", intervals)
	}
}
