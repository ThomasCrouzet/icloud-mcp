package icloud

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// retryClassifier is an httpDoer that wraps the upstream auth+allowlist
// doer to:
//
//  1. Retry read requests on 429 and 502/503/504, honoring Retry-After when
//     present, with exponential backoff + jitter otherwise.
//  2. Never replay PUT or DELETE. A gateway 502/503/504 or transport error
//     after dispatch has an ambiguous mutation outcome and is surfaced as
//     outcome_unknown. A write-side 429 is definitive and rate_limited.
//  3. Pass all other HTTP responses to the Calendar operation so it can apply
//     operation-specific semantics such as create conflict, update conflict,
//     and idempotent series-delete handling.
//
// The retry budget is bounded by the request context (the per-tool 25s
// timeout in production), so retries stop before the tool timeout fires.
// Network errors are NOT retried here: replaying a PUT after a connection
// reset could duplicate or wrongly bump SEQUENCE. The GuardedService keeps
// its own retry for idempotent reads on top of this layer.
type retryClassifier struct {
	inner     httpDoer
	maxTries  int           // total attempts (1 + retries), e.g. 6
	baseDelay time.Duration // backoff base for the no-Retry-After case
	maxDelay  time.Duration // backoff cap
	now       func() time.Time
	rand      func() float64 // [0,1) for jitter
}

// NewRetryClassifier builds a retryClassifier with sane defaults:
// 6 tries, 500ms base, 10s cap, wall-clock now, crypto-strength jitter. It is
// exported so the production wiring (cmd/icloud-mcp) can wrap the auth+allowlist
// doer; tests reach in through the unexported fields when they need
// deterministic timing.
func NewRetryClassifier(inner httpDoer) httpDoer {
	return &retryClassifier{
		inner:     inner,
		maxTries:  6,
		baseDelay: 500 * time.Millisecond,
		maxDelay:  10 * time.Second,
		now:       time.Now,
		rand:      rand.Float64,
	}
}

// Do implements httpDoer.
func (r *retryClassifier) Do(req *http.Request) (*http.Response, error) {
	write := isCalendarMutationMethod(req.Method)
	for attempt := 0; ; attempt++ {
		if err := req.Context().Err(); err != nil {
			return nil, calendarContextError(err)
		}
		if err := rewindRequestBody(req); err != nil {
			return nil, err
		}
		resp, err := r.inner.Do(req)
		if err != nil {
			if write {
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				return nil, outcomeUnknownError(0)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, calendarContextError(err)
			}
			return resp, err
		}
		if resp == nil {
			if write {
				return nil, outcomeUnknownError(0)
			}
			return nil, NewError(CodeProtocolError, 0, "Calendar HTTP client returned no response", nil)
		}
		if resp.Body == nil {
			resp.Body = http.NoBody
		}
		if isRetryStatus(resp.StatusCode) {
			if write {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusTooManyRequests {
					delay, _ := headerRetryAfter(resp, r.now)
					return nil, classifyStatusWithRetryAfter(resp.StatusCode, delay)
				}
				return nil, outcomeUnknownError(resp.StatusCode)
			}
			wait := retryDelay(resp, attempt, r.baseDelay, r.maxDelay, r.now, r.rand)
			_ = resp.Body.Close()
			if attempt+1 >= r.maxTries {
				return nil, classifyStatusWithRetryAfter(resp.StatusCode, wait)
			}
			if err := sleep(req.Context(), wait); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

func isCalendarMutationMethod(method string) bool {
	return method == http.MethodPut || method == http.MethodDelete
}

// rewindRequestBody restores req.Body from GetBody before each read attempt.
// No-op when there is no body or GetBody is unset.
func rewindRequestBody(req *http.Request) error {
	if req == nil || req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("rewinding request body for retry: %w", err)
	}
	req.Body = body
	return nil
}

// isRetryStatus reports whether a status is an idempotent "try again later"
// signal that the server guarantees it did not process.
func isRetryStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryDelay computes the wait before the next attempt. It honors Retry-After
// (delta-seconds or HTTP-date per RFC 9110) when present, else falls back to
// exponential backoff base*2^attempt capped at max, plus up to 25% jitter. The
// returned error, if non-nil, is the (already context-derived) reason to abort
// immediately rather than sleep.
func retryDelay(resp *http.Response, attempt int, base, max time.Duration, now func() time.Time, rand func() float64) time.Duration {
	if d, ok := headerRetryAfter(resp, now); ok {
		// Explicit Retry-After: 0 means retry immediately (do not fall back to
		// exponential backoff). Positive values are capped at max.
		return capDelay(d, max)
	}
	// Exponential backoff with jitter.
	d := base << attempt // base * 2^attempt
	d = capDelay(d, max)
	jitter := time.Duration(rand() * float64(d) * 0.25)
	return d + jitter
}

// headerRetryAfter parses Retry-After as delta-seconds or HTTP-date. ok is true
// when the header was present and parseable (including a zero delay).
func headerRetryAfter(resp *http.Response, now func() time.Time) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
		if secs <= 0 {
			return 0, true
		}
		// Cap before multiplying so huge delta-seconds cannot overflow Duration.
		const maxDeltaSecs = int64(maxPublicRetryAfter / time.Second)
		if secs > maxDeltaSecs {
			secs = maxDeltaSecs
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(ra); err == nil {
		d := t.Sub(now())
		if d < 0 {
			return 0, true
		}
		return d, true
	}
	return 0, false
}

func capDelay(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}

// sleep waits for d, but aborts immediately if the request context is
// already done (e.g. the 25s per-tool timeout fired).
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return calendarContextError(ctx.Err())
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return calendarContextError(ctx.Err())
	case <-timer.C:
		return nil
	}
}
