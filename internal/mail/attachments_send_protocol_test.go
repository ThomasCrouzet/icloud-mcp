package mail

import (
	"testing"
	"time"
)

func TestProtocolAttachmentFailureDoesNotPinIdempotencyKey(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, send: true})
	base := time.Now()
	clock := base
	f.client.now = func() time.Time { return clock }
	good := f.ref(f.partFor(t, "grafiek.png"))
	stale := good
	stale.UIDValidity++
	input := SendInput{To: []string{"to@example.invalid"}, Body: "x", IdempotencyKey: "stale-reference", Attachments: []AttachmentRef{stale}}
	_, err := f.client.SendMessage(t.Context(), input)
	wantCode(t, err, CodeConcurrentModification)
	// The definitive failure stays for the retention window, so the same key
	// with other content conflicts.
	input.Attachments = []AttachmentRef{good}
	_, err = f.client.SendMessage(t.Context(), input)
	wantCode(t, err, CodeConflict)
	// After the window, the corrected reference can use the same key.
	clock = base.Add(sendIdempotencyDefinitiveRetention + time.Second)
	result, err := f.client.SendMessage(t.Context(), input)
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send after retention = %+v, %v", result, err)
	}
	f.smtp.mu.Lock()
	submitted := len(f.smtp.submitted)
	f.smtp.mu.Unlock()
	if submitted == 0 {
		t.Fatal("the corrected send reached no SMTP DATA")
	}
}
