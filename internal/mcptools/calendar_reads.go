package mcptools

import (
	"context"
	"sync"
)

// calendarReadConcurrency matches the Calendar read semaphore. Handlers use a
// bounded worker pool for one multi-calendar request; the service still
// enforces the domain-wide bound across concurrent MCP requests.
const calendarReadConcurrency = 4

type calendarReadResult[T any] struct {
	value T
	err   error
}

// parallelCalendarReadsWaves keeps at most one worker-width wave of results
// alive. consume returns false to stop scheduling further waves.
func parallelCalendarReadsWaves[T any](ctx context.Context, paths []string, read func(context.Context, string) (T, error), consume func(int, T, error) bool) {
	for start := 0; start < len(paths); start += calendarReadConcurrency {
		end := start + calendarReadConcurrency
		if end > len(paths) {
			end = len(paths)
		}
		results := parallelCalendarReads(ctx, paths[start:end], read)
		for index, result := range results {
			if !consume(start+index, result.value, result.err) {
				return
			}
		}
	}
}

func parallelCalendarReads[T any](ctx context.Context, paths []string, read func(context.Context, string) (T, error)) []calendarReadResult[T] {
	results := make([]calendarReadResult[T], len(paths))
	if len(paths) == 0 {
		return results
	}
	workers := calendarReadConcurrency
	if workers > len(paths) {
		workers = len(paths)
	}
	jobs := make(chan int)
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for index := range jobs {
				value, err := read(ctx, paths[index])
				results[index] = calendarReadResult[T]{value: value, err: err}
			}
		}()
	}
	for index := range paths {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	return results
}
