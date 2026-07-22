package icloud

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Interval is a half-open time range [Start, End).
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// FreeSlotOptions configures FindFreeSlots.
type FreeSlotOptions struct {
	// RangeStart / RangeEnd bound the search window (required, end > start).
	RangeStart time.Time
	RangeEnd   time.Time
	// Duration is the required free slot length (must be > 0).
	Duration time.Duration
	// Location for working-hours interpretation (nil = UTC).
	Location *time.Location
	// WorkingHourStart / WorkingHourEnd are minutes from local midnight
	// [0, 1440]. When both zero, the whole day is allowed. When End <= Start
	// and both non-zero, hours cross midnight (e.g. 22:00-06:00).
	WorkingHourStart int
	WorkingHourEnd   int
	// DaysOfWeek: empty = all days. Sunday=0 ... Saturday=6 (time.Weekday).
	DaysOfWeek []time.Weekday
	// BufferBefore / BufferAfter expand each busy interval before merge.
	BufferBefore time.Duration
	BufferAfter  time.Duration
	// IncludeAllDayBusy: when false, all-day events are ignored as busy.
	IncludeAllDayBusy bool
	// Limit caps the number of free slots returned (default 50, max 200).
	Limit int
}

// BusyFromEvents converts events into busy intervals, ignoring TRANSPARENT
// and CANCELLED events. Event titles and notes are never retained.
func BusyFromEvents(events []Event, includeAllDay bool, bufferBefore, bufferAfter time.Duration) []Interval {
	out := make([]Interval, 0, len(events))
	for _, e := range events {
		if isTransparentOrCancelled(e) {
			continue
		}
		if e.AllDay && !includeAllDay {
			continue
		}
		start := e.StartTime.Add(-bufferBefore)
		end := e.EndTime.Add(bufferAfter)
		if !end.After(start) {
			continue
		}
		out = append(out, Interval{Start: start, End: end})
	}
	return out
}

func isTransparentOrCancelled(e Event) bool {
	return strings.EqualFold(e.Status, "CANCELLED") || strings.EqualFold(e.Transp, "TRANSPARENT")
}

// MergeIntervals merges overlapping or adjacent half-open intervals.
func MergeIntervals(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	sorted := make([]Interval, len(in))
	copy(sorted, in)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start.Equal(sorted[j].Start) {
			return sorted[i].End.Before(sorted[j].End)
		}
		return sorted[i].Start.Before(sorted[j].Start)
	})
	out := []Interval{sorted[0]}
	for _, cur := range sorted[1:] {
		last := &out[len(out)-1]
		// Adjacent (cur.Start == last.End) merges: no free gap of zero length.
		if !cur.Start.After(last.End) {
			if cur.End.After(last.End) {
				last.End = cur.End
			}
			continue
		}
		out = append(out, cur)
	}
	return out
}

// FindFreeSlots computes free intervals of at least opts.Duration within
// [RangeStart, RangeEnd), after subtracting merged busy intervals and
// applying working hours / days-of-week filters. It never returns event
// titles or any busy-event identity.
func FindFreeSlots(busy []Interval, opts FreeSlotOptions) ([]Interval, error) {
	if !opts.RangeEnd.After(opts.RangeStart) {
		return nil, NewValidationError("free-slot range end must be after start")
	}
	if opts.Duration <= 0 {
		return nil, NewValidationError("duration_minutes must be positive")
	}
	if opts.Duration > opts.RangeEnd.Sub(opts.RangeStart) {
		return nil, NewValidationError("requested duration is longer than the search range")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	loc := ResolveLocation(opts.Location)

	merged := MergeIntervals(busy)
	// Invert busy into free within the range.
	free := invertBusy(opts.RangeStart, opts.RangeEnd, merged)
	// Clip to working hours / days.
	free = clipWorkingHours(free, opts, loc)
	// Keep only slots long enough; emit fixed-duration slots greedily from
	// each free window start (non-overlapping consecutive placements).
	out := make([]Interval, 0, limit)
	for _, win := range free {
		cursor := win.Start
		for {
			slotEnd := cursor.Add(opts.Duration)
			if slotEnd.After(win.End) {
				break
			}
			out = append(out, Interval{Start: cursor, End: slotEnd})
			if len(out) >= limit {
				return out, nil
			}
			cursor = slotEnd
		}
	}
	return out, nil
}

func invertBusy(rangeStart, rangeEnd time.Time, busy []Interval) []Interval {
	var free []Interval
	cursor := rangeStart
	for _, b := range busy {
		// Clip busy to range.
		bs, be := b.Start, b.End
		if be.Before(rangeStart) || !be.After(rangeStart) {
			continue
		}
		if !bs.Before(rangeEnd) {
			break
		}
		if bs.Before(rangeStart) {
			bs = rangeStart
		}
		if be.After(rangeEnd) {
			be = rangeEnd
		}
		if bs.After(cursor) {
			free = append(free, Interval{Start: cursor, End: bs})
		}
		if be.After(cursor) {
			cursor = be
		}
	}
	if cursor.Before(rangeEnd) {
		free = append(free, Interval{Start: cursor, End: rangeEnd})
	}
	return free
}

func clipWorkingHours(free []Interval, opts FreeSlotOptions, loc *time.Location) []Interval {
	dayFilter := map[time.Weekday]bool{}
	if len(opts.DaysOfWeek) == 0 {
		for d := time.Sunday; d <= time.Saturday; d++ {
			dayFilter[d] = true
		}
	} else {
		for _, d := range opts.DaysOfWeek {
			dayFilter[d] = true
		}
	}
	whStart := opts.WorkingHourStart
	whEnd := opts.WorkingHourEnd
	useWH := whStart != 0 || whEnd != 0
	if useWH {
		if whStart < 0 || whStart > 24*60 || whEnd < 0 || whEnd > 24*60 {
			// Invalid hours: treat as no working-hours filter (caller should validate).
			useWH = false
		}
	}

	var out []Interval
	for _, win := range free {
		// Walk day by day in local time.
		day := startOfLocalDay(win.Start.In(loc), loc)
		endLocal := win.End.In(loc)
		for !day.After(endLocal) {
			if !dayFilter[day.Weekday()] {
				day = day.Add(24 * time.Hour)
				continue
			}
			var segments []Interval
			if !useWH {
				segments = []Interval{{Start: day, End: day.Add(24 * time.Hour)}}
			} else if whEnd > whStart {
				segments = []Interval{{
					Start: day.Add(time.Duration(whStart) * time.Minute),
					End:   day.Add(time.Duration(whEnd) * time.Minute),
				}}
			} else {
				// Cross-midnight: [whStart, 24h) U [0, whEnd).
				segments = []Interval{
					{Start: day.Add(time.Duration(whStart) * time.Minute), End: day.Add(24 * time.Hour)},
					{Start: day, End: day.Add(time.Duration(whEnd) * time.Minute)},
				}
			}
			for _, seg := range segments {
				inter := intersect(win, seg)
				if inter.End.After(inter.Start) {
					out = append(out, inter)
				}
			}
			day = day.Add(24 * time.Hour)
		}
	}
	return MergeIntervals(out)
}

func startOfLocalDay(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
}

func intersect(a, b Interval) Interval {
	start := a.Start
	if b.Start.After(start) {
		start = b.Start
	}
	end := a.End
	if b.End.Before(end) {
		end = b.End
	}
	return Interval{Start: start, End: end}
}

// ParseWorkingHours parses "HH:MM" into minutes from midnight.
func ParseWorkingHours(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, NewValidationError(fmt.Sprintf("invalid working hours %q: use HH:MM", s))
	}
	if h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, NewValidationError(fmt.Sprintf("invalid working hours %q", s))
	}
	return h*60 + m, nil
}
