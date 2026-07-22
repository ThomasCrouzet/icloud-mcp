package icloud

import (
	"math/rand"
	"testing"
	"time"
)

func TestMergeIntervals_OverlapsAndAdjacent(t *testing.T) {
	base := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	in := []Interval{
		{Start: base, End: base.Add(time.Hour)},
		{Start: base.Add(30 * time.Minute), End: base.Add(2 * time.Hour)},
		{Start: base.Add(2 * time.Hour), End: base.Add(3 * time.Hour)}, // adjacent
		{Start: base.Add(5 * time.Hour), End: base.Add(6 * time.Hour)},
	}
	out := MergeIntervals(in)
	if len(out) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(out), out)
	}
	if !out[0].Start.Equal(base) || !out[0].End.Equal(base.Add(3*time.Hour)) {
		t.Errorf("first merge = %+v", out[0])
	}
}

func TestFindFreeSlots_NoOverlapWithBusy(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	busy := []Interval{
		{Start: day.Add(10 * time.Hour), End: day.Add(11 * time.Hour)},
	}
	slots, err := FindFreeSlots(busy, FreeSlotOptions{
		RangeStart: day.Add(9 * time.Hour),
		RangeEnd:   day.Add(12 * time.Hour),
		Duration:   time.Hour,
		Limit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slots {
		if s.Start.Before(busy[0].End) && s.End.After(busy[0].Start) {
			t.Errorf("slot %+v overlaps busy", s)
		}
		if s.End.Sub(s.Start) != time.Hour {
			t.Errorf("slot duration = %v", s.End.Sub(s.Start))
		}
	}
	if len(slots) < 1 {
		t.Fatal("expected at least one free slot")
	}
}

func TestFindFreeSlots_IgnoresTransparentAndCancelled(t *testing.T) {
	day := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	events := []Event{
		{StartTime: day, EndTime: day.Add(time.Hour), Title: "busy"},
		{StartTime: day.Add(time.Hour), EndTime: day.Add(2 * time.Hour), Transp: "TRANSPARENT", Title: "free"},
		{StartTime: day.Add(2 * time.Hour), EndTime: day.Add(3 * time.Hour), Status: "CANCELLED", Title: "gone"},
	}
	busy := BusyFromEvents(events, true, 0, 0)
	if len(busy) != 1 {
		t.Fatalf("busy count = %d, want 1 (only opaque non-cancelled)", len(busy))
	}
}

func TestFindFreeSlots_ImpossibleDuration(t *testing.T) {
	day := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	_, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart: day,
		RangeEnd:   day.Add(30 * time.Minute),
		Duration:   time.Hour,
	})
	if err == nil {
		t.Fatal("expected validation error for duration > range")
	}
}

func TestFindFreeSlots_WorkingHoursCrossMidnight(t *testing.T) {
	// Night shift 22:00-06:00 local.
	loc := time.UTC
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	slots, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart:       day,
		RangeEnd:         day.Add(24 * time.Hour),
		Duration:         time.Hour,
		Location:         loc,
		WorkingHourStart: 22 * 60,
		WorkingHourEnd:   6 * 60,
		Limit:            50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) == 0 {
		t.Fatal("expected overnight free slots")
	}
	for _, s := range slots {
		h := s.Start.In(loc).Hour()
		if h >= 6 && h < 22 {
			t.Errorf("slot start hour %d outside overnight window: %+v", h, s)
		}
	}
}

func TestFindFreeSlots_GenerativeNoBusyOverlap(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for seed := 0; seed < 50; seed++ {
		r.Seed(int64(seed + 42))
		var busy []Interval
		cursor := base
		for i := 0; i < 5; i++ {
			gap := time.Duration(r.Intn(120)) * time.Minute
			dur := time.Duration(30+r.Intn(90)) * time.Minute
			start := cursor.Add(gap)
			end := start.Add(dur)
			busy = append(busy, Interval{Start: start, End: end})
			cursor = end
		}
		rangeEnd := base.Add(24 * time.Hour)
		slots, err := FindFreeSlots(busy, FreeSlotOptions{
			RangeStart: base,
			RangeEnd:   rangeEnd,
			Duration:   30 * time.Minute,
			Limit:      20,
		})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		merged := MergeIntervals(busy)
		for _, s := range slots {
			if !s.End.After(s.Start) {
				t.Fatalf("seed %d: non-positive slot %+v", seed, s)
			}
			if s.Start.Before(base) || s.End.After(rangeEnd) {
				t.Fatalf("seed %d: slot outside range %+v", seed, s)
			}
			for _, b := range merged {
				if s.Start.Before(b.End) && s.End.After(b.Start) {
					t.Fatalf("seed %d: slot %+v overlaps busy %+v", seed, s, b)
				}
			}
		}
	}
}

func TestBusyFromEvents_AllDayOptional(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	events := []Event{{StartTime: day, EndTime: day.Add(24 * time.Hour), AllDay: true, Title: "holiday"}}
	if n := len(BusyFromEvents(events, false, 0, 0)); n != 0 {
		t.Errorf("all-day excluded: got %d", n)
	}
	if n := len(BusyFromEvents(events, true, 0, 0)); n != 1 {
		t.Errorf("all-day included: got %d", n)
	}
}
