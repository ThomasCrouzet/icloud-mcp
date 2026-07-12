package icloud

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"golang.org/x/time/rate"
)

// GuardedService decorates a Service with rate limiting (two independent
// budgets: read and write) and bounded retry with exponential backoff.
// Only idempotent operations are retried (reads + DeleteEvent);
// CreateEvent/UpdateEvent are NEVER retried (non-idempotent: a retry would
// create a duplicate or wrongly bump SEQUENCE).
type GuardedService struct {
	inner      Service
	readLimit  *rate.Limiter // 60 reads/min, burst 10
	writeLimit *rate.Limiter // 20 writes/min, burst 3
	maxRetries int
	baseDelay  time.Duration
}

var _ Service = (*GuardedService)(nil)

// NewGuardedService builds a GuardedService.
func NewGuardedService(inner Service, maxRetries int, baseDelay time.Duration) *GuardedService {
	return &GuardedService{
		inner:      inner,
		readLimit:  rate.NewLimiter(rate.Every(time.Minute/60), 10),
		writeLimit: rate.NewLimiter(rate.Every(time.Minute/20), 3),
		maxRetries: maxRetries,
		baseDelay:  baseDelay,
	}
}

func (g *GuardedService) waitRead(ctx context.Context) error {
	if err := g.readLimit.Wait(ctx); err != nil {
		return fmt.Errorf("read rate limit exceeded: %w", err)
	}
	return nil
}

func (g *GuardedService) waitWrite(ctx context.Context) error {
	if err := g.writeLimit.Wait(ctx); err != nil {
		return fmt.Errorf("write rate limit exceeded: %w", err)
	}
	return nil
}

// retry retries fn up to maxRetries times with exponential backoff
// (baseDelay * 2^attempt), bounded by ctx.Done().
func (g *GuardedService) retry(ctx context.Context, op string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= g.maxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == g.maxRetries {
			break
		}
		delay := g.baseDelay * time.Duration(math.Pow(2, float64(attempt)))
		slog.Warn("retrying", "operation", op, "attempt", attempt+1, "delay", delay, "error", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

// ListCalendars: read, retried.
func (g *GuardedService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	if err := g.waitRead(ctx); err != nil {
		return nil, err
	}
	var result []Calendar
	err := g.retry(ctx, "ListCalendars", func() error {
		var e error
		result, e = g.inner.ListCalendars(ctx)
		return e
	})
	return result, err
}

// SearchEvents: read, retried.
func (g *GuardedService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	if err := g.waitRead(ctx); err != nil {
		return nil, err
	}
	var result []Event
	err := g.retry(ctx, "SearchEvents", func() error {
		var e error
		result, e = g.inner.SearchEvents(ctx, calendarPath, start, end)
		return e
	})
	return result, err
}

// CreateEvent: write, NEVER retried (non-idempotent).
func (g *GuardedService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	if err := g.waitWrite(ctx); err != nil {
		return "", err
	}
	return g.inner.CreateEvent(ctx, calendarPath, ev)
}

// UpdateEvent: write, NEVER retried (non-idempotent: SEQUENCE/DTSTAMP change
// on every attempt).
func (g *GuardedService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := g.waitWrite(ctx); err != nil {
		return err
	}
	return g.inner.UpdateEvent(ctx, calendarPath, uid, up)
}

// DeleteEvent: write, retried (idempotent: deleting an already deleted event
// fails cleanly with no dangerous side effect).
func (g *GuardedService) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	if err := g.waitWrite(ctx); err != nil {
		return "", err
	}
	var title string
	err := g.retry(ctx, "DeleteEvent", func() error {
		var e error
		title, e = g.inner.DeleteEvent(ctx, calendarPath, uid)
		return e
	})
	return title, err
}
