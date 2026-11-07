package mcptools

import (
	"strings"
	"testing"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func TestContractReplyValidDispatchBoundary(t *testing.T) {
	b := newReplyScopeBackend(t)
	source, err := b.Service.GetMessage(t.Context(), maildomain.GetMessageInput{
		Mailbox: replyScopeMailbox, UIDValidity: 9, UID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wire_message_id=%q adapter_message_id=%q source_body=%q", "<source@example.invalid>", source.MessageID, source.Body)
	if source.MessageID != "source@example.invalid" || source.Body != "source-body" || len(source.ReplyTo) != 1 || source.ReplyTo[0].Address != "reply@example.invalid" {
		t.Fatalf("synthetic source did not reach the concrete parser boundary: %+v", source)
	}
	f := newReplyScopeProtocol(t, b, replyScopePlan(t, "mail.read,mail.send", false), "mail.read="+replyScopeMailbox)
	res := f.call(t, replyScopeArgs())
	t.Logf("real_reply_calls=%d imap_dials=%d metadata_fetches=%d smtp_dials=%d", b.reply.Load(), b.imap.Load(), b.metadata.Load(), b.smtp.Load())
	if b.smtp.Load() != 1 || strings.Contains(mailToolsText(t, res), "source message has no valid Message-ID") {
		t.Fatalf("valid MCP reply cannot pass the actual adapter-to-ReplyMessage boundary: %s", mailToolsText(t, res))
	}
}
