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
