package mail

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
)

// submittedPart is one attachment part of the message that SMTP received.
type submittedPart struct {
	contentType, disposition string
	data                     []byte
}

// submittedAttachments parses the last SMTP DATA payload of the fixture and
// returns the raw headers and decoded bytes of each attachment part.
func (f *attachmentFixture) submittedAttachments(t *testing.T) []submittedPart {
	t.Helper()
	f.smtp.mu.Lock()
	submitted := append([]byte(nil), f.smtp.submitted...)
	f.smtp.mu.Unlock()
	entity, err := message.Read(bytes.NewReader(submitted))
	if err != nil {
		t.Fatal(err)
	}
	reader := entity.MultipartReader()
	if reader == nil {
		t.Fatal("submitted message is not multipart")
	}
	var parts []submittedPart
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		disposition := part.Header.Get("Content-Disposition")
		if !strings.HasPrefix(disposition, "attachment") {
			continue
		}
		data, err := io.ReadAll(part.Body)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, submittedPart{contentType: part.Header.Get("Content-Type"), disposition: disposition, data: data})
	}
	return parts
}

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

func TestProtocolAttachmentFilenameUsesRFC2231(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, send: true})
	docx, pngPart := f.partFor(t, "Offerte & prijs \u00e9t\u00e9.docx"), f.partFor(t, "grafiek.png")
	result, err := f.client.SendMessage(t.Context(), SendInput{To: []string{"to@example.invalid"}, Body: "x", Attachments: []AttachmentRef{f.ref(docx), f.ref(pngPart)}})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send = %+v, %v", result, err)
	}
	parts := f.submittedAttachments(t)
	if len(parts) != 2 {
		t.Fatalf("attachment parts = %d", len(parts))
	}
	for i, want := range []string{
		"attachment; filename*=utf-8''Offerte%20&%20prijs%20%C3%A9t%C3%A9.docx",
		"attachment; filename=grafiek.png",
	} {
		if parts[i].disposition != want {
			t.Errorf("Content-Disposition %d = %q, want %q", i, parts[i].disposition, want)
		}
	}
	// The legacy name parameter stays for clients that read only Content-Type.
	if _, params, err := mime.ParseMediaType(parts[1].contentType); err != nil || params["name"] != "grafiek.png" {
		t.Errorf("Content-Type = %q, %v", parts[1].contentType, err)
	}
}

func TestProtocolAttachmentInvalidContentTypeFallsBack(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, send: true, extraParts: []fixturePart{
		{filename: "report.pdf", contentType: "application/pdf@x", data: []byte("%PDF-1.4 synthetic")},
		{filename: "blob.bin", contentType: "no type", data: []byte{0, 1, 2, 3}},
	}})
	refs := []AttachmentRef{f.ref(f.partFor(t, "report.pdf")), f.ref(f.partFor(t, "blob.bin"))}
	result, err := f.client.SendMessage(t.Context(), SendInput{To: []string{"to@example.invalid"}, Body: "x", Attachments: refs})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send = %+v, %v", result, err)
	}
	parts := f.submittedAttachments(t)
	if len(parts) != 2 {
		t.Fatalf("attachment parts = %d", len(parts))
	}
	for i, want := range []string{"application/pdf", "application/octet-stream"} {
		mediaType, _, err := mime.ParseMediaType(parts[i].contentType)
		if err != nil || mediaType != want {
			t.Errorf("Content-Type %d = %q, %v; want %s", i, parts[i].contentType, err, want)
		}
	}
	if !bytes.Equal(parts[0].data, f.files["report.pdf"]) || !bytes.Equal(parts[1].data, f.files["blob.bin"]) {
		t.Error("submitted attachment bytes differ")
	}
}

func (f *attachmentFixture) draftCount(t *testing.T) int {
	t.Helper()
	drafts, err := f.client.SearchMessages(t.Context(), SearchInput{Mailbox: "Drafts"})
	if err != nil {
		t.Fatal(err)
	}
	return len(drafts.Messages)
}

func TestProtocolDraftRecipientsFollowConfiguredAllowlist(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, send: true, drafts: true, draftsMailbox: "Drafts"})
	for _, input := range []DraftInput{
		{To: []string{"anyone@example.invalid"}, Body: "x"},
		{To: []string{"to@example.invalid"}, Cc: []string{"anyone@example.invalid"}, Body: "x"},
	} {
		_, err := f.client.CreateDraft(t.Context(), input)
		wantCode(t, err, CodeAuthorization)
	}
	if count := f.draftCount(t); count != 0 {
		t.Fatalf("a draft to a recipient outside the allowlist was appended: %d", count)
	}
	draft, err := f.client.CreateDraft(t.Context(), DraftInput{To: []string{"to@EXAMPLE.invalid"}, Body: "x"})
	if err != nil || draft.Status != "stored" {
		t.Fatalf("draft to an allowed recipient = %+v, %v", draft, err)
	}
	// A draft still needs no recipient.
	if draft, err := f.client.CreateDraft(t.Context(), DraftInput{Body: "x"}); err != nil || draft.Status != "stored" {
		t.Fatalf("draft without recipients = %+v, %v", draft, err)
	}
}

func TestProtocolDraftRecipientsWithoutAllowlist(t *testing.T) {
	// Without a configured allowlist, a drafts-only profile keeps any
	// recipient. docs/security.md records this residual risk.
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, drafts: true, draftsMailbox: "Drafts", noRecipientPolicy: true})
	draft, err := f.client.CreateDraft(t.Context(), DraftInput{To: []string{"anyone@example.invalid"}, Body: "x"})
	if err != nil || draft.Status != "stored" {
		t.Fatalf("draft without an allowlist = %+v, %v", draft, err)
	}
}
