package mcptools

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// availabilityWaveRead returns a read function that rebuilds each calendar's
// events per call. The collector clears returned event slices, so a shared
// backing array cannot survive a second iteration.
func availabilityWaveRead(calendars, perCalendar int, paths []string) func(context.Context, string) (icloud.SearchResult, error) {
	base := mustParseTime("2026-07-01T09:00:00Z")
	return func(_ context.Context, path string) (icloud.SearchResult, error) {
		index := 0
		for i, candidate := range paths {
			if candidate == path {
				index = i
				break
			}
		}
		events := make([]icloud.Event, perCalendar)
		for j := range events {
			start := base.Add(time.Duration(index*perCalendar+j) * 24 * time.Hour)
			events[j] = icloud.Event{
				UID:       fmt.Sprintf("wave-%05d-%02d", index, j),
				Title:     "Busy",
				StartTime: start,
				EndTime:   start.Add(time.Hour),
			}
		}
		return icloud.SearchResult{Events: events}, nil
	}
}

// BenchmarkAvailabilityCollectorWaves measures the bounded busy-interval
// collection across calendar counts. merge-passes counts full-slice sort and
// merge work: the collector must keep it at one pass regardless of waves.
func BenchmarkAvailabilityCollectorWaves(b *testing.B) {
	for _, calendars := range []int{4, 64, 256, 1024} {
		b.Run(fmt.Sprintf("Calendars%d", calendars), func(b *testing.B) {
			paths := make([]string, calendars)
			listed := make([]icloud.Calendar, calendars)
			for i := range paths {
				paths[i] = fmt.Sprintf("/cal/wave-%05d/", i)
				listed[i] = icloud.Calendar{Path: paths[i]}
			}
			read := availabilityWaveRead(calendars, 2, paths)
			opts := availabilityOptions{IncludeAllDay: true, Location: time.UTC}

			var passes atomic.Int64
			var sortInput atomic.Int64
			original := mergeAvailabilityIntervals
			b.Cleanup(func() { mergeAvailabilityIntervals = original })
			mergeAvailabilityIntervals = func(in []icloud.Interval) []icloud.Interval {
				passes.Add(1)
				sortInput.Add(int64(len(in)))
				return original(in)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				passes.Store(0)
				sortInput.Store(0)
				intervals, err := collectAvailabilityIntervals(b.Context(), paths, read, opts)
				if err != nil {
					b.Fatal(err)
				}
				if len(intervals) != calendars*2 {
					b.Fatalf("intervals = %d, want %d", len(intervals), calendars*2)
				}
			}
			b.StopTimer()
			mergeAvailabilityIntervals = original
			b.ReportMetric(float64(calendars), "calendars")
			b.ReportMetric(float64((calendars+calendarReadConcurrency-1)/calendarReadConcurrency), "waves")
			b.ReportMetric(float64(passes.Load()), "merge-passes")
			b.ReportMetric(float64(sortInput.Load()), "sorted-intervals")
		})
	}
}
