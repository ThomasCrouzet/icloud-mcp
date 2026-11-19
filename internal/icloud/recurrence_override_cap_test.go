package icloud

import (
	"testing"
	"time"
)

// overrideCapMove is one detached RECURRENCE-ID override used by the cap
// tests. original is the RECURRENCE-ID slot; moved is the effective start.
type overrideCapMove struct {
	original time.Time
	moved    time.Time
}

func overrideCapEvents(moves []overrideCapMove) []Event {
	out := make([]Event, 0, len(moves))
	for _, move := range moves {
		out = append(out, Event{
			UID:          "uid-cap-override",
			Title:        "Moved",
			StartTime:    move.moved,
			EndTime:      move.moved.Add(time.Hour),
			RecurrenceID: move.original,
		})
	}
	return out
}

func occurrenceStarts(events []Event) []time.Time {
	out := make([]time.Time, len(events))
	for i := range events {
		out[i] = events[i].StartTime.UTC()
	}
	return out
}

// TestExpandOccurrences_OverrideMovedOutOfRangeKeepsValidSlots covers issue 221.
// A detached override moved outside the query window must not consume result
// capacity: the cap applies to the final projected occurrences.
func TestExpandOccurrences_OverrideMovedOutOfRangeKeepsValidSlots(t *testing.T) {
	master := Event{
		UID:        "uid-cap",
		Title:      "Daily",
		StartTime:  mustParse(t, "2026-07-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-07-01T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=4",
	}
	overrides := overrideCapEvents([]overrideCapMove{{
		original: mustParse(t, "2026-07-01T09:00:00Z"),
		moved:    mustParse(t, "2026-07-20T09:00:00Z"),
	}})
	rangeStart := mustParse(t, "2026-07-01T00:00:00Z")
	rangeEnd := mustParse(t, "2026-07-05T00:00:00Z")

	out, truncated, err := ExpandOccurrences(master, overrides, rangeStart, rangeEnd, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []time.Time{
		mustParse(t, "2026-07-02T09:00:00Z"),
		mustParse(t, "2026-07-03T09:00:00Z"),
	}
	if got := occurrenceStarts(out); len(got) != len(want) || !got[0].Equal(want[0]) || !got[1].Equal(want[1]) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
	if !truncated {
		t.Fatal("truncated = false, want true when the projected set exceeds the cap")
	}
}

// TestExpandOccurrences_MovedOutOverrideConsumesNoCapacity proves the cap
// counts only material results: three in-range occurrences with cap three are
// complete.
func TestExpandOccurrences_MovedOutOverrideConsumesNoCapacity(t *testing.T) {
	master := Event{
		UID:        "uid-cap",
		Title:      "Daily",
		StartTime:  mustParse(t, "2026-07-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-07-01T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=4",
	}
	overrides := overrideCapEvents([]overrideCapMove{{
		original: mustParse(t, "2026-07-01T09:00:00Z"),
		moved:    mustParse(t, "2026-07-20T09:00:00Z"),
	}})
	rangeStart := mustParse(t, "2026-07-01T00:00:00Z")
	rangeEnd := mustParse(t, "2026-07-06T00:00:00Z")

	out, truncated, err := ExpandOccurrences(master, overrides, rangeStart, rangeEnd, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []time.Time{
		mustParse(t, "2026-07-02T09:00:00Z"),
		mustParse(t, "2026-07-03T09:00:00Z"),
		mustParse(t, "2026-07-04T09:00:00Z"),
	}
	got := occurrenceStarts(out)
	if len(got) != len(want) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("starts = %v, want %v", got, want)
		}
	}
	if truncated {
		t.Fatal("truncated = true, want false when three material results fit the cap")
	}
}

// TestExpandOccurrences_MovedInOverridesAreDeterministic covers the second 221
// case: overrides whose RECURRENCE-ID is outside the generated series but
// whose effective start is inside the window must be selected by a stable
// order, independent of map iteration and caller slice order.
func TestExpandOccurrences_MovedInOverridesAreDeterministic(t *testing.T) {
	master := Event{
		UID:        "uid-moved-in",
		Title:      "Daily",
		StartTime:  mustParse(t, "2026-06-30T09:00:00Z"),
		EndTime:    mustParse(t, "2026-06-30T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=2",
	}
	// Five occurrences of one other series are moved into the window. The cap
	// keeps only three of them, so the selection order decides the result.
	moves := []overrideCapMove{
		{original: mustParse(t, "2026-05-01T09:00:00Z"), moved: mustParse(t, "2026-07-10T13:00:00Z")},
		{original: mustParse(t, "2026-05-02T09:00:00Z"), moved: mustParse(t, "2026-07-11T09:00:00Z")},
		{original: mustParse(t, "2026-05-03T09:00:00Z"), moved: mustParse(t, "2026-07-10T15:00:00Z")},
		{original: mustParse(t, "2026-05-04T09:00:00Z"), moved: mustParse(t, "2026-07-12T09:00:00Z")},
		{original: mustParse(t, "2026-05-05T09:00:00Z"), moved: mustParse(t, "2026-07-10T09:00:00Z")},
	}
	want := []time.Time{
		mustParse(t, "2026-06-30T09:00:00Z"),
		mustParse(t, "2026-07-01T09:00:00Z"),
		mustParse(t, "2026-07-10T09:00:00Z"),
	}
	rangeStart := mustParse(t, "2026-06-29T00:00:00Z")
	rangeEnd := mustParse(t, "2026-07-13T00:00:00Z")

	permutations := [][]overrideCapMove{
		moves,
		{moves[4], moves[3], moves[2], moves[1], moves[0]},
		{moves[2], moves[0], moves[4], moves[1], moves[3]},
		{moves[3], moves[4], moves[0], moves[2], moves[1]},
		{moves[1], moves[2], moves[3], moves[4], moves[0]},
	}
	for run, permutation := range permutations {
		out, truncated, err := ExpandOccurrences(master, overrideCapEvents(permutation), rangeStart, rangeEnd, 3)
		if err != nil {
			t.Fatalf("run %d: unexpected error: %v", run, err)
		}
		got := occurrenceStarts(out)
		if len(got) != len(want) {
			t.Fatalf("run %d: starts = %v, want %v", run, got, want)
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Fatalf("run %d: starts = %v, want %v", run, got, want)
			}
		}
		if !truncated {
			t.Fatalf("run %d: truncated = false, want true when six results exceed the cap", run)
		}
	}
}

// TestExpandOccurrences_DeterministicOrderUnderRepeatedRuns repeats one
// override set many times, which varies Go map iteration order between runs.
func TestExpandOccurrences_DeterministicOrderUnderRepeatedRuns(t *testing.T) {
	master := Event{
		UID:        "uid-moved-in",
		Title:      "Daily",
		StartTime:  mustParse(t, "2026-06-30T09:00:00Z"),
		EndTime:    mustParse(t, "2026-06-30T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=1",
	}
	moves := make([]overrideCapMove, 0, 8)
	for i := range 8 {
		moves = append(moves, overrideCapMove{
			original: mustParse(t, "2026-05-01T09:00:00Z").Add(time.Duration(i) * 24 * time.Hour),
			moved:    mustParse(t, "2026-07-10T09:00:00Z").Add(time.Duration(i) * time.Hour),
		})
	}
	overrides := overrideCapEvents(moves)
	rangeStart := mustParse(t, "2026-06-29T00:00:00Z")
	rangeEnd := mustParse(t, "2026-07-20T00:00:00Z")

	baseline, _, err := ExpandOccurrences(master, overrides, rangeStart, rangeEnd, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := occurrenceStarts(baseline)
	if len(want) != 4 {
		t.Fatalf("baseline starts = %v", want)
	}
	for run := range 40 {
		out, _, err := ExpandOccurrences(master, overrides, rangeStart, rangeEnd, 4)
		if err != nil {
			t.Fatalf("run %d: unexpected error: %v", run, err)
		}
		got := occurrenceStarts(out)
		if len(got) != len(want) {
			t.Fatalf("run %d: starts = %v, want %v", run, got, want)
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Fatalf("run %d: starts = %v, want %v", run, got, want)
			}
		}
	}
}
