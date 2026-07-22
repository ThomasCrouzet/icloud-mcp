package icloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- ETag conditional PUT ---------------------------------------------------

// TestClient_UpdateEvent_ConditionalPUTSendsIfMatch. When the GET on the
// event returns an ETag, UpdateEvent MUST send an If-Match header carrying
// that ETag on the subsequent PUT, reducing last-writer-wins. The mock
// tracks the ETag, so the PUT records the If-Match value for assertion.
func TestClient_UpdateEvent_ConditionalPUTSendsIfMatch(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	m.etags[objPath] = `"v1"`
	c := m.client()

	newTitle := "Refreshed"
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateEvent() error: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(m.puts))
	}
	if m.puts[0].ifMatch != `"v1"` {
		t.Errorf("PUT If-Match = %q, want %q (the ETag from the GET)", m.puts[0].ifMatch, `"v1"`)
	}
	if m.etags[objPath] == `"v1"` {
		t.Errorf("ETag should have been bumped after the successful PUT, still %q", m.etags[objPath])
	}
}

// TestClient_UpdateEvent_FallsBackToUnconditionalPUTWithoutETag. When the
// server returns no ETag on the GET (the legacy/fallback behavior), the PUT
// is unconditional (no If-Match): never worse than the previous
// last-writer-wins behavior. No 412.
func TestClient_UpdateEvent_FallsBackToUnconditionalPUTWithoutETag(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	// No m.etags entry: the GET serves no ETag.
	c := m.client()

	newTitle := "x"
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateEvent() error: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(m.puts))
	}
	if m.puts[0].ifMatch != "" {
		t.Errorf("PUT If-Match = %q, want empty (no ETag to match against)", m.puts[0].ifMatch)
	}
}

// TestClient_UpdateEvent_Concurrent412PreconditionFailed. Two updates on
// the SAME UID launched concurrently: both GET the current ETag, both modify,
// both PUT with If-Match. The mock serializes PUTs under its mutex: the first
// PUT matches the current ETag and succeeds (bumping it); the second PUT
// carries the now-stale ETag and MUST receive 412 Precondition Failed. The
// losing UpdateEvent returns a typed concurrent_modification error.
func TestClient_UpdateEvent_Concurrent412PreconditionFailed(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	m.etags[objPath] = `"v1"`
	c := m.client()

	var wg sync.WaitGroup
	var aErr, bErr error
	var aStart, bStart sync.WaitGroup

	// Start barriers so both goroutines fire their UpdateEvent as close to
	// simultaneously as possible (both have already done the GET or will).
	aStart.Add(1)
	bStart.Add(1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		aStart.Wait()
		aErr = c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{Title: ref("A")})
	}()
	go func() {
		defer wg.Done()
		bStart.Wait()
		bErr = c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{Title: ref("B")})
	}()
	aStart.Done()
	bStart.Done()
	wg.Wait()

	// Exactly one succeeds, exactly one gets the 412 typed error.
	var successCount, conflictCount int
	for _, err := range []error{aErr, bErr} {
		switch {
		case err == nil:
			successCount++
		case AsICloudError(err) != nil && AsICloudError(err).Code == CodeConcurrentModification:
			conflictCount++
		}
	}
	if successCount != 1 {
		t.Errorf("successCount = %d, want 1 (aErr=%v, bErr=%v)", successCount, aErr, bErr)
	}
	if conflictCount != 1 {
		t.Errorf("conflictCount = %d, want 1 (exactly one 412; aErr=%v, bErr=%v)", conflictCount, aErr, bErr)
	}
}

func ref(s string) *string { return &s }

// --- Retry / classify doer --------------------------------------------------

// statusDoer is a test doer that returns a canned sequence of responses,
// recording how many times Do was called. It never touches the network. When
// the canned sequence is exhausted, the LAST status repeats forever (so an
// always-503 fixture is a one-element slice).
type statusDoer struct {
	mu       sync.Mutex
	statuses []int
	calls    int
	delayFn  func(attempt int)
}

func (s *statusDoer) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	idx := s.calls
	s.calls++
	status := http.StatusOK
	if len(s.statuses) > 0 {
		status = s.statuses[min(idx, len(s.statuses)-1)]
	}
	if s.delayFn != nil {
		f := s.delayFn
		s.mu.Unlock()
		f(idx)
	} else {
		s.mu.Unlock()
	}
	resp := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	return resp, nil
}

func TestRetryClassifier_Retries429ThenSucceeds(t *testing.T) {
	doer := &statusDoer{statuses: []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK}}
	rc := &retryClassifier{
		inner:     doer,
		maxTries:  6,
		baseDelay: time.Millisecond,
		maxDelay:  10 * time.Millisecond,
		now:       time.Now,
		rand:      func() float64 { return 0 },
	}
	req := mustPUTReq(t)
	resp, err := rc.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if doer.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries + success)", doer.calls)
	}
}

func TestRetryClassifier_Retries503ThenExhausts(t *testing.T) {
	doer := &statusDoer{statuses: []int{http.StatusServiceUnavailable}} // always 503
	rc := &retryClassifier{
		inner:     doer,
		maxTries:  3, // 1 + 2 retries
		baseDelay: time.Millisecond,
		maxDelay:  5 * time.Millisecond,
		now:       time.Now,
		rand:      func() float64 { return 0 },
	}
	req := mustPUTReq(t)
	_, err := rc.Do(req)
	if err == nil {
		t.Fatal("expected an error after retries exhausted")
	}
	cerr := AsICloudError(err)
	if cerr == nil || cerr.Code != CodeServerUnavailable {
		t.Errorf("err = %v, want a typed server_unavailable error", err)
	}
	if doer.calls != 3 {
		t.Errorf("calls = %d, want 3 (maxTries)", doer.calls)
	}
}

func TestRetryClassifier_HonorsRetryAfterSeconds(t *testing.T) {
	doer := &statusDoer{statuses: []int{http.StatusTooManyRequests, http.StatusOK}}
	rc := &retryClassifier{
		inner:     doer,
		maxTries:  6,
		baseDelay: 5 * time.Minute, // absurdly high; Retry-After must override
		maxDelay:  10 * time.Minute,
		now:       time.Now,
		rand:      func() float64 { return 0 },
	}
	req := mustPUTReq(t)
	// We need a 429 response carrying Retry-After: 0 so the wait collapses to
	// ~0 rather than the absurd base. Replace the doer's canned body approach
	// with a doer that sets the header on the first response.
	rc.inner = &retryAfterDoer{seconds: 0, then: http.StatusOK}
	resp, err := rc.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

type retryAfterDoer struct {
	seconds int
	then    int
	calls   atomic.Int32
}

func (d *retryAfterDoer) Do(req *http.Request) (*http.Response, error) {
	n := d.calls.Add(1)
	resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
	if n == 1 {
		resp.StatusCode = http.StatusTooManyRequests
		resp.Header.Set("Retry-After", strconv.Itoa(d.seconds))
		return resp, nil
	}
	resp.StatusCode = d.then
	return resp, nil
}

func TestRetryClassifier_DoesNotRetryNonRetryable(t *testing.T) {
	doer := &statusDoer{statuses: []int{http.StatusNotFound}}
	rc := &retryClassifier{
		inner:     doer,
		maxTries:  6,
		baseDelay: time.Millisecond,
		maxDelay:  5 * time.Millisecond,
		now:       time.Now,
		rand:      func() float64 { return 0 },
	}
	req := mustPUTReq(t)
	_, err := rc.Do(req)
	if err == nil {
		t.Fatal("expected an error (404 not retried, classified)")
	}
	cerr := AsICloudError(err)
	if cerr == nil || cerr.Code != CodeNotFound {
		t.Errorf("err = %v, want typed not_found", err)
	}
	if doer.calls != 1 {
		t.Errorf("calls = %d, want 1 (404 must not be retried)", doer.calls)
	}
}

func TestRetryClassifier_AbortsOnContextDone(t *testing.T) {
	doer := &statusDoer{statuses: []int{http.StatusTooManyRequests}} // always 429
	rc := &retryClassifier{
		inner:     doer,
		maxTries:  100,
		baseDelay: 200 * time.Millisecond,
		maxDelay:  200 * time.Millisecond,
		now:       time.Now,
		rand:      func() float64 { return 0 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req := mustPUTReqWithCtx(t, ctx)
	_, err := rc.Do(req)
	if err == nil {
		t.Fatal("expected a context-cancellation error")
	}
	if doer.calls < 1 {
		t.Errorf("calls = %d, want >= 1", doer.calls)
	}
}

// TestRetryClassifier_ConcurrentClassifiesAll: many concurrent requests over
// the same retryClassifier against a busy server (always 503). Each must end
// with a typed server_unavailable error, with no panic and no shared-state
// corruption (-race).
func TestRetryClassifier_ConcurrentClassifiesAll(t *testing.T) {
	doer := &statusDoer{statuses: []int{http.StatusServiceUnavailable}}
	rc := &retryClassifier{
		inner:     doer,
		maxTries:  2,
		baseDelay: time.Millisecond,
		maxDelay:  2 * time.Millisecond,
		now:       time.Now,
		rand:      func() float64 { return 0 },
	}
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	var errCount atomic.Int32
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := rc.Do(mustPUTReq(t))
			if err != nil && AsICloudError(err) != nil {
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := errCount.Load(); got != n {
		t.Errorf("typed error count = %d, want %d", got, n)
	}
}

func mustPUTReq(t *testing.T) *http.Request {
	t.Helper()
	return mustPUTReqWithCtx(t, context.Background())
}

func mustPUTReqWithCtx(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://p42-caldav.icloud.com/cal/x.ics", strings.NewReader("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestClassifyStatus_MapsKnownCodes(t *testing.T) {
	cases := []struct {
		status int
		want   Code
	}{
		{http.StatusUnauthorized, CodeAuthenticationRefused},
		{http.StatusForbidden, CodeForbidden},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusPreconditionFailed, CodeConcurrentModification},
		{http.StatusTooManyRequests, CodeRateLimited},
		{http.StatusInternalServerError, CodeServerUnavailable},
		{http.StatusBadGateway, CodeServerUnavailable},
		{http.StatusServiceUnavailable, CodeServerUnavailable},
		{http.StatusGatewayTimeout, CodeServerUnavailable},
		{http.StatusConflict, CodeConflict},
	}
	for _, c := range cases {
		got := classifyStatus(c.status)
		if got.Code != c.want {
			t.Errorf("status %d: code = %q, want %q", c.status, got.Code, c.want)
		}
		if got.Status != c.status {
			t.Errorf("status %d: Status field = %d", c.status, got.Status)
		}
		if !strings.Contains(got.Error(), string(c.want)) {
			t.Errorf("status %d: Error() = %q, want it to start with %q", c.status, got.Error(), c.want)
		}
	}
}

func TestAsICloudError_UnwrapsWrappedError(t *testing.T) {
	inner := NewError(CodeConcurrentModification, 412, "mismatch", nil)
	// Negative control: a plain errors.New copying the text must NOT match.
	plain := errors.New("updating event (uid=x): " + inner.Error())
	if got := AsICloudError(plain); got != nil {
		t.Errorf("AsICloudError should not match a plain errors.New, got %+v", got)
	}
	// Real path: a fmt.Errorf("%w") chain, like the Client wraps errors.
	chained := fmt.Errorf("updating event (uid=x): %w", inner)
	got := AsICloudError(chained)
	if got == nil || got.Code != CodeConcurrentModification {
		t.Errorf("AsICloudError on a %%w chain = %+v, want code %q", got, CodeConcurrentModification)
	}
}

func TestNormalizeIfMatch(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"*", "*"},
		{"v1", `"v1"`},           // bare -> quoted
		{`"v1"`, `"v1"`},         // already quoted -> passthrough
		{`W/"v1"`, `W/"v1"`},     // weak -> passthrough
		{"abc-123", `"abc-123"`}, // bare with dash -> quoted
	}
	for _, c := range cases {
		if got := normalizeIfMatch(c.in); got != c.want {
			t.Errorf("normalizeIfMatch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRetryDelay_HTTPDateForm(t *testing.T) {
	// Retry-After as an HTTP-date in the past collapses to 0 (no negative
	// sleep); one 5s in the future is honored, capped by maxDelay.
	past := time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat)
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	resp := func(date string) *http.Response {
		r := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
		r.Header.Set("Retry-After", date)
		return r
	}
	if d := retryDelay(resp(past), 0, time.Second, 10*time.Second, time.Now, func() float64 { return 0 }); d != 0 {
		t.Errorf("past HTTP-date delay = %v, want 0", d)
	}
	d := retryDelay(resp(future), 0, time.Second, 10*time.Second, time.Now, func() float64 { return 0 })
	if d <= 0 || d > 10*time.Second {
		t.Errorf("future HTTP-date delay = %v, want within (0, 10s]", d)
	}
}

func TestNewRetryClassifierReturnsWorkingDoer(t *testing.T) {
	// The exported constructor must yield a doer that classifies non-retryable
	// statuses (smoke test of the production wiring path).
	inner := &statusDoer{statuses: []int{http.StatusUnauthorized}}
	rc := NewRetryClassifier(inner)
	_, err := rc.Do(mustPUTReq(t))
	if err == nil {
		t.Fatal("expected a classified auth error")
	}
	cerr := AsICloudError(err)
	if cerr == nil || cerr.Code != CodeAuthenticationRefused {
		t.Errorf("err = %v, want authentication_refused", err)
	}
}
