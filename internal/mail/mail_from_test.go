package mail

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

func TestSendUsesConfiguredFromButAuthenticatesWithAddress(t *testing.T) {
	t.Parallel()
	policy, err := ParseRecipientPolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	fake := &fakeSMTP{log: &log}
	imap := &fakeIMAP{}
	service, err := newService(Config{Address: "sender@icloud.com", From: "Alias@Example.com", Password: "app-password", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
		func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
		false, true,
		func(context.Context, net.Conn, string, string) (imapSession, error) { return imap, nil },
		func(context.Context, net.Conn) (smtpSession, error) { return fake, nil })
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	result, err := service.SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Subject: "s", Body: "body"})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send = %+v, %v", result, err)
	}
	if fake.authUser == "Alias@Example.com" || !strings.Contains(fake.authUser, "sender") {
		t.Fatalf("SMTP AUTH user = %q, want the login address", fake.authUser)
	}
	if fake.from != "Alias@Example.com" {
		t.Fatalf("MAIL FROM = %q, want the configured sender", fake.from)
	}
	msg := string(fake.message)
	if !strings.Contains(msg, "From: <Alias@Example.com>\r\n") || strings.Contains(msg, "sender@icloud.com") {
		t.Fatalf("message headers do not use only the configured sender:\n%s", msg)
	}
}

func TestNewServiceRejectsInvalidFrom(t *testing.T) {
	t.Parallel()
	policy, err := ParseRecipientPolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"not-an-address", "Alias <alias@example.com>", "a@example.com\r\nBcc: x@example.com", "élise@custom.example"} {
		_, err := newService(Config{Address: "sender@icloud.com", From: from, Password: "app-password", RecipientPolicy: policy},
			func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
			func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
			false, true, nil, nil)
		if err == nil {
			t.Fatalf("From %q was accepted", from)
		}
	}
}

func TestSendWithoutFromKeepsAddressAsSender(t *testing.T) {
	t.Parallel()
	var log []string
	fake := &fakeSMTP{log: &log}
	service := testService(t, nil, fake, false, true)
	result, err := service.SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Subject: "s", Body: "body"})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send = %+v, %v", result, err)
	}
	if fake.authUser != "sender@icloud.com" || fake.from != "sender@icloud.com" {
		t.Fatalf("AUTH/MAIL FROM = %q/%q, want the login address for both", fake.authUser, fake.from)
	}
	if !strings.Contains(string(fake.message), "From: <sender@icloud.com>\r\n") {
		t.Fatalf("From header changed without ICLOUD_MAIL_FROM:\n%s", fake.message)
	}
}

func TestReplyAllExcludesLoginAndFrom(t *testing.T) {
	t.Parallel()
	var log []string
	fake := &fakeSMTP{log: &log}
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 9, UIDNext: 2},
		metadata: []imapadapter.Message{{
			UID: 1,
			Envelope: imapadapter.Envelope{
				Subject: "Subject", MessageID: "<source@example.com>",
				From: []imapadapter.Address{{Address: "author@example.com"}},
				To:   []imapadapter.Address{{Address: "alias@CUSTOM.example"}, {Address: "sender@icloud.com"}, {Address: "other@example.com"}},
			},
			Headers: imapadapter.CuratedHeaders{MessageID: "<source@example.com>"},
		}},
	}
	service := testService(t, imap, fake, false, true)
	service.config.From = "alias@custom.example"
	result, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 9, UID: 1, Mode: "reply_all", Body: "body",
	})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("reply_all = %+v, %v", result, err)
	}
	joined := strings.Join(log, ",")
	for _, self := range []string{"rcpt:alias@", "rcpt:sender@icloud.com"} {
		if strings.Contains(joined, self) {
			t.Fatalf("own identity %q kept as a reply-all recipient: %s", self, joined)
		}
	}
	for _, want := range []string{"rcpt:author@example.com", "rcpt:other@example.com"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing recipient %q: %s", want, joined)
		}
	}
}

func TestSentCopyMatchesSubmittedMessageWithFrom(t *testing.T) {
	t.Parallel()
	var log []string
	imap := &fakeIMAP{
		mailboxes:  []imapadapter.Mailbox{{Name: "INBOX"}, {Name: "Sent", Attributes: []string{"\\Sent"}}},
		appendData: imapadapter.AppendData{UIDValidity: 9, UID: 42},
	}
	fake := &fakeSMTP{log: &log}
	service := testService(t, imap, fake, false, true)
	service.config.From = "alias@custom.example"
	service.config.EnableSentCopy = true
	result, err := service.SendMessage(context.Background(), SendInput{
		To: []string{"to@example.com"}, Subject: "archived", Body: "body", ArchiveSent: true,
	})
	if err != nil || result.Status != SendAccepted || result.ArchivalStatus != ArchivalStored {
		t.Fatalf("archived send = %+v, %v", result, err)
	}
	if !bytes.Equal(imap.appendedMessage, fake.message) {
		t.Fatal("Sent copy differs from the submitted message")
	}
	if !strings.Contains(string(imap.appendedMessage), "From: <alias@custom.example>\r\n") {
		t.Fatalf("Sent copy does not carry the configured sender:\n%s", imap.appendedMessage)
	}
}
