package icloud

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// retryClassifier is an httpDoer that wraps the upstream auth+allowlist
// doer to:
//
//  1. Retry idempotent-at-the-server signals (429 Too Many Requests,
//     502/503/504 gateway errors) honoring Retry-After when present, with
//     exponential backoff + jitter otherwise. These statuses are the server
//     asserting it did NOT process the request, so retrying even a PUT is
//     safe (unlike a connection timeout, where the request may have landed).
//  2. Classify the final non-2xx response into a typed *Error (see
//     errors.go), so the MCP layer surfaces a stable code and a clear,
//     Apple-aware message instead of a raw HTTP status string.
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
	for attempt := 0; ; attempt++ {
		resp, err := r.inner.Do(req)
		if err != nil {
			// Network/transport error: do not replay (unsafe for writes).
			return resp, err
		}
		if isRetryStatus(resp.StatusCode) {
			wait := retryDelay(resp, attempt, r.baseDelay, r.maxDelay, r.now, r.rand)
			_ = resp.Body.Close()
			if attempt+1 >= r.maxTries {
				return nil, classifyStatus(resp.StatusCode)
			}
			if err := sleep(req.Context(), wait); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		// Non-2xx, non-retryable: classify and drain the body.
		_ = resp.Body.Close()
		return nil, classifyStatus(resp.StatusCode)
	}
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
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		// Delta-seconds form.
		if secs, err := strconv.Atoi(ra); err == nil {
			d := time.Duration(secs) * time.Second
			if d < 0 {
				d = 0
			}
			return capDelay(d, max)
		}
		// HTTP-date form (RFC 9110 §6.6.7).
		if t, err := http.ParseTime(ra); err == nil {
			d := t.Sub(now())
			if d < 0 {
				d = 0
			}
			return capDelay(d, max)
		}
	}
	// Exponential backoff with jitter.
	d := base << attempt // base * 2^attempt
	d = capDelay(d, max)
	jitter := time.Duration(rand() * float64(d) * 0.25)
	return d + jitter
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
			return fmt.Errorf("retry aborted: %w", ctx.Err())
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("retry aborted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
