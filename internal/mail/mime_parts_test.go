package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

func plainMetadata(parts ...imapadapter.BodyPart) *fakeIMAP {
	return &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 3, UIDNext: 2},
		metadata: []imapadapter.Message{{
			UID: 1, Envelope: imapadapter.Envelope{Subject: "Body"}, Parts: parts,
		}},
	}
}

func TestGetMessageCombinesIndependentMixedPlainParts(t *testing.T) {
	t.Parallel()
	imap := plainMetadata(
		imapadapter.BodyPart{Path: []int{1}, ContentType: "text/plain", InlinePlain: true},
		imapadapter.BodyPart{Path: []int{2}, ContentType: "text/plain", InlinePlain: true},
	)
	imap.bodyByPath = map[string]imapadapter.BodyData{
		"1": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte("Please review the contract")},
		"2": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte("Deadline: Friday")},
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if !strings.Contains(message.Body, "Please review the contract") || !strings.Contains(message.Body, "Deadline: Friday") {
		t.Fatalf("mixed body = %q", message.Body)
	}
	if strings.Index(message.Body, "Please review") > strings.Index(message.Body, "Deadline") {
		t.Fatalf("mixed parts are out of order: %q", message.Body)
	}
	if message.BodyOmitted || message.BodySource != "plain_text" {
		t.Fatalf("mixed metadata = %#v", message)
	}
}

func TestGetMessageDoesNotConcatenateAlternativePlainParts(t *testing.T) {
	t.Parallel()
	imap := plainMetadata(
		imapadapter.BodyPart{Path: []int{1}, ContentType: "text/plain", InlinePlain: true, AlternativeGroup: "root"},
		imapadapter.BodyPart{Path: []int{2}, ContentType: "text/plain", InlinePlain: true, AlternativeGroup: "root"},
	)
	imap.bodyByPath = map[string]imapadapter.BodyData{
		"1": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte("preferred representation")},
		"2": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte("duplicate representation")},
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if message.Body != "preferred representation" {
		t.Fatalf("alternative body = %q", message.Body)
	}
	if strings.Count(strings.Join(imap.log, ","), "body-peek") != 1 {
		t.Fatalf("alternative parts were both fetched: %v", imap.log)
	}
}

func TestGetMessageMarksPlainPartsThatDoNotFit(t *testing.T) {
	t.Parallel()
	imap := plainMetadata(
		imapadapter.BodyPart{Path: []int{1}, ContentType: "text/plain", InlinePlain: true},
		imapadapter.BodyPart{Path: []int{2}, ContentType: "text/plain", InlinePlain: true},
	)
	imap.bodyByPath = map[string]imapadapter.BodyData{
		"1": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte(strings.Repeat("a", 40))},
		"2": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte(strings.Repeat("b", 40))},
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{
		Mailbox: "INBOX", UIDValidity: 3, UID: 1, MaxBodyBytes: 50,
	})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if !message.BodyOmitted || len(message.Body) > 50 {
		t.Fatalf("bounded body = %q omitted=%v", message.Body, message.BodyOmitted)
	}
	found := false
	for _, warning := range message.Warnings {
		if warning.Code == "body_truncated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing body_truncated warning: %#v", message.Warnings)
	}
}
