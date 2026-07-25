package icloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"golang.org/x/time/rate"
)

// maxRateWait is the longest a tool call may block waiting for a local rate
// limiter token. Beyond that, fail fast with CodeRateLimited so the 25s tool
// timeout is not burned on queueing under burst contention.
const maxRateWait = 2 * time.Second

// GuardedService decorates a Service with rate limiting (two independent
// budgets: read and write) and bounded retry with exponential backoff.
// Only reads are retried. Mutations are never replayed because a transport
// failure can occur after the server applied the request.
type GuardedService struct {
	inner      Service
	readLimit  *rate.Limiter // 60 reads/min, burst 10
	writeLimit *rate.Limiter // 20 writes/min, burst 3
	readSem    chan struct{} // at most 4 concurrent Calendar reads
	writeSem   chan struct{} // at most 2 concurrent Calendar writes
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
		readSem:    make(chan struct{}, 4),
		writeSem:   make(chan struct{}, 2),
		maxRetries: maxRetries,
		baseDelay:  baseDelay,
	}
}

func acquireCalendar(ctx context.Context, semaphore chan struct{}) error {
	select {
	case semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return calendarContextError(ctx.Err())
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
	return waitLimiter(ctx, g.readLimit, "read")
}

func (g *GuardedService) waitWrite(ctx context.Context) error {
	return waitLimiter(ctx, g.writeLimit, "write")
}

// waitLimiter reserves one token. If the delay exceeds maxRateWait, the
// reservation is cancelled and a typed rate_limited error is returned so
// callers can surface a stable code without sitting on the tool timeout.
func waitLimiter(ctx context.Context, lim *rate.Limiter, kind string) error {
	if err := ctx.Err(); err != nil {
		return calendarContextError(err)
	}
	res := lim.Reserve()
	if !res.OK() {
		return NewError(CodeRateLimited, 429,
			fmt.Sprintf("%s rate limit exceeded", kind), nil)
	}
	delay := res.DelayFrom(time.Now())
	if err := ctx.Err(); err != nil {
		res.Cancel()
		return calendarContextError(err)
	}
	if delay > maxRateWait {
		res.Cancel()
		return &Error{
			Code:       CodeRateLimited,
			Status:     429,
			Message:    fmt.Sprintf("%s rate limit exceeded: retry later", kind),
			Retryable:  true,
			RetryAfter: capPublicRetryAfter(delay),
		}
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		res.Cancel()
		return calendarContextError(ctx.Err())
	case <-timer.C:
		return nil
	}
}

func calendarContextError(err error) *Error {
	message := "Calendar request was canceled"
	if errors.Is(err, context.DeadlineExceeded) {
		message = "Calendar request timed out"
	}
	return NewError(CodeTimeout, 0, message, err)
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
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return calendarContextError(lastErr)
		}
		if AsICloudError(lastErr) != nil {
			return lastErr
		}
		if attempt == g.maxRetries {
			break
		}
		delay := g.baseDelay * time.Duration(math.Pow(2, float64(attempt)))
		// Never log lastErr: Client wraps may include calendar paths or UIDs.
		// Typed *Error already returned above; remaining retries are transport noise.
		slog.Warn("retrying", "operation", op, "attempt", attempt+1, "delay", delay, "error_code", "unavailable")
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return calendarContextError(ctx.Err())
		case <-timer.C:
		}
	}
	return lastErr
}

// ListCalendars: read, retried.
func (g *GuardedService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	if err := g.waitRead(ctx); err != nil {
		return nil, err
	}
	if err := acquireCalendar(ctx, g.readSem); err != nil {
		return nil, err
	}
	defer func() { <-g.readSem }()
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
	if err := acquireCalendar(ctx, g.readSem); err != nil {
		return SearchResult{}, err
	}
	defer func() { <-g.readSem }()
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
	if err := acquireCalendar(ctx, g.readSem); err != nil {
		return nil, err
	}
	defer func() { <-g.readSem }()
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
	if err := acquireCalendar(ctx, g.writeSem); err != nil {
		return "", err
	}
	defer func() { <-g.writeSem }()
	return g.inner.CreateEvent(ctx, calendarPath, ev)
}

// UpdateEvent: write, NEVER retried (non-idempotent: SEQUENCE/DTSTAMP change
// on every attempt). Also never auto-retries 412 conflicts.
func (g *GuardedService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := g.waitWrite(ctx); err != nil {
		return err
	}
	if err := acquireCalendar(ctx, g.writeSem); err != nil {
		return err
	}
	defer func() { <-g.writeSem }()
	return g.inner.UpdateEvent(ctx, calendarPath, uid, up)
}

// DeleteEvent is never retried. A transport error after dispatch cannot prove
// whether either a series DELETE or an occurrence PUT was applied.
func (g *GuardedService) DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error) {
	if err := g.waitWrite(ctx); err != nil {
		return DeleteResult{}, err
	}
	if err := acquireCalendar(ctx, g.writeSem); err != nil {
		return DeleteResult{}, err
	}
	defer func() { <-g.writeSem }()
	return g.inner.DeleteEvent(ctx, calendarPath, uid, opts)
}
