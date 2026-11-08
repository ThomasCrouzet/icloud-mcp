package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

func TestRecipientPolicyKeepsLocalPartCase(t *testing.T) {
	t.Parallel()
	policy, err := ParseRecipientPolicy("Team@case-sensitive.example")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.allows("Team@case-sensitive.example") || !policy.allows("Team@CASE-SENSITIVE.EXAMPLE") {
		t.Fatal("exact local-part case with domain folding should be allowed")
	}
	if policy.allows("team@case-sensitive.example") {
		t.Fatal("a case-distinct local part must not be authorized")
	}
}

func TestSendIdempotencyConflictsOnLocalPartCase(t *testing.T) {
	t.Parallel()
	var log []string
	fake := &fakeSMTP{log: &log}
	service := testService(t, nil, fake, false, true)
	first, err := service.SendMessage(context.Background(), SendInput{
		To: []string{"Team@example.com"}, Body: "body", IdempotencyKey: "case-key",
	})
	if err != nil || first.Status != SendAccepted {
		t.Fatalf("first send = %+v, %v", first, err)
	}
	second, err := service.SendMessage(context.Background(), SendInput{
		To: []string{"team@example.com"}, Body: "body", IdempotencyKey: "case-key",
	})
	if errorCode(t, err) != CodeConflict || second.MessageID != "" {
		t.Fatalf("case-only local-part change = %+v, %v", second, err)
	}
	if strings.Count(strings.Join(log, ","), "data") != 1 {
		t.Fatalf("case-only local-part change dispatched twice: %v", log)
	}
}

func TestReplyAllPreservesLocalPartCase(t *testing.T) {
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
				To: []imapadapter.Address{
					{Address: "sender@ICLOUD.COM"},
					{Address: "Sender@icloud.com"},
					{Address: "dup@example.com"},
				},
				Cc: []imapadapter.Address{{Address: "dup@EXAMPLE.COM"}},
			},
			Headers: imapadapter.CuratedHeaders{MessageID: "<source@example.com>"},
		}},
	}
	service := testService(t, imap, fake, false, true)
	result, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 9, UID: 1, Mode: "reply_all", Body: "body",
	})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("reply_all = %+v, %v", result, err)
	}
	joined := strings.Join(log, ",")
	if strings.Contains(joined, "rcpt:sender@ICLOUD.COM") {
		t.Fatalf("domain-case self recipient was not suppressed: %s", joined)
	}
	if !strings.Contains(joined, "rcpt:Sender@icloud.com") {
		t.Fatalf("case-distinct local part was dropped: %s", joined)
	}
	if strings.Count(joined, "rcpt:dup@example.com") != 1 || strings.Contains(joined, "rcpt:dup@EXAMPLE.COM") {
		t.Fatalf("domain-case duplicate was not deduplicated: %s", joined)
	}
}
