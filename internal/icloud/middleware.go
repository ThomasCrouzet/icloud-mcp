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

// LimitStatus reports the live state of a rate limiter: the configured
// sustained rate (tokens/sec), the burst size, and the currently available
// tokens. No secrets: only numeric rate-limit state.
type LimitStatus struct {
	Tokens float64 `json:"tokens"` // available tokens right now
	Limit  float64 `json:"limit"`  // tokens per second (rate.Every duration)
	Burst  int     `json:"burst"`  // bucket size
}

// RateLimitStatus returns the current read/write rate-limiter state, for the
// optional health endpoint. It is safe for concurrent use.
func (g *GuardedService) RateLimitStatus() RateLimits {
	return RateLimits{
		Read:  LimitStatus{Tokens: g.readLimit.Tokens(), Limit: float64(g.readLimit.Limit()), Burst: g.readLimit.Burst()},
		Write: LimitStatus{Tokens: g.writeLimit.Tokens(), Limit: float64(g.writeLimit.Limit()), Burst: g.writeLimit.Burst()},
	}
}

// RateLimits groups the read and write limiter statuses.
type RateLimits struct {
	Read  LimitStatus `json:"read"`
	Write LimitStatus `json:"write"`
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
// (baseDelay * 2^attempt), bounded by ctx.Done(). It only retries TRANSIENT,
// NON-CLASSIFIED errors (e.g. a connection blip): a typed *icloud.Error means
// the HTTP-layer retry/classify doer already exhausted its own budget for the
// retryable statuses (429/5xx), or the error is terminal (auth, not found,
// 412), so retrying at this layer would be either redundant or pointless.
func (g *GuardedService) retry(ctx context.Context, op string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= g.maxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if AsICloudError(lastErr) != nil {
			return lastErr
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
func (g *GuardedService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time, opts *SearchOptions) (SearchResult, error) {
	if err := g.waitRead(ctx); err != nil {
		return SearchResult{}, err
	}
	var result SearchResult
	err := g.retry(ctx, "SearchEvents", func() error {
		var e error
		result, e = g.inner.SearchEvents(ctx, calendarPath, start, end, opts)
		return e
	})
	return result, err
}

// GetEvent: read, retried.
func (g *GuardedService) GetEvent(ctx context.Context, calendarPath, uid string) (*EventDetail, error) {
	if err := g.waitRead(ctx); err != nil {
		return nil, err
	}
	var result *EventDetail
	err := g.retry(ctx, "GetEvent", func() error {
		var e error
		result, e = g.inner.GetEvent(ctx, calendarPath, uid)
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
// on every attempt). Also never auto-retries 412 conflicts.
func (g *GuardedService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := g.waitWrite(ctx); err != nil {
		return err
	}
	return g.inner.UpdateEvent(ctx, calendarPath, uid, up)
}

// DeleteEvent: write. Series/full deletes are retried (idempotent at the
// resource level). Dry-run never hits the inner service more than once and
// performs no mutation. Occurrence-scoped deletes are NOT retried (they
// rewrite the master with EXDATE/override and bump SEQUENCE).
func (g *GuardedService) DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error) {
	if err := g.waitWrite(ctx); err != nil {
		return DeleteResult{}, err
	}
	if opts != nil && opts.DryRun {
		return g.inner.DeleteEvent(ctx, calendarPath, uid, opts)
	}
	if opts != nil && opts.Scope == ScopeOccurrence {
		return g.inner.DeleteEvent(ctx, calendarPath, uid, opts)
	}
	var result DeleteResult
	err := g.retry(ctx, "DeleteEvent", func() error {
		var e error
		result, e = g.inner.DeleteEvent(ctx, calendarPath, uid, opts)
		return e
	})
	return result, err
}
