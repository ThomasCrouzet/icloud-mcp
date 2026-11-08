package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"github.com/emersion/go-smtp"
	"golang.org/x/time/rate"
)

// Send idempotency lifecycle tests.
//
// Fixture note: production send allows 20 operations per minute with a burst of
// 3. These tests replace the limiter with an infinite limiter so one test can
// reach the 128-entry cache boundary. They are not throughput measurements. The
// service clock is injected and never waits on wall time.
const lifecycleRetention = 15 * time.Minute

type lifecycleClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *lifecycleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *lifecycleClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(duration)
}

func (c *lifecycleClock) Set(value time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = value
}

func newLifecycleService(t *testing.T, imap *fakeIMAP, smtpFake *fakeSMTP) (*Client, *lifecycleClock) {
	t.Helper()
	service := testService(t, imap, smtpFake, false, true)
	clock := &lifecycleClock{current: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)}
	service.now = clock.Now
	service.sendLimiter = rate.NewLimiter(rate.Inf, 1)
	return service, clock
}

func lifecycleSend(service *Client, key, body string) (SendResult, error) {
	return service.SendMessage(context.Background(), SendInput{
		To:             []string{"to@example.invalid"},
		Subject:        "lifecycle",
		Body:           body,
		IdempotencyKey: key,
	})
}

func lifecycleMustSend(t *testing.T, service *Client, key, body string) SendResult {
	t.Helper()
	result, err := lifecycleSend(service, key, body)
	return mustLifecycleAccepted(t, result, err)
}

func lifecycleMustSendStatus(t *testing.T, service *Client, key, body string, status SendStatus) SendResult {
	t.Helper()
	result, err := lifecycleSend(service, key, body)
	return mustLifecycleAcceptedOrUnknown(t, result, err, status)
}

func lifecycleDataCount(log []string) int {
	count := 0
	for _, entry := range log {
		if entry == "data" {
			count++
		}
	}
	return count
}

func lifecycleEntryCount(service *Client) int {
	service.sendCache.mu.Lock()
	defer service.sendCache.mu.Unlock()
	return len(service.sendCache.entries)
}

func mustLifecycleAccepted(t *testing.T, result SendResult, err error) SendResult {
	t.Helper()
	if err != nil || result.Status != SendAccepted || result.MessageID == "" {
		t.Fatalf("send = status %q messageId %q err %v, want accepted", result.Status, result.MessageID, err)
	}
	return result
}

func mustLifecycleCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	public := AsError(err)
	if public == nil || public.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	return public
}

func TestSendIdempotencyDefinitiveSaturationRecoversAtRetention(t *testing.T) {
	var log []string
	service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})

	for index := 0; index < maxSendIdempotencyEntries; index++ {
		key := fmt.Sprintf("definitive-%03d", index)
		lifecycleMustSend(t, service, key, "body-"+key)
	}
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries {
		t.Fatalf("SMTP DATA = %d, want %d", got, maxSendIdempotencyEntries)
	}
	if got := lifecycleEntryCount(service); got != maxSendIdempotencyEntries {
		t.Fatalf("retained entries = %d, want %d", got, maxSendIdempotencyEntries)
	}

	clock.Advance(lifecycleRetention - time.Nanosecond)
	_, err := lifecycleSend(service, "definitive-128", "body-definitive-128")
	public := mustLifecycleCode(t, err, CodeConflict)
	if strings.Contains(public.Message, "new process") || strings.Contains(public.Message, "different key") {
		t.Fatalf("saturation message gives unsafe recovery advice: %q", public.Message)
	}
	if !strings.Contains(public.Message, "no SMTP submission was dispatched for this request") ||
		strings.Contains(public.Message, "for this key") ||
		!strings.Contains(public.Reconciliation, "Keep the same key") ||
		!strings.Contains(public.Reconciliation, "Check Sent") ||
		!strings.Contains(public.Reconciliation, "process exit") {
		t.Fatalf("saturation guidance is incomplete or key-history scoped: %+v", public)
	}
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries {
		t.Fatalf("saturated request dispatched SMTP: DATA = %d", got)
	}
	if got := lifecycleEntryCount(service); got != maxSendIdempotencyEntries {
		t.Fatalf("failed claim changed capacity: entries = %d", got)
	}

	clock.Advance(time.Nanosecond)
	lifecycleMustSend(t, service, "definitive-128", "body-definitive-128")
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries+1 {
		t.Fatalf("SMTP DATA after retention = %d, want %d", got, maxSendIdempotencyEntries+1)
	}
	if got := lifecycleEntryCount(service); got != 1 {
		t.Fatalf("entries after retention sweep = %d, want 1", got)
	}
}

func TestSendIdempotencyExpiryBoundaryAndOldKeyContract(t *testing.T) {
	var log []string
	service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})

	firstA := lifecycleMustSend(t, service, "key-a", "body-a")
	firstB := lifecycleMustSend(t, service, "key-b", "body-b")
	if got := lifecycleDataCount(log); got != 2 {
		t.Fatalf("initial SMTP DATA = %d, want 2", got)
	}

	clock.Advance(lifecycleRetention - time.Nanosecond)
	retainedA := lifecycleMustSend(t, service, "key-a", "body-a")
	if retainedA.MessageID != firstA.MessageID {
		t.Fatalf("retained key changed Message-ID: %q / %q", retainedA.MessageID, firstA.MessageID)
	}
	_, err := lifecycleSend(service, "key-b", "changed-b")
	mustLifecycleCode(t, err, CodeConflict)
	if got := lifecycleDataCount(log); got != 2 {
		t.Fatalf("retained reuse or conflict dispatched SMTP: DATA = %d", got)
	}

	clock.Advance(time.Nanosecond)
	expiredA := lifecycleMustSend(t, service, "key-a", "body-a")
	if expiredA.MessageID == firstA.MessageID {
		t.Fatalf("expired key reused Message-ID %q", expiredA.MessageID)
	}
	expiredB := lifecycleMustSend(t, service, "key-b", "changed-b")
	if expiredB.MessageID == firstB.MessageID {
		t.Fatalf("expired conflicting content reused Message-ID %q", expiredB.MessageID)
	}
	if got := lifecycleDataCount(log); got != 4 {
		t.Fatalf("SMTP DATA after expiry = %d, want 4", got)
	}
}

func TestSendIdempotencyRetentionStartsAtCompletion(t *testing.T) {
	var log []string
	started := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	fake := &fakeSMTP{log: &log, dataStarted: true, dataFn: func() error {
		blockOnce.Do(func() {
			close(started)
			<-release
		})
		return nil
	}}
	service, clock := newLifecycleService(t, nil, fake)

	type outcome struct {
		result SendResult
		err    error
	}
	owner := make(chan outcome, 1)
	go func() {
		result, err := lifecycleSend(service, "completion", "body-completion")
		owner <- outcome{result, err}
	}()
	<-started
	clock.Advance(10 * time.Minute)
	close(release)
	completed := <-owner
	if completed.err != nil || completed.result.Status != SendAccepted {
		t.Fatalf("owner completion = %+v %v", completed.result, completed.err)
	}

	clock.Advance(14 * time.Minute)
	retained := lifecycleMustSend(t, service, "completion", "body-completion")
	if retained.MessageID != completed.result.MessageID {
		t.Fatalf("completion retention changed Message-ID: %q / %q", retained.MessageID, completed.result.MessageID)
	}
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("completion retention dispatched SMTP: DATA = %d", got)
	}

	clock.Advance(time.Minute)
	expired := lifecycleMustSend(t, service, "completion", "body-completion")
	if expired.MessageID == completed.result.MessageID {
		t.Fatalf("completion boundary reused Message-ID %q", expired.MessageID)
	}
	if got := lifecycleDataCount(log); got != 2 {
		t.Fatalf("completion boundary did not dispatch: DATA = %d", got)
	}
}

func TestSendIdempotencyClockRollbackDoesNotExpire(t *testing.T) {
	var log []string
	service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})
	first := lifecycleMustSend(t, service, "rollback", "body-rollback")
	clock.Advance(-time.Hour)
	retained := lifecycleMustSend(t, service, "rollback", "body-rollback")
	if retained.MessageID != first.MessageID {
		t.Fatalf("clock rollback expired a retained key: %q / %q", retained.MessageID, first.MessageID)
	}
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("clock rollback dispatched SMTP: DATA = %d", got)
	}
}

func TestSendIdempotencyAmbiguousOutcomeNeverExpires(t *testing.T) {
	var log []string
	service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log, dataStarted: true, dataErr: io.ErrUnexpectedEOF})

	for index := 0; index < maxSendIdempotencyEntries; index++ {
		key := fmt.Sprintf("ambiguous-%03d", index)
		result, err := lifecycleSend(service, key, "body-"+key)
		mustLifecycleCode(t, err, CodeOutcomeUnknown)
		if result.Status != SendOutcomeUnknown || result.MessageID == "" {
			t.Fatalf("ambiguous result %d = %+v", index, result)
		}
	}
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries {
		t.Fatalf("ambiguous SMTP DATA = %d, want %d", got, maxSendIdempotencyEntries)
	}

	clock.Advance(24 * time.Hour)
	cached, err := lifecycleSend(service, "ambiguous-000", "body-ambiguous-000")
	mustLifecycleCode(t, err, CodeOutcomeUnknown)
	if cached.Status != SendOutcomeUnknown {
		t.Fatalf("cached ambiguous status = %q", cached.Status)
	}
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries {
		t.Fatalf("ambiguous key dispatched again: DATA = %d", got)
	}

	_, err = lifecycleSend(service, "ambiguous-128", "body-ambiguous-128")
	public := mustLifecycleCode(t, err, CodeConflict)
	if !strings.Contains(public.Message, "no SMTP submission was dispatched for this request") ||
		strings.Contains(public.Message, "for this key") {
		t.Fatalf("ambiguous saturation message is not request scoped: %q", public.Message)
	}
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries {
		t.Fatalf("ambiguous saturation dispatched SMTP: DATA = %d", got)
	}
	if got := lifecycleEntryCount(service); got != maxSendIdempotencyEntries {
		t.Fatalf("ambiguous entries changed: %d", got)
	}
}

func TestSendIdempotencyMixedSaturationReclaimsOnlyDefinitive(t *testing.T) {
	var log []string
	fake := &fakeSMTP{log: &log, dataStarted: true}
	fake.dataFn = func() error {
		if strings.Contains(string(fake.message), "ambiguous") {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	service, clock := newLifecycleService(t, nil, fake)

	for index := 0; index < maxSendIdempotencyEntries-1; index++ {
		key := fmt.Sprintf("definitive-%03d", index)
		lifecycleMustSend(t, service, key, "body-"+key)
	}
	ambiguous := lifecycleMustSendStatus(t, service, "ambiguous-127", "body-ambiguous-127", SendOutcomeUnknown)
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries {
		t.Fatalf("mixed SMTP DATA = %d, want %d", got, maxSendIdempotencyEntries)
	}

	clock.Advance(lifecycleRetention)
	lifecycleMustSend(t, service, "definitive-127", "body-definitive-127")
	if got := lifecycleEntryCount(service); got != 2 {
		t.Fatalf("mixed entries after sweep = %d, want 2", got)
	}
	cached, err := lifecycleSend(service, "ambiguous-127", "body-ambiguous-127")
	mustLifecycleCode(t, err, CodeOutcomeUnknown)
	if cached.MessageID != ambiguous.MessageID {
		t.Fatalf("mixed ambiguous Message-ID changed: %q / %q", cached.MessageID, ambiguous.MessageID)
	}
	if got := lifecycleDataCount(log); got != maxSendIdempotencyEntries+1 {
		t.Fatalf("mixed SMTP DATA = %d, want %d", got, maxSendIdempotencyEntries+1)
	}
}

func mustLifecycleAcceptedOrUnknown(t *testing.T, result SendResult, err error, status SendStatus) SendResult {
	t.Helper()
	if status == SendOutcomeUnknown {
		mustLifecycleCode(t, err, CodeOutcomeUnknown)
	} else if err != nil {
		t.Fatalf("send error = %v", err)
	}
	if result.Status != status || result.MessageID == "" {
		t.Fatalf("send = status %q messageId %q, want %q", result.Status, result.MessageID, status)
	}
	return result
}

func TestSendIdempotencyPendingClaimNeverExpires(t *testing.T) {
	var log []string
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeSMTP{log: &log, dataStarted: true, dataFn: func() error {
		close(started)
		<-release
		return nil
	}}
	service, clock := newLifecycleService(t, nil, fake)

	type outcome struct {
		result SendResult
		err    error
	}
	owner := make(chan outcome, 1)
	go func() {
		result, err := lifecycleSend(service, "pending", "body-pending")
		owner <- outcome{result, err}
	}()
	<-started
	clock.Advance(24 * time.Hour)

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.SendMessage(waitCtx, SendInput{
		To: []string{"to@example.invalid"}, Subject: "lifecycle", Body: "body-pending", IdempotencyKey: "pending",
	})
	mustLifecycleCode(t, err, CodeTimeout)

	close(release)
	completed := <-owner
	if completed.err != nil || completed.result.Status != SendAccepted {
		t.Fatalf("pending owner = %+v %v", completed.result, completed.err)
	}
	cached := lifecycleMustSend(t, service, "pending", "body-pending")
	if cached.MessageID != completed.result.MessageID {
		t.Fatalf("pending claim was released: %q / %q", cached.MessageID, completed.result.MessageID)
	}
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("pending claim dispatched again: DATA = %d", got)
	}
	if got := lifecycleEntryCount(service); got != 1 {
		t.Fatalf("pending entries = %d, want 1", got)
	}
}

func TestSendIdempotencyPreDispatchDefinitiveFailuresExpire(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		var log []string
		service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})
		_, err := lifecycleSend(service, "validation", "bad\x00body")
		mustLifecycleCode(t, err, CodeValidation)
		if got := lifecycleEntryCount(service); got != 1 {
			t.Fatalf("validation entries = %d, want 1", got)
		}
		clock.Advance(lifecycleRetention)
		lifecycleMustSend(t, service, "validation", "good body")
		if got := lifecycleDataCount(log); got != 1 {
			t.Fatalf("validation did not expire: DATA = %d", got)
		}
	})

	t.Run("authorization", func(t *testing.T) {
		var log []string
		service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})
		service.sendEnabled = false
		_, err := lifecycleSend(service, "authorization", "body")
		mustLifecycleCode(t, err, CodeAuthorization)
		if got := lifecycleEntryCount(service); got != 1 {
			t.Fatalf("authorization entries = %d, want 1", got)
		}
		clock.Advance(lifecycleRetention)
		service.sendEnabled = true
		lifecycleMustSend(t, service, "authorization", "body")
		if got := lifecycleDataCount(log); got != 1 {
			t.Fatalf("authorization did not expire: DATA = %d", got)
		}
	})

	t.Run("rate limit", func(t *testing.T) {
		var log []string
		service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})
		service.sendLimiter = rate.NewLimiter(rate.Every(time.Hour), 0)
		_, err := lifecycleSend(service, "rate", "body")
		mustLifecycleCode(t, err, CodeRateLimited)
		if got := lifecycleEntryCount(service); got != 1 {
			t.Fatalf("rate entries = %d, want 1", got)
		}
		clock.Advance(lifecycleRetention)
		service.sendLimiter = rate.NewLimiter(rate.Inf, 1)
		lifecycleMustSend(t, service, "rate", "body")
		if got := lifecycleDataCount(log); got != 1 {
			t.Fatalf("rate limit did not expire: DATA = %d", got)
		}
	})
}

func TestSendIdempotencyDefinitiveSMTPRejectionExpires(t *testing.T) {
	t.Run("recipient", func(t *testing.T) {
		var log []string
		fake := &fakeSMTP{log: &log, rcptErrors: map[string]error{
			"bad@example.invalid": &smtp.SMTPError{Code: 550, Message: "rejected"},
		}}
		service, clock := newLifecycleService(t, nil, fake)
		_, err := service.SendMessage(context.Background(), SendInput{
			To: []string{"bad@example.invalid"}, Body: "body", IdempotencyKey: "recipient",
		})
		mustLifecycleCode(t, err, CodeAuthorization)
		if got := lifecycleDataCount(log); got != 0 {
			t.Fatalf("recipient rejection reached DATA: %d", got)
		}
		clock.Advance(lifecycleRetention)
		lifecycleMustSend(t, service, "recipient", "body")
		if got := lifecycleDataCount(log); got != 1 {
			t.Fatalf("recipient rejection did not expire: DATA = %d", got)
		}
	})

	t.Run("data", func(t *testing.T) {
		var log []string
		fake := &fakeSMTP{log: &log, dataStarted: true, dataErr: &smtp.SMTPError{Code: 550, Message: "rejected"}}
		service, clock := newLifecycleService(t, nil, fake)
		result, err := lifecycleSend(service, "data-rejection", "body")
		mustLifecycleCode(t, err, CodeAuthorization)
		if result.Status != SendRejected {
			t.Fatalf("definitive DATA rejection status = %q", result.Status)
		}
		if got := lifecycleDataCount(log); got != 1 {
			t.Fatalf("definitive DATA rejection count = %d", got)
		}
		fake.dataErr = nil
		clock.Advance(lifecycleRetention)
		lifecycleMustSend(t, service, "data-rejection", "body")
		if got := lifecycleDataCount(log); got != 2 {
			t.Fatalf("definitive DATA rejection did not expire: DATA = %d", got)
		}
	})
}

func TestSendIdempotencyPanicClassification(t *testing.T) {
	t.Run("after DATA", func(t *testing.T) {
		var log []string
		fake := &fakeSMTP{log: &log, dataStarted: true, dataFn: func() error {
			panic("synthetic parser panic")
		}}
		service, clock := newLifecycleService(t, nil, fake)
		result, err := lifecycleSend(service, "panic-after", "body")
		mustLifecycleCode(t, err, CodeOutcomeUnknown)
		if result.Status != SendOutcomeUnknown {
			t.Fatalf("panic after DATA status = %q", result.Status)
		}
		clock.Advance(24 * time.Hour)
		cached, err := lifecycleSend(service, "panic-after", "body")
		mustLifecycleCode(t, err, CodeOutcomeUnknown)
		if cached.MessageID != result.MessageID {
			t.Fatalf("panic after DATA Message-ID changed: %q / %q", cached.MessageID, result.MessageID)
		}
		if got := lifecycleDataCount(log); got != 1 {
			t.Fatalf("panic after DATA dispatched again: DATA = %d", got)
		}
	})

	t.Run("before DATA", func(t *testing.T) {
		var log []string
		fake := &fakeSMTP{log: &log, dataStarted: false, dataFn: func() error {
			panic("synthetic parser panic")
		}}
		service, clock := newLifecycleService(t, nil, fake)
		result, err := lifecycleSend(service, "panic-before", "body")
		mustLifecycleCode(t, err, CodeProtocolError)
		if result.Status != SendRejected {
			t.Fatalf("panic before DATA status = %q", result.Status)
		}
		fake.dataFn = nil
		clock.Advance(lifecycleRetention)
		lifecycleMustSend(t, service, "panic-before", "body")
		if got := lifecycleDataCount(log); got != 2 {
			t.Fatalf("panic before DATA did not expire: DATA = %d", got)
		}
	})
}

func TestSendIdempotencyUnkeyedPathDoesNotUseCache(t *testing.T) {
	var log []string
	service, _ := newLifecycleService(t, nil, &fakeSMTP{log: &log})
	for index := 0; index < 2; index++ {
		result, err := service.SendMessage(context.Background(), SendInput{
			To: []string{"to@example.invalid"}, Body: "body",
		})
		mustLifecycleAccepted(t, result, err)
	}
	if got := lifecycleEntryCount(service); got != 0 {
		t.Fatalf("unkeyed sends created %d entries", got)
	}
	if got := lifecycleDataCount(log); got != 2 {
		t.Fatalf("unkeyed SMTP DATA = %d, want 2", got)
	}
}

func TestSendIdempotencyFingerprintRecipientOrderAndGroups(t *testing.T) {
	var log []string
	service, _ := newLifecycleService(t, nil, &fakeSMTP{log: &log})
	first, err := service.SendMessage(context.Background(), SendInput{
		To: []string{"a@example.invalid", "b@example.invalid"}, Cc: []string{"c@example.invalid"},
		Body: "body", IdempotencyKey: "order",
	})
	mustLifecycleAccepted(t, first, err)

	reversed := SendInput{
		To: []string{"b@example.invalid", "a@example.invalid"}, Cc: []string{"c@example.invalid"},
		Body: "body", IdempotencyKey: "order",
	}
	_, err = service.SendMessage(context.Background(), reversed)
	mustLifecycleCode(t, err, CodeConflict)

	grouped := SendInput{
		To: []string{"a@example.invalid"}, Cc: []string{"b@example.invalid", "c@example.invalid"},
		Body: "body", IdempotencyKey: "order",
	}
	_, err = service.SendMessage(context.Background(), grouped)
	mustLifecycleCode(t, err, CodeConflict)

	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("fingerprint conflict dispatched SMTP: DATA = %d", got)
	}
}

func TestReplyMessageKeyedReusesAfterRealSourceValidation(t *testing.T) {
	var log []string
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 7, UIDNext: 3},
		metadata: []imapadapter.Message{{
			UID: 2,
			Envelope: imapadapter.Envelope{
				Subject: "Original subject", MessageID: "<source@example.invalid>",
				From: []imapadapter.Address{{Address: "from@example.invalid"}},
			},
			Headers: imapadapter.CuratedHeaders{
				MessageID: "<source@example.invalid>",
			},
		}},
	}
	service, _ := newLifecycleService(t, imap, &fakeSMTP{log: &log})
	input := ReplyInput{Mailbox: "INBOX", UIDValidity: 7, UID: 2, Body: "reply body", IdempotencyKey: "reply-key"}
	first, err := service.ReplyMessage(context.Background(), input)
	mustLifecycleAccepted(t, first, err)
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("first reply SMTP DATA = %d", got)
	}

	second, err := service.ReplyMessage(context.Background(), input)
	mustLifecycleAccepted(t, second, err)
	if second.MessageID != first.MessageID {
		t.Fatalf("keyed reply changed Message-ID: %q / %q", second.MessageID, first.MessageID)
	}
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("keyed reply dispatched again: DATA = %d", got)
	}

	imap.metadata[0].Envelope.Subject = "Changed subject"
	_, err = service.ReplyMessage(context.Background(), input)
	mustLifecycleCode(t, err, CodeConflict)
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("changed derived reply dispatched again: DATA = %d", got)
	}

	input.UIDValidity = 6
	_, err = service.ReplyMessage(context.Background(), input)
	mustLifecycleCode(t, err, CodeConcurrentModification)
	if got := lifecycleDataCount(log); got != 1 {
		t.Fatalf("invalid reply source dispatched again: DATA = %d", got)
	}
}

func TestSendClaimIsDefinitiveClassification(t *testing.T) {
	tests := []struct {
		name   string
		result SendResult
		err    error
		want   bool
	}{
		{name: "accepted", result: SendResult{Status: SendAccepted}, want: true},
		{name: "definitive rejected", result: SendResult{Status: SendRejected}, err: validationError("invalid"), want: true},
		{name: "definitive internal rejection", result: SendResult{Status: SendRejected}, err: newError(CodeInternalError, "build failed"), want: true},
		{name: "pre-dispatch authorization", result: SendResult{}, err: newError(CodeAuthorization, "disabled"), want: true},
		{name: "pre-dispatch rate limit", result: SendResult{}, err: newRetryableError(CodeRateLimited, "limited"), want: true},
		{name: "unknown status", result: SendResult{Status: SendOutcomeUnknown}, err: newError(CodeOutcomeUnknown, "unknown"), want: false},
		{name: "unknown code with rejected status", result: SendResult{Status: SendRejected}, err: newError(CodeOutcomeUnknown, "unknown"), want: false},
		{name: "unclassified rejected code", result: SendResult{Status: SendRejected}, err: newError(CodeConflict, "unexpected"), want: false},
		{name: "unclassified internal error", result: SendResult{}, err: newError(CodeInternalError, "unexpected"), want: false},
		{name: "non-public error", result: SendResult{Status: SendRejected}, err: errors.New("unexpected"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sendClaimIsDefinitive(test.result, test.err); got != test.want {
				t.Fatalf("sendClaimIsDefinitive(%+v, %v) = %v, want %v", test.result, test.err, got, test.want)
			}
		})
	}
}

func TestSendIdempotencyRepeatedDefinitiveBatches(t *testing.T) {
	var log []string
	service, clock := newLifecycleService(t, nil, &fakeSMTP{log: &log})
	for batch := 0; batch < 2; batch++ {
		for index := 0; index < maxSendIdempotencyEntries; index++ {
			key := fmt.Sprintf("batch-%d-%03d", batch, index)
			lifecycleMustSend(t, service, key, "body-"+key)
		}
		if got := lifecycleDataCount(log); got != (batch+1)*maxSendIdempotencyEntries {
			t.Fatalf("batch %d SMTP DATA = %d", batch, got)
		}
		clock.Advance(lifecycleRetention)
	}
	lifecycleMustSend(t, service, "batch-2-000", "body-batch-2-000")
	if got := lifecycleDataCount(log); got != 2*maxSendIdempotencyEntries+1 {
		t.Fatalf("third batch SMTP DATA = %d, want %d", got, 2*maxSendIdempotencyEntries+1)
	}
	if got := lifecycleEntryCount(service); got != 1 {
		t.Fatalf("third batch entries = %d, want 1", got)
	}
}
