package icloud

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// maxOccurrencesPerSeries bounds the number of occurrences expanded for a
// single recurring series, as protection against a pathological RRULE (e.g.
// FREQ=MINUTELY without UNTIL/COUNT over a 366-day range).
const maxOccurrencesPerSeries = 2000

// maxRecurrenceExpansionWork bounds iterator advancement, including
// occurrences before the requested window. This matters because rrule-go's
// iterator always starts at DTSTART and has no constant-time seek operation.
const maxRecurrenceExpansionWork int64 = 100000

// maxRecurrenceSearchWork bounds iterator advancement across all series in one
// Calendar search. The per-series bound still applies independently.
const maxRecurrenceSearchWork int64 = 250000

// ExpandOccurrences expands a recurring event within [rangeStart, rangeEnd].
// Handles RRULE + EXDATE (exclusions); RECURRENCE-ID overrides replace the
// matching generated occurrence (compared at second precision, in UTC);
// overrides falling inside the range but absent from the generated series
// (moved out of their original slot) are still included.
// maxOccurrences bounds the expansion; if <= 0, the package default is used.
// truncated is true when the per-series cap dropped occurrences that the
// RRULE would otherwise have produced inside the widened selection window.
func ExpandOccurrences(master Event, overrides []Event, rangeStart, rangeEnd time.Time, maxOccurrences int) (events []Event, truncated bool, err error) {
	return ExpandOccurrencesContext(context.Background(), master, overrides, rangeStart, rangeEnd, maxOccurrences)
}

// ExpandOccurrencesContext is the cancellation-aware form of
// ExpandOccurrences. It preflights the RRULE's bounded work estimate and then
// advances at most one occurrence at a time, retaining only cap+1 entries.
func ExpandOccurrencesContext(ctx context.Context, master Event, overrides []Event, rangeStart, rangeEnd time.Time, maxOccurrences int) (events []Event, truncated bool, err error) {
	remainingWork := maxRecurrenceSearchWork
	return expandOccurrencesContext(ctx, master, overrides, rangeStart, rangeEnd, maxOccurrences, &remainingWork)
}

func expandOccurrencesContext(ctx context.Context, master Event, overrides []Event, rangeStart, rangeEnd time.Time, maxOccurrences int, remainingWork *int64) (events []Event, truncated bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, calendarContextError(err)
	}
	if maxOccurrences <= 0 || maxOccurrences > maxOccurrencesPerSeries {
		maxOccurrences = maxOccurrencesPerSeries
	}
	if err := validateReadEventFields(master); err != nil {
		return nil, false, err
	}
	if len(overrides) > maxRemoteOverrides {
		return nil, false, NewError(CodePayloadTooLarge, 0, "Calendar event data exceeded its override limit", nil)
	}
	for i := range overrides {
		if err := validateReadEventFields(overrides[i]); err != nil {
			return nil, false, err
		}
	}

	if master.Recurrence == "" {
		if eventOverlaps(master, rangeStart, rangeEnd) {
			return []Event{master}, false, nil
		}
		return nil, false, nil
	}

	ropt, err := rrule.StrToROption(strings.ToUpper(master.Recurrence))
	if err != nil {
		return nil, false, NewError(CodeProtocolError, 0, "Calendar recurrence rule is invalid", nil)
	}
	if !normalizeRecurrenceSelectorLists(ropt) {
		return nil, false, NewError(CodePayloadTooLarge, 0, "Calendar recurrence rule has excessive selector cardinality", nil)
	}
	// Do NOT force .UTC() here: RFC 5545 requires the recurrence to follow
	// the local WALL CLOCK time of the Dtstart (TZID), not a fixed UTC
	// instant. Converting to UTC would destroy the Location and pin every
	// occurrence to the original Dtstart's UTC offset, shifting it by 1h
	// from the expected wall clock time as soon as a DST change happens in
	// between. If the event is already in Z (UTC), StartTime.Location() is
	// already time.UTC and no information is lost.
	ropt.Dtstart = master.StartTime
	seriesRemaining := maxRecurrenceExpansionWork
	safety, maxEmptyPeriods, maxPeriodCandidates := checkRecurrenceSelectorSafety(ctx, ropt, &seriesRemaining, remainingWork)
	switch safety {
	case recurrenceSelectorsCanceled:
		return nil, false, calendarContextError(ctx.Err())
	case recurrenceSelectorsUnsupported:
		return nil, false, NewError(CodeProtocolError, 0, "Calendar recurrence rule uses an unsupported unsafe selector combination", nil)
	case recurrenceSelectorsUnreachable:
		return nil, false, NewError(CodeProtocolError, 0, "Calendar recurrence rule has unreachable date selectors", nil)
	case recurrenceSelectorsExcessive:
		return nil, false, NewError(CodePayloadTooLarge, 0, "Calendar recurrence rule requires excessive internal selector work", nil)
	}
	// Preflight estimate only; actual iterator steps debit seriesRemaining and
	// remainingWork below so under-estimates cannot blow the aggregate budget.
	estimatedWork := recurrenceWorkEstimate(ropt, rangeEnd, maxEmptyPeriods, maxPeriodCandidates)
	if estimatedWork > seriesRemaining || remainingWork == nil || estimatedWork > *remainingWork {
		return nil, false, NewError(CodePayloadTooLarge, 0, "Calendar recurrence rule requires excessive expansion work", nil)
	}

	rule, err := rrule.NewRRule(*ropt)
	if err != nil {
		return nil, false, NewError(CodeProtocolError, 0, "Calendar recurrence rule is invalid", nil)
	}

	excluded := make(map[int64]struct{}, len(master.exDates))
	for _, ex := range master.exDates {
		excluded[ex.UTC().Unix()] = struct{}{}
	}

	// duration MUST be computed before iteration: it is used to widen the
	// lower bound (see below) so occurrences starting before rangeStart
	// but spilling into the range are not lost. Clamp negative/zero durations
	// from corrupt End < Start data so the lower bound is never widened the
	// wrong way.
	duration := master.EndTime.Sub(master.StartTime)
	if duration < 0 {
		duration = 0
	}

	// Lower bound widened by `duration`: an occurrence starting before
	// rangeStart can still overlap the range if it spills into it (e.g. an
	// overnight 22:00 to 02:00 slot). RRULE filtering is by START time;
	// without this widening, those occurrences would be lost even though the
	// non-recurring path (eventOverlaps) would include them. The iterator
	// deliberately admits an occurrence exactly at rangeEnd; eventOverlaps
	// restores the half-open [rangeStart, rangeEnd) semantics used elsewhere.
	overlapDuration := duration
	if master.hasNominalDuration {
		nominalElapsed := time.Duration(master.nominalDurationDays)*24*time.Hour + master.nominalDurationRemainder
		if nominalElapsed <= time.Duration(1<<63-1)-24*time.Hour {
			nominalElapsed += 24 * time.Hour
		}
		if nominalElapsed > overlapDuration {
			overlapDuration = nominalElapsed
		}
	}
	lowerBound := rangeStart.Add(-overlapDuration)
	next := rule.Iterator()
	occTimes := make([]time.Time, 0, min(maxOccurrences+1, 64))
	var advances int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, calendarContextError(err)
		}
		if advances >= maxRecurrenceExpansionWork || seriesRemaining <= 0 || *remainingWork <= 0 {
			return nil, false, NewError(CodePayloadTooLarge, 0, "Calendar recurrence rule exceeded its expansion-work limit", nil)
		}
		occ, ok := next()
		advances++
		seriesRemaining--
		*remainingWork--
		if !ok || occ.After(rangeEnd) {
			break
		}
		if occ.Before(lowerBound) {
			continue
		}
		if _, skip := excluded[occ.UTC().Unix()]; skip {
			continue
		}
		if !eventOverlaps(Event{StartTime: occ, EndTime: recurrenceOccurrenceEnd(master, occ)}, rangeStart, rangeEnd) {
			continue
		}
		occTimes = append(occTimes, occ)
		if len(occTimes) == maxOccurrences+1 {
			occTimes = occTimes[:maxOccurrences]
			truncated = true
			break
		}
	}

	overrideByRecID := make(map[int64]Event, len(overrides))
	for _, o := range overrides {
		ov := o
		ov.Recurrence = "" // never present an override as a series master
		ov.IsOverride = true
		overrideByRecID[o.RecurrenceID.UTC().Unix()] = ov
	}
	used := make(map[int64]bool, len(overrideByRecID))

	out := make([]Event, 0, len(occTimes))
	for _, occ := range occTimes {
		// Keep only occurrences that genuinely overlap
		// [rangeStart, rangeEnd) once rebuilt with their full duration; the
		// widened lower bound above deliberately over-selects, and this
		// filter restores the exact eventOverlaps semantics (consistent
		// with the non-recurring path).
		if !eventOverlaps(Event{StartTime: occ, EndTime: recurrenceOccurrenceEnd(master, occ)}, rangeStart, rangeEnd) {
			continue
		}
		key := occ.UTC().Unix()
		if ov, ok := overrideByRecID[key]; ok {
			used[key] = true
			if eventOverlaps(ov, rangeStart, rangeEnd) {
				out = append(out, ov)
			}
			continue
		}
		clone := master
		clone.StartTime = occ
		clone.EndTime = recurrenceOccurrenceEnd(master, occ)
		clone.exDates = nil
		// Expanded row is not a master: clear RRULE and expose the original
		// slot as recurrenceId so agents can target scope=occurrence.
		clone.Recurrence = ""
		clone.RecurrenceID = occ
		clone.IsOverride = false
		out = append(out, clone)
	}

	// Overrides may fall inside the range while their original series slot
	// does not, for example after an occurrence is moved.
	for key, ov := range overrideByRecID {
		if used[key] {
			continue
		}
		if eventOverlaps(ov, rangeStart, rangeEnd) {
			out = append(out, ov)
		}
	}

	if len(out) > maxOccurrences {
		out = out[:maxOccurrences]
		truncated = true
	}

	return out, truncated, nil
}

// recurrenceClockSelectorSafety proves that rrule-go's high-frequency clock
// loop can reach an allowed time and charges the finite modular proof to both
// recurrence budgets.
func recurrenceClockSelectorSafety(ctx context.Context, opt *rrule.ROption, seriesRemaining, aggregateRemaining *int64) (recurrenceSelectorSafety, int) {
	if opt == nil {
		return recurrenceSelectorsUnreachable, 0
	}
	interval := opt.Interval
	if interval < 1 {
		interval = 1
	}

	var cycle, start int
	var matches func(int) bool
	switch opt.Freq {
	case rrule.HOURLY:
		if len(opt.Byhour) == 0 {
			return recurrenceSelectorsSafe, 0
		}
		cycle = 24
		start = opt.Dtstart.Hour()
		matches = func(value int) bool {
			return intSliceContains(opt.Byhour, value)
		}
	case rrule.MINUTELY:
		if len(opt.Byhour) == 0 && len(opt.Byminute) == 0 {
			return recurrenceSelectorsSafe, 0
		}
		if len(opt.Byhour) == 0 {
			cycle = 60
			start = opt.Dtstart.Minute()
		} else {
			cycle = 24 * 60
			start = opt.Dtstart.Hour()*60 + opt.Dtstart.Minute()
		}
		matches = func(value int) bool {
			hour := value / 60
			if cycle == 60 {
				hour = opt.Dtstart.Hour()
			}
			return (len(opt.Byhour) == 0 || intSliceContains(opt.Byhour, hour)) &&
				(len(opt.Byminute) == 0 || intSliceContains(opt.Byminute, value%60))
		}
	case rrule.SECONDLY:
		if len(opt.Byhour) == 0 && len(opt.Byminute) == 0 && len(opt.Bysecond) == 0 {
			return recurrenceSelectorsSafe, 0
		}
		switch {
		case len(opt.Byhour) != 0:
			cycle = 24 * 60 * 60
			start = opt.Dtstart.Hour()*3600 + opt.Dtstart.Minute()*60 + opt.Dtstart.Second()
		case len(opt.Byminute) != 0:
			cycle = 60 * 60
			start = opt.Dtstart.Minute()*60 + opt.Dtstart.Second()
		default:
			cycle = 60
			start = opt.Dtstart.Second()
		}
		matches = func(value int) bool {
			hour, minute := value/3600, (value/60)%60
			if cycle < 24*60*60 {
				hour = opt.Dtstart.Hour()
			}
			if cycle == 60 {
				minute = opt.Dtstart.Minute()
			}
			return (len(opt.Byhour) == 0 || intSliceContains(opt.Byhour, hour)) &&
				(len(opt.Byminute) == 0 || intSliceContains(opt.Byminute, minute)) &&
				(len(opt.Bysecond) == 0 || intSliceContains(opt.Bysecond, value%60))
		}
	default:
		return recurrenceSelectorsSafe, 0
	}

	states := cycle / greatestCommonDivisor(interval, cycle)
	step := interval % cycle
	value := start
	reachable := make([]bool, states)
	for i := 0; i < states; i++ {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return recurrenceSelectorsCanceled, 0
			default:
			}
		}
		if !consumeRecurrenceWork(seriesRemaining, aggregateRemaining) {
			return recurrenceSelectorsExcessive, 0
		}
		reachable[i] = matches(value)
		value = (value + step) % cycle
	}
	maxEmpty, ok := maximumCyclicEmptyPeriods(reachable)
	if !ok {
		return recurrenceSelectorsUnreachable, 0
	}
	return recurrenceSelectorsSafe, maxEmpty
}

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func intSliceContains(values []int, target int) bool {
	index := sort.SearchInts(values, target)
	return index < len(values) && values[index] == target
}

func normalizeRecurrenceSelectorLists(opt *rrule.ROption) bool {
	if opt == nil {
		return false
	}
	for _, values := range []*[]int{
		&opt.Bysetpos, &opt.Bymonth, &opt.Bymonthday, &opt.Byyearday,
		&opt.Byweekno, &opt.Byhour, &opt.Byminute, &opt.Bysecond, &opt.Byeaster,
	} {
		seen := make(map[int]struct{}, len(*values))
		unique := (*values)[:0]
		for _, value := range *values {
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			unique = append(unique, value)
		}
		sort.Ints(unique)
		*values = unique
	}
	seenWeekdays := make(map[rrule.Weekday]struct{}, len(opt.Byweekday))
	uniqueWeekdays := opt.Byweekday[:0]
	for _, value := range opt.Byweekday {
		if _, exists := seenWeekdays[value]; exists {
			continue
		}
		seenWeekdays[value] = struct{}{}
		uniqueWeekdays = append(uniqueWeekdays, value)
	}
	opt.Byweekday = uniqueWeekdays
	return recurrenceTimesPerDate(opt) <= int(maxRecurrenceExpansionWork)
}

type recurrenceSelectorSafety uint8

const (
	recurrenceSelectorsSafe recurrenceSelectorSafety = iota
	recurrenceSelectorsCanceled
	recurrenceSelectorsUnsupported
	recurrenceSelectorsUnreachable
	recurrenceSelectorsExcessive
)

// checkRecurrenceSelectorSafety prevents one rrule-go Iterator call from
// scanning empty periods all the way to year 9999. Date selector phases repeat
// over the 400-year Gregorian cycle, so lower-frequency rules can be proved
// reachable with bounded work before entering the dependency's iterator.
func checkRecurrenceSelectorSafety(ctx context.Context, opt *rrule.ROption, seriesRemaining, aggregateRemaining *int64) (recurrenceSelectorSafety, int, int64) {
	if opt == nil {
		return recurrenceSelectorsUnreachable, 0, 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(opt.Byeaster) != 0 {
		// BYEASTER is a non-RFC extension and unchecked offsets can panic in
		// rrule-go while it builds its internal year mask.
		return recurrenceSelectorsUnsupported, 0, 0
	}
	for i := range opt.Byweekday {
		if opt.Byweekday[i].N() != 0 && opt.Freq != rrule.YEARLY && opt.Freq != rrule.MONTHLY {
			return recurrenceSelectorsUnsupported, 0, 0
		}
	}
	interval := opt.Interval
	if interval < 1 {
		interval = 1
	}
	if interval > int(maxRecurrenceExpansionWork) {
		return recurrenceSelectorsExcessive, 0, 0
	}
	clockSafety, clockMaxEmpty := recurrenceClockSelectorSafety(ctx, opt, seriesRemaining, aggregateRemaining)
	if clockSafety != recurrenceSelectorsSafe {
		return clockSafety, 0, 0
	}

	if opt.Freq >= rrule.HOURLY {
		if hasRecurrenceDateSelectors(opt) {
			// Combining a sub-daily INTERVAL phase with calendar selectors has a
			// multi-century state space. Fail closed rather than call an iterator
			// whose empty-period loop has no context or work-budget check.
			return recurrenceSelectorsUnsupported, 0, 0
		}
		if recurrenceBySetPosCanSelect(opt, recurrenceTimesPerPeriod(opt)) {
			return recurrenceSelectorsSafe, clockMaxEmpty, int64(recurrenceTimesPerPeriod(opt))
		}
		return recurrenceSelectorsUnreachable, 0, 0
	}

	normalized := normalizeRecurrenceDateDefaults(opt)
	needsDefaultCycle := normalized.Freq == rrule.YEARLY && normalized.Dtstart.Month() == time.February && normalized.Dtstart.Day() == 29 ||
		normalized.Freq == rrule.MONTHLY && normalized.Dtstart.Day() > 28
	if !hasRecurrenceDateSelectors(opt) && !needsDefaultCycle {
		if !consumeRecurrenceWork(seriesRemaining, aggregateRemaining) {
			return recurrenceSelectorsExcessive, 0, 0
		}
		candidates := recurrenceTimesPerDate(&normalized)
		if !recurrenceBySetPosCanSelect(&normalized, candidates) {
			return recurrenceSelectorsUnreachable, 0, 0
		}
		maxEmpty := 0
		if normalized.Freq == rrule.MONTHLY && normalized.Dtstart.Day() > 28 {
			maxEmpty = 1
		}
		return recurrenceSelectorsSafe, maxEmpty, int64(candidates)
	}

	var cycle int
	var periodStart time.Time
	switch normalized.Freq {
	case rrule.YEARLY:
		cycle = 400 / greatestCommonDivisor(interval, 400)
		periodStart = time.Date(normalized.Dtstart.Year(), time.January, 1, 0, 0, 0, 0, normalized.Dtstart.Location())
	case rrule.MONTHLY:
		cycle = 4800 / greatestCommonDivisor(interval, 4800)
		periodStart = time.Date(normalized.Dtstart.Year(), normalized.Dtstart.Month(), 1, 0, 0, 0, 0, normalized.Dtstart.Location())
	case rrule.WEEKLY:
		cycle = 20871 / greatestCommonDivisor(interval, 20871)
		periodStart = recurrenceWeekStart(normalized.Dtstart, normalized.Wkst.Day())
	case rrule.DAILY:
		cycleBase := 146097
		if recurrenceHasOnlyWeekdayDateSelectors(&normalized) {
			cycleBase = 7
		}
		cycle = cycleBase / greatestCommonDivisor(interval, cycleBase)
		year, month, day := normalized.Dtstart.Date()
		periodStart = time.Date(year, month, day, 0, 0, 0, 0, normalized.Dtstart.Location())
	default:
		return recurrenceSelectorsUnsupported, 0, 0
	}

	states := make([]bool, cycle)
	timesPerDate := recurrenceTimesPerDate(&normalized)
	var maxPeriodWork int64
	for i := 0; i < cycle; i++ {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return recurrenceSelectorsCanceled, 0, 0
			default:
			}
		}
		matches, ok := recurrenceMatchingDatesInPeriod(&normalized, periodStart, aggregateRemaining)
		if !ok {
			return recurrenceSelectorsExcessive, 0, 0
		}
		candidates := matches * timesPerDate
		states[i] = recurrenceBySetPosCanSelect(&normalized, candidates)
		periodWork := int64(candidates)
		if len(normalized.Bysetpos) != 0 {
			positionWork := saturatingWorkMultiply(int64(max(1, matches)), int64(len(normalized.Bysetpos)))
			periodWork = max(int64(timesPerDate), int64(matches), positionWork)
		} else if normalized.Count > 0 && int64(normalized.Count) < periodWork {
			periodWork = int64(normalized.Count)
		}
		if periodWork > maxRecurrenceExpansionWork {
			return recurrenceSelectorsExcessive, 0, 0
		}
		if periodWork > maxPeriodWork {
			maxPeriodWork = periodWork
		}
		switch normalized.Freq {
		case rrule.YEARLY:
			periodStart = periodStart.AddDate(interval, 0, 0)
		case rrule.MONTHLY:
			periodStart = periodStart.AddDate(0, interval, 0)
		case rrule.WEEKLY:
			periodStart = periodStart.AddDate(0, 0, interval*7)
		case rrule.DAILY:
			periodStart = periodStart.AddDate(0, 0, interval)
		}
	}

	maxEmpty, reachable := maximumCyclicEmptyPeriods(states)
	if !reachable {
		return recurrenceSelectorsUnreachable, 0, 0
	}
	if maxEmpty >= int(maxRecurrenceExpansionWork) {
		return recurrenceSelectorsExcessive, 0, 0
	}
	return recurrenceSelectorsSafe, maxEmpty, maxPeriodWork
}

func consumeRecurrenceWork(seriesRemaining, aggregateRemaining *int64) bool {
	if seriesRemaining == nil || aggregateRemaining == nil || *seriesRemaining <= 0 || *aggregateRemaining <= 0 {
		return false
	}
	*seriesRemaining = *seriesRemaining - 1
	*aggregateRemaining = *aggregateRemaining - 1
	return true
}

func consumeAggregateRecurrenceWork(aggregateRemaining *int64, work int64) bool {
	if aggregateRemaining == nil || work < 0 || *aggregateRemaining < work {
		return false
	}
	*aggregateRemaining -= work
	return true
}

func hasRecurrenceDateSelectors(opt *rrule.ROption) bool {
	return len(opt.Bymonth)+len(opt.Bymonthday)+len(opt.Byyearday)+
		len(opt.Byweekno)+len(opt.Byweekday)+len(opt.Byeaster) > 0
}

func recurrenceHasOnlyWeekdayDateSelectors(opt *rrule.ROption) bool {
	return len(opt.Byweekday) != 0 && len(opt.Bymonth)+len(opt.Bymonthday)+
		len(opt.Byyearday)+len(opt.Byweekno)+len(opt.Byeaster) == 0
}

func normalizeRecurrenceDateDefaults(opt *rrule.ROption) rrule.ROption {
	normalized := *opt
	if len(normalized.Byweekno)+len(normalized.Byyearday)+len(normalized.Bymonthday)+
		len(normalized.Byweekday)+len(normalized.Byeaster) != 0 {
		return normalized
	}
	switch normalized.Freq {
	case rrule.YEARLY:
		if len(normalized.Bymonth) == 0 {
			normalized.Bymonth = []int{int(normalized.Dtstart.Month())}
		}
		normalized.Bymonthday = []int{normalized.Dtstart.Day()}
	case rrule.MONTHLY:
		normalized.Bymonthday = []int{normalized.Dtstart.Day()}
	case rrule.WEEKLY:
		day := (int(normalized.Dtstart.Weekday()) + 6) % 7
		weekdays := [...]rrule.Weekday{rrule.MO, rrule.TU, rrule.WE, rrule.TH, rrule.FR, rrule.SA, rrule.SU}
		normalized.Byweekday = []rrule.Weekday{weekdays[day]}
	}
	return normalized
}

func recurrenceTimesPerDate(opt *rrule.ROption) int {
	hours, minutes, seconds := len(opt.Byhour), len(opt.Byminute), len(opt.Bysecond)
	if hours == 0 {
		hours = 1
	}
	if minutes == 0 {
		minutes = 1
	}
	if seconds == 0 {
		seconds = 1
	}
	return hours * minutes * seconds
}

func recurrenceTimesPerPeriod(opt *rrule.ROption) int {
	switch opt.Freq {
	case rrule.HOURLY:
		minutes, seconds := len(opt.Byminute), len(opt.Bysecond)
		if minutes == 0 {
			minutes = 1
		}
		if seconds == 0 {
			seconds = 1
		}
		return minutes * seconds
	case rrule.MINUTELY:
		if len(opt.Bysecond) != 0 {
			return len(opt.Bysecond)
		}
		return 1
	case rrule.SECONDLY:
		return 1
	default:
		return recurrenceTimesPerDate(opt)
	}
}

func recurrenceBySetPosCanSelect(opt *rrule.ROption, candidates int) bool {
	if candidates <= 0 {
		return false
	}
	if len(opt.Bysetpos) == 0 {
		return true
	}
	for _, position := range opt.Bysetpos {
		if position > 0 && position <= candidates || position < 0 && -position <= candidates {
			return true
		}
	}
	return false
}

func recurrenceMatchingDatesInPeriod(opt *rrule.ROption, start time.Time, aggregateRemaining *int64) (int, bool) {
	end := start.AddDate(0, 0, 1)
	switch opt.Freq {
	case rrule.YEARLY:
		end = start.AddDate(1, 0, 0)
	case rrule.MONTHLY:
		end = start.AddDate(0, 1, 0)
	case rrule.WEEKLY:
		end = start.AddDate(0, 0, 7)
	}
	matches := 0
	for date := start; date.Before(end); date = date.AddDate(0, 0, 1) {
		if !consumeAggregateRecurrenceWork(aggregateRemaining, 1) {
			return 0, false
		}
		if recurrenceDateMatches(opt, date) {
			matches++
		}
	}
	return matches, true
}

func recurrenceDateMatches(opt *rrule.ROption, date time.Time) bool {
	if len(opt.Bymonth) != 0 && !intSliceContains(opt.Bymonth, int(date.Month())) {
		return false
	}
	daysInMonth := time.Date(date.Year(), date.Month()+1, 0, 0, 0, 0, 0, date.Location()).Day()
	negativeMonthDay := date.Day() - daysInMonth - 1
	if len(opt.Bymonthday) != 0 &&
		!intSliceContains(opt.Bymonthday, date.Day()) &&
		!intSliceContains(opt.Bymonthday, negativeMonthDay) {
		return false
	}
	daysInYear := 365
	if time.Date(date.Year(), time.February, 29, 0, 0, 0, 0, date.Location()).Month() == time.February {
		daysInYear = 366
	}
	negativeYearDay := date.YearDay() - daysInYear - 1
	if len(opt.Byyearday) != 0 &&
		!intSliceContains(opt.Byyearday, date.YearDay()) &&
		!intSliceContains(opt.Byyearday, negativeYearDay) {
		return false
	}
	if len(opt.Byweekno) != 0 && !recurrenceWeekNumberMatches(date, opt.Wkst.Day(), opt.Byweekno) {
		return false
	}

	weekday := (int(date.Weekday()) + 6) % 7
	hasPlain, plainMatch := false, false
	hasOrdinal, ordinalMatch := false, false
	for i := range opt.Byweekday {
		selector := &opt.Byweekday[i]
		if selector.N() == 0 {
			hasPlain = true
			plainMatch = plainMatch || selector.Day() == weekday
			continue
		}
		hasOrdinal = true
		if selector.Day() != weekday {
			continue
		}
		positive, negative := recurrenceWeekdayOrdinals(opt, date, daysInMonth, daysInYear)
		ordinalMatch = ordinalMatch || selector.N() == positive || selector.N() == negative
	}
	return (!hasPlain || plainMatch) && (!hasOrdinal || ordinalMatch)
}

func recurrenceWeekdayOrdinals(opt *rrule.ROption, date time.Time, daysInMonth, daysInYear int) (int, int) {
	day, lastDay := date.Day(), daysInMonth
	if opt.Freq == rrule.YEARLY && len(opt.Bymonth) == 0 {
		day, lastDay = date.YearDay(), daysInYear
	}
	positive := (day-1)/7 + 1
	negative := -((lastDay-day)/7 + 1)
	return positive, negative
}

func recurrenceWeekNumberMatches(date time.Time, weekStart int, selectors []int) bool {
	first := recurrenceFirstWeekStart(date.Year(), weekStart, date.Location())
	weekYear := date.Year()
	if date.Before(first) {
		weekYear--
		first = recurrenceFirstWeekStart(weekYear, weekStart, date.Location())
	} else {
		next := recurrenceFirstWeekStart(date.Year()+1, weekStart, date.Location())
		if !date.Before(next) {
			weekYear++
			first = next
		}
	}
	week := recurrenceCalendarDaysBetween(first, date)/7 + 1
	nextFirst := recurrenceFirstWeekStart(weekYear+1, weekStart, date.Location())
	weeks := recurrenceCalendarDaysBetween(first, nextFirst) / 7
	return intSliceContains(selectors, week) || intSliceContains(selectors, week-weeks-1)
}

func recurrenceCalendarDaysBetween(start, end time.Time) int {
	startYear, startMonth, startDay := start.Date()
	endYear, endMonth, endDay := end.Date()
	startUTC := time.Date(startYear, startMonth, startDay, 0, 0, 0, 0, time.UTC)
	endUTC := time.Date(endYear, endMonth, endDay, 0, 0, 0, 0, time.UTC)
	return int(endUTC.Sub(startUTC) / (24 * time.Hour))
}

func recurrenceFirstWeekStart(year, weekStart int, location *time.Location) time.Time {
	jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, location)
	return recurrenceWeekStart(jan4, weekStart)
}

func recurrenceWeekStart(date time.Time, weekStart int) time.Time {
	year, month, day := date.Date()
	date = time.Date(year, month, day, 0, 0, 0, 0, date.Location())
	weekday := (int(date.Weekday()) + 6) % 7
	delta := (weekday - weekStart + 7) % 7
	return date.AddDate(0, 0, -delta)
}

func maximumCyclicEmptyPeriods(states []bool) (int, bool) {
	if len(states) == 0 {
		return 0, false
	}
	reachable := false
	for _, state := range states {
		reachable = reachable || state
	}
	if !reachable {
		return len(states), false
	}
	maxEmpty, empty := 0, 0
	for i := 0; i < len(states)*2; i++ {
		if states[i%len(states)] {
			empty = 0
			continue
		}
		empty++
		if empty > maxEmpty && empty < len(states) {
			maxEmpty = empty
		}
	}
	return maxEmpty, true
}

func recurrenceOccurrenceEnd(master Event, start time.Time) time.Time {
	if master.hasNominalDuration {
		return start.AddDate(0, 0, master.nominalDurationDays).Add(master.nominalDurationRemainder)
	}
	return start.Add(master.EndTime.Sub(master.StartTime))
}

func recurrenceWorkEstimate(opt *rrule.ROption, rangeEnd time.Time, maxEmptyPeriods int, maxPeriodCandidates int64) int64 {
	if opt == nil {
		return maxRecurrenceExpansionWork + 1
	}
	if maxPeriodCandidates < 1 {
		maxPeriodCandidates = 1
	}
	interval := int64(opt.Interval)
	if interval < 1 {
		interval = 1
	}
	if interval > maxRecurrenceExpansionWork {
		return maxRecurrenceExpansionWork + 1
	}
	end := rangeEnd
	if !opt.Until.IsZero() && opt.Until.Before(end) {
		end = opt.Until
	}
	if !end.After(opt.Dtstart) {
		return 1
	}

	var periods int64
	switch opt.Freq {
	case rrule.SECONDLY:
		periods = durationPeriods(opt.Dtstart, end, time.Second, interval)
	case rrule.MINUTELY:
		periods = durationPeriods(opt.Dtstart, end, time.Minute, interval)
	case rrule.HOURLY:
		periods = durationPeriods(opt.Dtstart, end, time.Hour, interval)
	case rrule.DAILY:
		periods = durationPeriods(opt.Dtstart, end, 24*time.Hour, interval)
	case rrule.WEEKLY:
		periods = durationPeriods(opt.Dtstart, end, 7*24*time.Hour, interval)
	case rrule.MONTHLY:
		months := int64(end.Year()-opt.Dtstart.Year())*12 + int64(end.Month()-opt.Dtstart.Month())
		periods = months/interval + 2
	case rrule.YEARLY:
		periods = int64(end.Year()-opt.Dtstart.Year())/interval + 2
	default:
		return maxRecurrenceExpansionWork + 1
	}
	if periods < 1 {
		periods = 1
	}

	estimate := saturatingWorkMultiply(periods, maxPeriodCandidates)
	perResult := saturatingWorkMultiply(int64(maxEmptyPeriods+1), maxPeriodCandidates)
	if estimate < perResult {
		estimate = perResult
	}
	if opt.Count > 0 {
		countEstimate := saturatingWorkMultiply(int64(opt.Count), perResult)
		if countEstimate < estimate {
			estimate = countEstimate
		}
	}
	return estimate
}

func durationPeriods(start, end time.Time, unit time.Duration, interval int64) int64 {
	span := end.Sub(start)
	if span < 0 {
		return 1
	}
	units := int64(span / unit)
	if units/interval >= maxRecurrenceExpansionWork {
		return maxRecurrenceExpansionWork + 1
	}
	return units/interval + 2
}

func saturatingWorkMultiply(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > maxRecurrenceExpansionWork/b {
		return maxRecurrenceExpansionWork + 1
	}
	return a * b
}

// eventOverlaps tests the half-open [start,end) overlap of an event with
// [rangeStart, rangeEnd].
func eventOverlaps(e Event, rangeStart, rangeEnd time.Time) bool {
	return e.StartTime.Before(rangeEnd) && e.EndTime.After(rangeStart)
}
