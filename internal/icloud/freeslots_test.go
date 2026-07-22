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

// Europe/Paris DST 2026: spring-forward 29 Mar (02:00->03:00), fall-back 25 Oct (03:00->02:00).

func TestFindFreeSlots_ParisSpringForward_WorkingHoursWallClock(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// Spring-forward day: civil day is 23 hours. Working hours 09:00-17:00 local
	// must keep wall-clock times (not midnight+9h which lands on 10:00 CEST).
	rangeStart := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	rangeEnd := time.Date(2026, 3, 30, 0, 0, 0, 0, loc)
	slots, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart:       rangeStart,
		RangeEnd:         rangeEnd,
		Duration:         time.Hour,
		Location:         loc,
		WorkingHourStart: 9 * 60,
		WorkingHourEnd:   17 * 60,
		Limit:            20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) == 0 {
		t.Fatal("expected free slots on spring-forward day")
	}
	assertSlotsInRangeNoDupNoBusy(t, slots, rangeStart, rangeEnd, nil)
	for _, s := range slots {
		localStart := s.Start.In(loc)
		if localStart.Hour() < 9 || localStart.Hour() >= 17 {
			t.Errorf("slot start wall-clock outside 09-17: %s", localStart)
		}
		// First slot of the day must start at 09:00 local, not 10:00 from Add(9h).
		if localStart.Day() == 29 && localStart.Hour() == 9 && localStart.Minute() == 0 {
			return
		}
	}
	t.Fatalf("expected a slot starting at 09:00 local on spring-forward day; slots=%v", formatSlots(slots, loc))
}

func TestFindFreeSlots_ParisFallBack_CivilDayProgression(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// Fall-back day is 25h. A fixed 24h step from midnight lands at 23:00 same
	// civil day, which can skip or double-count when scanning multi-day ranges.
	rangeStart := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	rangeEnd := time.Date(2026, 10, 27, 0, 0, 0, 0, loc) // through day after fall-back
	slots, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart:       rangeStart,
		RangeEnd:         rangeEnd,
		Duration:         time.Hour,
		Location:         loc,
		WorkingHourStart: 9 * 60,
		WorkingHourEnd:   12 * 60, // three 1h slots per day when no busy
		Limit:            50,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSlotsInRangeNoDupNoBusy(t, slots, rangeStart, rangeEnd, nil)
	// Expect exactly 3 slots/day * 2 days = 6 (25 Oct and 26 Oct).
	if len(slots) != 6 {
		t.Fatalf("got %d slots, want 6 (3 per civil day over 2 days): %v", len(slots), formatSlots(slots, loc))
	}
	// 26 Oct (day after DST) must still start at 09:00 local.
	var saw26 bool
	for _, s := range slots {
		ls := s.Start.In(loc)
		if ls.Month() == time.October && ls.Day() == 26 && ls.Hour() == 9 {
			saw26 = true
		}
		if ls.Hour() < 9 || ls.Hour() >= 12 {
			t.Errorf("slot outside 09-12 local: %s", ls)
		}
	}
	if !saw26 {
		t.Fatalf("missing 09:00 slot on 26 Oct (civil-day after fall-back): %v", formatSlots(slots, loc))
	}
}

func TestFindFreeSlots_ParisNormalWorkingHours(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// Non-DST transition mid-summer: wall-clock working hours preserved.
	rangeStart := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	rangeEnd := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	busyStart := time.Date(2026, 7, 1, 10, 0, 0, 0, loc)
	busy := []Interval{{Start: busyStart, End: busyStart.Add(time.Hour)}}
	slots, err := FindFreeSlots(busy, FreeSlotOptions{
		RangeStart:       rangeStart,
		RangeEnd:         rangeEnd,
		Duration:         time.Hour,
		Location:         loc,
		WorkingHourStart: 9 * 60,
		WorkingHourEnd:   12 * 60,
		Limit:            20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSlotsInRangeNoDupNoBusy(t, slots, rangeStart, rangeEnd, busy)
	// Free: 09-10 and 11-12 (10-11 busy) => 2 slots.
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2: %v", len(slots), formatSlots(slots, loc))
	}
	if slots[0].Start.In(loc).Hour() != 9 || slots[1].Start.In(loc).Hour() != 11 {
		t.Fatalf("unexpected starts: %v", formatSlots(slots, loc))
	}
}

func TestFindFreeSlots_ParisCrossMidnightWorkingHours(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// Overnight window 22:00-06:00 across a normal night (no DST edge).
	rangeStart := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	rangeEnd := time.Date(2026, 7, 2, 12, 0, 0, 0, loc)
	slots, err := FindFreeSlots(nil, FreeSlotOptions{
		RangeStart:       rangeStart,
		RangeEnd:         rangeEnd,
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
	assertSlotsInRangeNoDupNoBusy(t, slots, rangeStart, rangeEnd, nil)
	for _, s := range slots {
		h := s.Start.In(loc).Hour()
		if h >= 6 && h < 22 {
			t.Errorf("slot start hour %d outside overnight window: %s", h, s.Start.In(loc))
		}
	}
}

func TestNextLocalCivilDay_DSTTransitions(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// Spring-forward: +24h is wrong (lands 01:00 next day); civil next is midnight.
	spring := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	next := nextLocalCivilDay(spring, loc)
	if next.In(loc).Hour() != 0 || next.In(loc).Day() != 30 {
		t.Errorf("spring next = %v", next.In(loc))
	}
	if spring.Add(24 * time.Hour).Equal(next) {
		t.Fatal("unexpected: +24h equals civil next on spring-forward (test env?)")
	}
	// Fall-back: +24h lands 23:00 same civil day.
	fall := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	nextF := nextLocalCivilDay(fall, loc)
	if nextF.In(loc).Day() != 26 || nextF.In(loc).Hour() != 0 {
		t.Errorf("fall next = %v", nextF.In(loc))
	}
	if fall.Add(24 * time.Hour).Equal(nextF) {
		t.Fatal("unexpected: +24h equals civil next on fall-back (test env?)")
	}
}

func TestLocalTimeOnDay_SpringForwardWallClock(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	nine := localTimeOnDay(day, 9*60, loc)
	if nine.In(loc).Hour() != 9 || nine.In(loc).Minute() != 0 {
		t.Fatalf("wall 09:00 got %v", nine.In(loc))
	}
	// Broken Add(9h) would yield 10:00 CEST on this day.
	broken := day.Add(9 * time.Hour)
	if broken.In(loc).Hour() == 9 {
		t.Skip("environment does not exhibit spring-forward Add skew")
	}
	if broken.Equal(nine) {
		t.Fatal("localTimeOnDay must not equal midnight.Add(9h) on spring-forward")
	}
}

func assertSlotsInRangeNoDupNoBusy(t *testing.T, slots []Interval, rangeStart, rangeEnd time.Time, busy []Interval) {
	t.Helper()
	merged := MergeIntervals(busy)
	seen := map[string]bool{}
	for i, s := range slots {
		if !s.End.After(s.Start) {
			t.Fatalf("non-positive slot %+v", s)
		}
		if s.Start.Before(rangeStart) || s.End.After(rangeEnd) {
			t.Fatalf("slot outside range: %+v", s)
		}
		key := s.Start.UTC().Format(time.RFC3339) + ".." + s.End.UTC().Format(time.RFC3339)
		if seen[key] {
			t.Fatalf("duplicate slot %s", key)
		}
		seen[key] = true
		if i > 0 && s.Start.Before(slots[i-1].Start) {
			t.Fatalf("slots not ordered by start: %v then %v", slots[i-1].Start, s.Start)
		}
		for _, b := range merged {
			if s.Start.Before(b.End) && s.End.After(b.Start) {
				t.Fatalf("slot %+v overlaps busy %+v", s, b)
			}
		}
	}
}

func formatSlots(slots []Interval, loc *time.Location) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.Start.In(loc).Format("2006-01-02 15:04") + "->" + s.End.In(loc).Format("15:04")
	}
	return out
}
