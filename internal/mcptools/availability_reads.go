package mcptools

import (
	"context"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// Count all returned records before busy filtering. This is not a byte or RSS limit.
const maxAvailabilityEvents = 10000

// Ordered four-read windows bound retained batches independently of calendar count.
// Keep search_events on its separate all-calendar pool for fair result selection.
func collectAvailabilityEvents(ctx context.Context, paths []string, read func(context.Context, string) (icloud.SearchResult, error)) ([]icloud.Event, error) {
	var events []icloud.Event
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
			if len(result.value.Events) > maxAvailabilityEvents-len(events) {
				return nil, icloud.NewError(icloud.CodePayloadTooLarge, 0,
					"availability exceeded its total event budget; narrow the date range or calendars", nil)
			}
			if len(result.value.Events) > 0 {
				if events == nil {
					events = make([]icloud.Event, 0, maxAvailabilityEvents)
				}
				events = append(events, result.value.Events...)
			}
		}
		// Release batch backing arrays before the next wave can start.
		clear(results)
	}
	return events, nil
}
