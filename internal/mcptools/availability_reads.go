package mcptools

import (
	"context"
	"sort"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// Count all returned records before busy filtering. This is not a byte or RSS limit.
const maxAvailabilityEvents = 10000

// maxAvailabilityIntervals bounds the accumulated busy intervals. BusyFromEvents
// yields at most one interval per event, so the default matches the event
// budget. It is a variable so tests can drive the refusal path.
var maxAvailabilityIntervals = maxAvailabilityEvents

// mergeAvailabilityIntervals is the single bounded merge at the end of the
// collection. It is a variable so tests can count merge passes.
var mergeAvailabilityIntervals = mergeSortedIntervals

// availabilityOptions carries the busy-interval projection applied as soon as
// each bounded read batch returns. Converting early keeps only intervals
// resident instead of up to 10,000 full Event values.
type availabilityOptions struct {
	IncludeAllDay bool
	BufferBefore  time.Duration
	BufferAfter   time.Duration
	Location      *time.Location
}

// Ordered four-read windows bound retained batches independently of calendar count.
// Keep search_events on its separate all-calendar pool for fair result selection.
//
// Busy intervals append to one bounded slice as waves complete. A single
// deterministic sort and merge runs at the end, so N intervals over B waves
// cost O(N log N) instead of O(N x B) copies and re-sorts. FindFreeSlots then
// re-sorts the merged result once; the extra pass stays linear in the merged
// count and keeps the placement policy unchanged.
func collectAvailabilityIntervals(ctx context.Context, paths []string, read func(context.Context, string) (icloud.SearchResult, error), opts availabilityOptions) ([]icloud.Interval, error) {
	intervals := make([]icloud.Interval, 0, min(len(paths), 16))
	count := 0
	for offset := 0; offset < len(paths); offset += calendarReadConcurrency {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(offset+calendarReadConcurrency, len(paths))
		results := parallelCalendarReads(ctx, paths[offset:end], func(ctx context.Context, path string) (icloud.SearchResult, error) {
			if err := ctx.Err(); err != nil {
				return icloud.SearchResult{}, err
			}
			return read(ctx, path)
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, result := range results {
			if result.err != nil {
				return nil, result.err
			}
			if result.value.TruncatedByExpansion {
				return nil, icloud.NewError(icloud.CodePartialFailure, 0,
					"recurrence expansion was truncated; availability cannot be determined", nil)
			}
			// The cap counts every returned record, including transparent,
			// cancelled, or otherwise non-busy rows that are dropped now.
			if len(result.value.Events) > maxAvailabilityEvents-count {
				return nil, icloud.NewError(icloud.CodePayloadTooLarge, 0,
					"availability exceeded its total event budget; narrow the date range or calendars", nil)
			}
			count += len(result.value.Events)
			if len(result.value.Events) > 0 {
				// Project each batch to bounded intervals immediately, then
				// drop the events so no payload survives the wave.
				batch := icloud.BusyFromEvents(result.value.Events,
					opts.IncludeAllDay, opts.BufferBefore, opts.BufferAfter, opts.Location)
				clear(result.value.Events)
				if len(intervals)+len(batch) > maxAvailabilityIntervals {
					return nil, icloud.NewError(icloud.CodePayloadTooLarge, 0,
						"availability exceeded its busy-interval budget; narrow the date range or calendars", nil)
				}
				intervals = append(intervals, batch...)
			}
		}
		// Release batch backing arrays before the next wave can start.
		clear(results)
	}
	if len(intervals) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return mergeAvailabilityIntervals(intervals), nil
}

// mergeSortedIntervals sorts bounded intervals by start, then end, and merges
// overlapping or adjacent half-open ranges. It keeps the input slice as its
// output buffer, so no copy of the accumulated set is made.
func mergeSortedIntervals(in []icloud.Interval) []icloud.Interval {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Start.Equal(in[j].Start) {
			return in[i].End.Before(in[j].End)
		}
		return in[i].Start.Before(in[j].Start)
	})
	out := in[:1]
	for _, cur := range in[1:] {
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
