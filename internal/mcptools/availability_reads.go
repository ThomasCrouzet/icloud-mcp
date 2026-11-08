package mcptools

import (
	"context"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// Count all returned records before busy filtering. This is not a byte or RSS limit.
const maxAvailabilityEvents = 10000

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
func collectAvailabilityIntervals(ctx context.Context, paths []string, read func(context.Context, string) (icloud.SearchResult, error), opts availabilityOptions) ([]icloud.Interval, error) {
	var intervals []icloud.Interval
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
				batch := icloud.BusyFromEvents(result.value.Events,
					opts.IncludeAllDay, opts.BufferBefore, opts.BufferAfter, opts.Location)
				// Merge as batches arrive so overlapping busy rows do not
				// accumulate across waves.
				intervals = icloud.MergeIntervals(append(intervals, batch...))
				clear(result.value.Events)
			}
		}
		// Release batch backing arrays before the next wave can start.
		clear(results)
	}
	if len(intervals) == 0 {
		return nil, nil
	}
	return intervals, nil
}
