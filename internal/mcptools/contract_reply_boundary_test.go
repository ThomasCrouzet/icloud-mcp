package mcptools

import (
	"bytes"
	"net/mail"
	"strings"
	"testing"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func TestContractReplyValidDispatchBoundary(t *testing.T) {
	b := &contractMailBackend{}
	svc := b.service(t)
	source, err := svc.GetMessage(t.Context(), maildomain.GetMessageInput{
		Mailbox: "INBOX", UIDValidity: 9, UID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wire_message_id=%q adapter_message_id=%q source_body=%q", "<source@example.invalid>", source.MessageID, source.Body)
	if source.MessageID != "<source@example.invalid>" || source.Body != "source-body" || len(source.ReplyTo) != 1 || source.ReplyTo[0].Address != "reply@example.invalid" {
		t.Fatalf("synthetic source did not reach the concrete parser boundary: %+v", source)
	}
	f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.send", false), "mail.read=INBOX")
	res := f.call(t, map[string]any{"mailbox": "INBOX", "uid_validity": 9, "uid": 2, "body": "Synthetic reply body"})
	t.Logf("real_backend=%s", contractReplayJSON(t, b.snapshot()))
	if res.IsError || mailToolsJSON(t, res)["status"] != "accepted" {
		t.Fatalf("actual ReplyMessage did not accept verified-STARTTLS submission: %s", mailToolsText(t, res))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.imapDials != 2 || b.smtpDials != 1 || b.data != 1 || b.tls != 1 || len(b.payloads) != 1 {
		t.Fatalf("unexpected real reply counts: imap=%d smtp=%d DATA=%d TLS=%d", b.imapDials, b.smtpDials, b.data, b.tls)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(b.payloads[0]))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("In-Reply-To") != "<source@example.invalid>" || msg.Header.Get("References") != "<root@example.invalid> <source@example.invalid>" || msg.Header.Get("Subject") != "Re: Synthetic" {
		t.Fatalf("actual threading headers changed: %v", msg.Header)
	}
	if len(b.recipients) != 1 || !strings.Contains(b.recipients[0], "reply@example.invalid") {
		t.Fatalf("wrong actual reply recipients: %v", b.recipients)
	}
}

func TestContractReplyRecipientPolicyAndReplyAll(t *testing.T) {
	for _, tc := range []struct {
		name, mode, policy string
		want               int
	}{{"sender-default", "", "", 1}, {"reply-all", "reply_all", "", 2}, {"recipient-denied", "sender", "recipient@example.invalid", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			b := &contractMailBackend{policy: tc.policy}
			f := newReplyScopeProtocol(t, b.service(t), replyScopePlan(t, "mail.read,mail.send", false), "mail.read=INBOX")
			args := map[string]any{"mailbox": "INBOX", "uid_validity": 9, "uid": 2, "body": "Synthetic reply body"}
			if tc.mode != "" {
				args["mode"] = tc.mode
			}
			res := f.call(t, args)
			t.Logf("real_backend=%s", contractReplayJSON(t, b.snapshot()))
			b.mu.Lock()
			defer b.mu.Unlock()
			if tc.want == 0 {
				if !res.IsError || b.smtpDials != 0 || b.data != 0 {
					t.Fatal("recipient policy failed before SMTP")
				}
				return
			}
			if res.IsError || b.smtpDials != 1 || b.data != 1 || b.tls != 1 || len(b.recipients) != tc.want {
				t.Fatalf("reply recipients or send count: %v, %s", b.recipients, mailToolsText(t, res))
			}
		})
	}
}
