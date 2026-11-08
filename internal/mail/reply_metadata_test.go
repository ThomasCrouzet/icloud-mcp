package mail

import (
	"context"
	"fmt"
	stdmail "net/mail"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

func replyFixture(headers imapadapter.CuratedHeaders, envelope imapadapter.Envelope) *fakeIMAP {
	envelope.MessageID = headers.MessageID
	return &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 12, UIDNext: 5},
		metadata: []imapadapter.Message{{UID: 4, Envelope: envelope, Headers: headers, Parts: []imapadapter.BodyPart{
			{Path: []int{1}, ContentType: "text/plain", InlinePlain: true},
		}}},
		bodyByPath: map[string]imapadapter.BodyData{
			"1": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte(strings.Repeat("body", 1000))},
		},
	}
}

func TestReplyNeverFetchesMessageBody(t *testing.T) {
	t.Parallel()
	var log []string
	fakeSMTP := &fakeSMTP{log: &log}
	imap := replyFixture(imapadapter.CuratedHeaders{MessageID: "<source@example.com>"}, imapadapter.Envelope{
		Subject: "Subject",
		From:    []imapadapter.Address{{Address: "author@example.com"}},
	})
	service := testService(t, imap, fakeSMTP, false, true)
	if _, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 12, UID: 4, Body: "reply",
	}); err != nil {
		t.Fatalf("ReplyMessage() error = %v", err)
	}
	if strings.Contains(strings.Join(imap.log, ","), "body-peek") {
		t.Fatalf("reply fetched a body part: %v", imap.log)
	}
}

func TestReplyUsesInReplyToWhenReferencesAreAbsent(t *testing.T) {
	t.Parallel()
	var log []string
	fakeSMTP := &fakeSMTP{log: &log}
	imap := replyFixture(imapadapter.CuratedHeaders{
		MessageID: "<b@example.com>", InReplyTo: []string{"<a@example.com>"},
	}, imapadapter.Envelope{
		Subject: "Subject",
		From:    []imapadapter.Address{{Address: "author@example.com"}},
	})
	service := testService(t, imap, fakeSMTP, false, true)
	if _, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 12, UID: 4, Body: "reply",
	}); err != nil {
		t.Fatalf("ReplyMessage() error = %v", err)
	}
	message := string(fakeSMTP.message)
	if !strings.Contains(message, "In-Reply-To: <b@example.com>") || !strings.Contains(message, "References: <a@example.com> <b@example.com>") {
		t.Fatalf("threading headers:\n%s", message)
	}
}

func TestReplyBoundsLongReferenceChain(t *testing.T) {
	t.Parallel()
	var log []string
	fakeSMTP := &fakeSMTP{log: &log}
	var references []string
	for index := 0; index < 30; index++ {
		references = append(references, fmt.Sprintf("<r%d@example.com>", index))
	}
	imap := replyFixture(imapadapter.CuratedHeaders{
		MessageID: "<source@example.com>", References: references,
	}, imapadapter.Envelope{
		Subject: "Subject",
		From:    []imapadapter.Address{{Address: "author@example.com"}},
	})
	service := testService(t, imap, fakeSMTP, false, true)
	if _, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 12, UID: 4, Body: "reply",
	}); err != nil {
		t.Fatalf("long thread reply error = %v", err)
	}
	parsed, err := stdmail.ReadMessage(strings.NewReader(string(fakeSMTP.message)))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	ids := strings.Fields(parsed.Header.Get("References"))
	if len(ids) != maxReplyReferences {
		t.Fatalf("References entries = %d, want %d", len(ids), maxReplyReferences)
	}
	if ids[len(ids)-1] != "<source@example.com>" || ids[len(ids)-2] != "<r29@example.com>" {
		t.Fatalf("bounded chain lost the recent ancestry: %v", ids)
	}
}

func TestReplyRejectsMalformedReferenceBeforeSMTP(t *testing.T) {
	t.Parallel()
	var log []string
	fakeSMTP := &fakeSMTP{log: &log}
	imap := replyFixture(imapadapter.CuratedHeaders{
		MessageID: "<source@example.com>", References: []string{"<bad id@example.com>"},
	}, imapadapter.Envelope{
		Subject: "Subject",
		From:    []imapadapter.Address{{Address: "author@example.com"}},
	})
	service := testService(t, imap, fakeSMTP, false, true)
	_, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 12, UID: 4, Body: "reply",
	})
	if errorCode(t, err) != CodeValidation {
		t.Fatalf("malformed reference error = %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("malformed reference reached SMTP: %v", log)
	}
}
