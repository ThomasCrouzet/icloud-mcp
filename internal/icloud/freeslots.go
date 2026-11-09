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
	// and both non-zero, hours cross midnight (e.g. 22:00-06:00) as one
	// continuous window [startDay@Start, nextCivilDay@End).
	WorkingHourStart int
	WorkingHourEnd   int
	// DaysOfWeek: empty = all days. Sunday=0 ... Saturday=6 (time.Weekday).
	// For overnight working hours, the filter applies to the civil day the
	// shift starts (e.g. Monday + 22:00-06:00 => Monday 22:00 through Tuesday 06:00).
	DaysOfWeek []time.Weekday
	// BufferBefore / BufferAfter expand each busy interval before merge.
	BufferBefore time.Duration
	BufferAfter  time.Duration
	// IncludeAllDayBusy: when false, all-day events are ignored as busy.
	IncludeAllDayBusy bool
	// Limit caps the number of free slots returned (default 50, max 200).
	Limit int
	// ProbeExtra asks the calculation to stop one slot after Limit, so a
	// caller can report truncation without materializing every later slot.
	ProbeExtra bool
}

// BusyFromEvents converts events into busy intervals, ignoring TRANSPARENT
// and CANCELLED events. DATE-valued all-day events are rebuilt as civil dates
// in loc before buffers are applied. Event titles and notes are never retained.
func BusyFromEvents(events []Event, includeAllDay bool, bufferBefore, bufferAfter time.Duration, loc *time.Location) []Interval {
	if loc == nil {
		loc = time.UTC
	}
	out := make([]Interval, 0, len(events))
	for _, e := range events {
		if isTransparentOrCancelled(e) {
			continue
		}
		if e.AllDay && !includeAllDay {
			continue
		}
		start := e.StartTime
		end := e.EndTime
		if e.AllDay {
			start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
			end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, loc)
		} else if e.IsFloating {
			// A floating value has no instant. For availability in the
			// requested zone, interpret its wall clock there. The event stays
			// floating in every Calendar result.
			start = WallClockInLocation(start, loc)
			end = WallClockInLocation(end, loc)
		}
		start = start.Add(-bufferBefore)
		end = end.Add(bufferAfter)
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
	if opts.WorkingHourStart < 0 || opts.WorkingHourStart > 24*60 || opts.WorkingHourEnd < 0 || opts.WorkingHourEnd > 24*60 {
		return nil, NewValidationError("working hours must be between 00:00 and 24:00")
	}
	for _, day := range opts.DaysOfWeek {
		if day < time.Sunday || day > time.Saturday {
			return nil, NewValidationError("days of week must be between Sunday and Saturday")
		}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	maximum := 200
	if opts.ProbeExtra {
		maximum = 201
	}
	if limit > maximum {
		limit = maximum
	}
	loc := ResolveLocation(opts.Location)

	merged := MergeIntervals(busy)
	// Invert busy into free within the range.
	free := invertBusy(opts.RangeStart, opts.RangeEnd, merged)
	// Clip to working hours / days.
	free, err := clipWorkingHours(free, opts, loc)
	if err != nil {
		return nil, err
	}
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

// civilDate is a calendar label independent of any zone offset. Iterating by
// civil date guarantees progress even when local midnight does not exist.
type civilDate struct {
	year  int
	month time.Month
	day   int
}

func civilDateOf(t time.Time, loc *time.Location) civilDate {
	lt := t.In(loc)
	return civilDate{year: lt.Year(), month: lt.Month(), day: lt.Day()}
}

func (d civilDate) next() civilDate {
	t := time.Date(d.year, d.month, d.day+1, 12, 0, 0, 0, time.UTC)
	return civilDate{year: t.Year(), month: t.Month(), day: t.Day()}
}

func (d civilDate) previous() civilDate {
	t := time.Date(d.year, d.month, d.day-1, 12, 0, 0, 0, time.UTC)
	return civilDate{year: t.Year(), month: t.Month(), day: t.Day()}
}

func (d civilDate) weekday() time.Weekday {
	return time.Date(d.year, d.month, d.day, 12, 0, 0, 0, time.UTC).Weekday()
}

func (d civilDate) after(other civilDate) bool {
	if d.year != other.year {
		return d.year > other.year
	}
	if d.month != other.month {
		return d.month > other.month
	}
	return d.day > other.day
}

// civilDayStart returns the first representable instant whose local civil date
// is d. When midnight is missing, Go normalizes time.Date to the previous day,
// so scan forward to the actual transition instead of looping on that value.
func civilDayStart(d civilDate, loc *time.Location) time.Time {
	midnight := time.Date(d.year, d.month, d.day, 0, 0, 0, 0, loc)
	for minute := 0; minute <= 180; minute++ {
		candidate := midnight.Add(time.Duration(minute) * time.Minute)
		local := candidate.In(loc)
		if local.Year() == d.year && local.Month() == d.month && local.Day() == d.day {
			return candidate
		}
	}
	return midnight
}

func localTimeOnCivilDay(d civilDate, minutesFromMidnight int, loc *time.Location) time.Time {
	day := civilDayStart(d, loc).In(loc)
	year, month, date := day.Date()
	if minutesFromMidnight >= 24*60 {
		next := d.next()
		return civilDayStart(next, loc)
	}
	return time.Date(year, month, date, minutesFromMidnight/60, minutesFromMidnight%60, 0, 0, loc)
}

const maxFreeSlotCivilDays = 100_000

func clipWorkingHours(free []Interval, opts FreeSlotOptions, loc *time.Location) ([]Interval, error) {
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
	overnight := useWH && whEnd <= whStart
	work := 0

	var out []Interval
	for _, win := range free {
		day := civilDateOf(win.Start, loc)
		if overnight {
			day = day.previous()
		}
		endDate := civilDateOf(win.End, loc)
		for !day.after(endDate) {
			work++
			if work > maxFreeSlotCivilDays {
				return nil, NewError(CodePayloadTooLarge, 0, "free-slot civil-day work exceeded its bound", nil)
			}
			dayStart := civilDayStart(day, loc)
			nextStart := civilDayStart(day.next(), loc)
			if !dayFilter[day.weekday()] {
				day = day.next()
				continue
			}
			var segments []Interval
			switch {
			case !useWH:
				segments = []Interval{{Start: dayStart, End: nextStart}}
			case !overnight:
				segments = []Interval{{
					Start: localTimeOnCivilDay(day, whStart, loc),
					End:   localTimeOnCivilDay(day, whEnd, loc),
				}}
			default:
				segments = []Interval{{
					Start: localTimeOnCivilDay(day, whStart, loc),
					End:   localTimeOnCivilDay(day.next(), whEnd, loc),
				}}
			}
			for _, seg := range segments {
				inter := intersect(win, seg)
				if inter.End.After(inter.Start) {
					out = append(out, inter)
				}
			}
			day = day.next()
		}
	}
	return MergeIntervals(out), nil
}

// nextLocalCivilDay returns local midnight of the calendar day after day.
// day is expected to be a local midnight; progression uses civil date
// arithmetic so spring-forward (23h) and fall-back (25h) days are correct.
func nextLocalCivilDay(day time.Time, loc *time.Location) time.Time {
	lt := day.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day()+1, 0, 0, 0, 0, loc)
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
	if len(s) != len("HH:MM") || s[2] != ':' ||
		s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' ||
		s[3] < '0' || s[3] > '9' || s[4] < '0' || s[4] > '9' {
		return 0, NewValidationError(fmt.Sprintf("invalid working hours %q: use HH:MM", s))
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 24 || m > 59 || (h == 24 && m != 0) {
		return 0, NewValidationError(fmt.Sprintf("invalid working hours %q", s))
	}
	return h*60 + m, nil
}
