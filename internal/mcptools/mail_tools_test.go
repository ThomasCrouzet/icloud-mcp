package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type mailToolsFakeService struct {
	listResult   maildomain.ListMailboxesResult
	listErr      error
	searchResult maildomain.SearchResult
	searchErr    error
	searchInput  maildomain.SearchInput
	searchCalls  int
	getResult    maildomain.Message
	getErr       error
	getInput     maildomain.GetMessageInput
	getCalls     int
	flagsResult  maildomain.SetFlagsResult
	flagsErr     error
	flagsInput   maildomain.SetFlagsInput
	flagsCalls   int
	moveResult   maildomain.MoveResult
	moveErr      error
	moveInput    maildomain.MoveInput
	moveCalls    int
	trashResult  maildomain.MoveResult
	trashErr     error
	trashInput   maildomain.TrashInput
	trashCalls   int
	sendResult   maildomain.SendResult
	sendErr      error
	sendInput    maildomain.SendInput
	sendCalls    int
}

func (f *mailToolsFakeService) ListMailboxes(context.Context) (maildomain.ListMailboxesResult, error) {
	return f.listResult, f.listErr
}

func (f *mailToolsFakeService) SearchMessages(_ context.Context, input maildomain.SearchInput) (maildomain.SearchResult, error) {
	f.searchCalls++
	f.searchInput = input
	return f.searchResult, f.searchErr
}

func (f *mailToolsFakeService) GetMessage(_ context.Context, input maildomain.GetMessageInput) (maildomain.Message, error) {
	f.getCalls++
	f.getInput = input
	return f.getResult, f.getErr
}

func (f *mailToolsFakeService) SetMessageFlags(_ context.Context, input maildomain.SetFlagsInput) (maildomain.SetFlagsResult, error) {
	f.flagsCalls++
	f.flagsInput = input
	return f.flagsResult, f.flagsErr
}

func (f *mailToolsFakeService) MoveMessage(_ context.Context, input maildomain.MoveInput) (maildomain.MoveResult, error) {
	f.moveCalls++
	f.moveInput = input
	return f.moveResult, f.moveErr
}

func (f *mailToolsFakeService) TrashMessage(_ context.Context, input maildomain.TrashInput) (maildomain.MoveResult, error) {
	f.trashCalls++
	f.trashInput = input
	return f.trashResult, f.trashErr
}

func (f *mailToolsFakeService) SendMessage(_ context.Context, input maildomain.SendInput) (maildomain.SendResult, error) {
	f.sendCalls++
	f.sendInput = input
	return f.sendResult, f.sendErr
}

type mailToolsDiscardWriter struct{}

func (mailToolsDiscardWriter) Write(data []byte) (int, error) { return len(data), nil }

func mailToolsDeps(service maildomain.Service) MailDeps {
	return MailDeps{
		Service:  service,
		Audit:    security.NewAuditLogger(mailToolsDiscardWriter{}),
		Redactor: security.NewRedactor("mail-test-secret"),
	}
}

func mailToolsRequest(arguments map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = arguments
	return req
}

func mailToolsRawRequest(raw string) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.RawArguments = json.RawMessage(raw)
	return req
}

func mailToolsText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("unexpected tool result: %#v", result)
	}
	text, ok := mcp.AsTextContent(result.Content[0])
	if !ok {
		t.Fatalf("non-text tool result: %#v", result.Content[0])
	}
	return text.Text
}

func mailToolsJSON(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(mailToolsText(t, result)), &payload); err != nil {
		t.Fatalf("invalid JSON result: %v", err)
	}
	return payload
}

func TestRegisterMailCombinations(t *testing.T) {
	tests := []struct {
		name      string
		mutations bool
		send      bool
		want      []string
	}{
		{"reads", false, false, []string{"list_mailboxes", "search_messages", "get_message"}},
		{"mutations", true, false, []string{"list_mailboxes", "search_messages", "get_message", "set_message_flags", "move_message", "trash_message"}},
		{"send_independent", false, true, []string{"list_mailboxes", "search_messages", "get_message", "send_message"}},
		{"all", true, true, []string{"list_mailboxes", "search_messages", "get_message", "set_message_flags", "move_message", "trash_message", "send_message"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := server.NewMCPServer("mail-test", "test", server.WithToolCapabilities(false))
			got := RegisterMail(s, mailToolsDeps(&mailToolsFakeService{}), test.mutations, test.send)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("RegisterMail names = %v, want %v", got, test.want)
			}
			listed := listToolNames(t, s)
			if len(listed) != len(test.want) {
				t.Fatalf("listed names = %v, want %v", listed, test.want)
			}
			want := make(map[string]bool, len(test.want))
			for _, name := range test.want {
				want[name] = true
			}
			for _, name := range listed {
				if !want[name] {
					t.Errorf("unexpected registered tool %q", name)
				}
			}
		})
	}
}

func TestMailToolSchemas(t *testing.T) {
	search := newSearchMessagesTool()
	if search.InputSchema.AdditionalProperties != false {
		t.Errorf("search_messages additionalProperties = %#v", search.InputSchema.AdditionalProperties)
	}
	for _, required := range []string{"mailbox"} {
		if !mailToolsContains(search.InputSchema.Required, required) {
			t.Errorf("search_messages missing required %q", required)
		}
	}
	limit := search.InputSchema.Properties["limit"].(map[string]any)
	if limit["type"] != "integer" || limit["minimum"] != 1 || limit["maximum"] != maildomain.MaxSearchResults {
		t.Errorf("limit schema = %#v", limit)
	}
	date := search.InputSchema.Properties["since"].(map[string]any)
	if date["pattern"] != `^\d{4}-\d{2}-\d{2}$` {
		t.Errorf("since pattern = %#v", date["pattern"])
	}

	flagsTool := newSetMessageFlagsTool()
	flags := flagsTool.InputSchema.Properties["flags"].(map[string]any)
	items := flags["items"].(map[string]any)
	if !reflect.DeepEqual(items["enum"], []string{"seen", "flagged", "answered"}) || flags["maxItems"] != 3 || flags["uniqueItems"] != true {
		t.Errorf("safe flags schema = %#v", flags)
	}
	modSeq := flagsTool.InputSchema.Properties["expected_modseq"].(map[string]any)
	if modSeq["maximum"] != ^uint64(0) {
		t.Errorf("expected_modseq maximum = %#v", modSeq["maximum"])
	}
	flagsDescription := strings.ToLower(flagsTool.Description)
	for _, required := range []string{"expected_modseq", "condstore", "beta.8", "cannot observe modified", "before store", "protocol_error"} {
		if !strings.Contains(flagsDescription, required) {
			t.Errorf("set_message_flags description missing %q: %q", required, flagsTool.Description)
		}
	}
	if strings.Contains(flagsDescription, "returns concurrent_modification") {
		t.Errorf("set_message_flags description makes a false concurrency promise: %q", flagsTool.Description)
	}

	trash := newTrashMessageTool()
	if _, exists := trash.InputSchema.Properties["permanent_delete"]; exists || !strings.Contains(strings.ToLower(trash.Description), "no permanent-delete") {
		t.Errorf("trash_message does not make permanent deletion unavailable: %#v", trash)
	}
	send := newSendMessageTool()
	var sendSchema struct {
		Properties map[string]any   `json:"properties"`
		Required   []string         `json:"required"`
		AnyOf      []map[string]any `json:"anyOf"`
	}
	if err := json.Unmarshal(send.RawInputSchema, &sendSchema); err != nil {
		t.Fatalf("decode send_message schema: %v", err)
	}
	if !mailToolsContains(sendSchema.Required, "body") || mailToolsContains(sendSchema.Required, "to") || mailToolsContains(sendSchema.Required, "cc") || mailToolsContains(sendSchema.Required, "bcc") {
		t.Errorf("send_message required fields = %v", sendSchema.Required)
	}
	for _, forbidden := range []string{"html", "attachments", "from", "headers"} {
		if _, exists := sendSchema.Properties[forbidden]; exists {
			t.Errorf("send_message exposes forbidden property %q", forbidden)
		}
	}
	if len(sendSchema.AnyOf) != 3 {
		t.Fatalf("send_message anyOf = %#v", sendSchema.AnyOf)
	}
	for _, recipient := range []string{"to", "cc", "bcc"} {
		property, _ := sendSchema.Properties[recipient].(map[string]any)
		if property["minItems"] != float64(1) {
			t.Errorf("send_message %s minItems = %#v", recipient, property["minItems"])
		}
	}
	for i, recipient := range []string{"to", "cc", "bcc"} {
		required, _ := sendSchema.AnyOf[i]["required"].([]any)
		properties, _ := sendSchema.AnyOf[i]["properties"].(map[string]any)
		property, _ := properties[recipient].(map[string]any)
		if len(required) != 1 || required[0] != recipient || property["minItems"] != float64(1) {
			t.Errorf("send_message anyOf[%d] = %#v", i, sendSchema.AnyOf[i])
		}
	}
	if !strings.Contains(strings.ToLower(send.Description), "plain-text") || !strings.Contains(strings.ToLower(send.Description), "allowlist") {
		t.Errorf("send_message description does not state plaintext/allowlist policy: %q", send.Description)
	}
	for _, tool := range []mcp.Tool{newListMailboxesTool(), search, newGetMessageTool()} {
		if !strings.Contains(strings.ToLower(tool.Description), "untrusted remote data") {
			t.Errorf("%s description does not label remote content untrusted", tool.Name)
		}
	}
}

func TestMailStrictNumbersDatesAndArrays(t *testing.T) {
	service := &mailToolsFakeService{}
	deps := mailToolsDeps(service)

	result, _ := searchMessagesHandler(deps)(t.Context(), mailToolsRawRequest(`{"mailbox":"INBOX","before_uid":4294967296,"uid_validity":1}`))
	if !result.IsError || service.searchCalls != 0 {
		t.Fatalf("uint32 overflow reached service: result=%#v calls=%d", result, service.searchCalls)
	}
	result, _ = getMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"mailbox": "INBOX", "uid_validity": 1.5, "uid": 2}))
	if !result.IsError || service.getCalls != 0 {
		t.Fatalf("fractional UID reached service: result=%#v calls=%d", result, service.getCalls)
	}
	result, _ = searchMessagesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"mailbox": "INBOX", "since": "2026-07-25T00:00:00Z"}))
	if !result.IsError || service.searchCalls != 0 {
		t.Fatal("non-day-granularity date reached service")
	}
	result, _ = searchMessagesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"mailbox": "INBOX", "unseen_only": "true"}))
	if !result.IsError || service.searchCalls != 0 {
		t.Fatal("non-boolean unseen_only reached service")
	}
	result, _ = sendMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"to": []any{"a@example.com", 7}, "body": "hello"}))
	if !result.IsError || service.sendCalls != 0 {
		t.Fatal("mixed recipient array reached service")
	}
	if payload := mailToolsJSON(t, result); payload["status"] != string(maildomain.SendRejected) {
		t.Fatalf("local send validation status = %#v", payload)
	}
	result, _ = sendMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"body": "hello"}))
	if !result.IsError || service.sendCalls != 0 || mailToolsJSON(t, result)["status"] != string(maildomain.SendRejected) {
		t.Fatal("empty aggregate recipient set was not rejected locally")
	}

	valid := `{"mailbox":"INBOX","uid_validity":1,"uid":2,"operation":"add","flags":["seen"],"expected_modseq":18446744073709551615}`
	result, _ = setMessageFlagsHandler(deps)(t.Context(), mailToolsRawRequest(valid))
	if result.IsError || service.flagsCalls != 1 || service.flagsInput.ExpectedModSeq != ^uint64(0) {
		t.Fatalf("maximum uint64 was not preserved: result=%s input=%+v", mailToolsText(t, result), service.flagsInput)
	}
	overflow := `{"mailbox":"INBOX","uid_validity":1,"uid":2,"operation":"add","flags":["seen"],"expected_modseq":18446744073709551616}`
	result, _ = setMessageFlagsHandler(deps)(t.Context(), mailToolsRawRequest(overflow))
	if !result.IsError || service.flagsCalls != 1 {
		t.Fatal("uint64 overflow reached service")
	}
}

func TestMailReadHandlersAndUIDIdentity(t *testing.T) {
	service := &mailToolsFakeService{
		listResult:   maildomain.ListMailboxesResult{Mailboxes: []maildomain.Mailbox{{Name: "INBOX"}}},
		searchResult: maildomain.SearchResult{UIDValidity: 44, Messages: []maildomain.MessageSummary{{Mailbox: "INBOX", UIDValidity: 44, UID: 9}}},
		getResult:    maildomain.Message{MessageSummary: maildomain.MessageSummary{Mailbox: "INBOX", UIDValidity: 44, UID: 9}, Body: "hello"},
	}
	deps := mailToolsDeps(service)

	listed, _ := listMailboxesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{}))
	payload := mailToolsJSON(t, listed)
	if len(payload["mailboxes"].([]any)) != 1 {
		t.Errorf("list response = %#v", payload)
	}

	searched, _ := searchMessagesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{
		"mailbox": "INBOX", "since": "2026-07-01", "before": "2026-07-26", "before_uid": 10, "uid_validity": 44,
	}))
	if searched.IsError {
		t.Fatalf("search failed: %s", mailToolsText(t, searched))
	}
	if service.searchInput.Limit != 20 || service.searchInput.BeforeUID != 10 || service.searchInput.UIDValidity != 44 ||
		!service.searchInput.Since.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) ||
		!service.searchInput.Before.Equal(time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("search input = %+v", service.searchInput)
	}
	_ = mailToolsJSON(t, searched)

	got, _ := getMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"mailbox": "INBOX", "uid_validity": 44, "uid": 9}))
	if got.IsError {
		t.Fatalf("get failed: %s", mailToolsText(t, got))
	}
	if service.getInput != (maildomain.GetMessageInput{Mailbox: "INBOX", UIDValidity: 44, UID: 9, MaxBodyBytes: maildomain.DefaultBodyBytes}) {
		t.Errorf("get input = %+v", service.getInput)
	}
	getPayload := mailToolsJSON(t, got)
	if getPayload["uidValidity"] != float64(44) || getPayload["uid"] != float64(9) {
		t.Errorf("get response = %#v", getPayload)
	}
}

func TestMailMutationIdentityAndSendArrays(t *testing.T) {
	service := &mailToolsFakeService{
		flagsResult: maildomain.SetFlagsResult{Mailbox: "Archive", UIDValidity: 88, UID: 99},
		moveResult:  maildomain.MoveResult{Mailbox: "Archive", UIDValidity: 88, UID: 99, Destination: "Target", Method: "move"},
		trashResult: maildomain.MoveResult{Mailbox: "Archive", UIDValidity: 88, UID: 99, Destination: "Trash", Method: "move"},
		sendResult:  maildomain.SendResult{Status: maildomain.SendAccepted, MessageID: "message-id"},
	}
	deps := mailToolsDeps(service)
	identity := map[string]any{"mailbox": "Archive", "uid_validity": 88, "uid": 99}

	flagsArgs := map[string]any{"mailbox": "Archive", "uid_validity": 88, "uid": 99, "operation": "remove", "flags": []any{"seen", "answered"}, "expected_modseq": 123}
	result, _ := setMessageFlagsHandler(deps)(t.Context(), mailToolsRequest(flagsArgs))
	if result.IsError || service.flagsInput.Mailbox != "Archive" || service.flagsInput.UIDValidity != 88 || service.flagsInput.UID != 99 || service.flagsInput.ExpectedModSeq != 123 {
		t.Errorf("set flags identity/input = %+v, result=%s", service.flagsInput, mailToolsText(t, result))
	}
	moveArgs := map[string]any{"mailbox": identity["mailbox"], "uid_validity": identity["uid_validity"], "uid": identity["uid"], "destination": "Target"}
	result, _ = moveMessageHandler(deps)(t.Context(), mailToolsRequest(moveArgs))
	if result.IsError || service.moveInput != (maildomain.MoveInput{Mailbox: "Archive", UIDValidity: 88, UID: 99, Destination: "Target"}) {
		t.Errorf("move input = %+v", service.moveInput)
	}
	result, _ = trashMessageHandler(deps)(t.Context(), mailToolsRequest(identity))
	if result.IsError || service.trashInput != (maildomain.TrashInput{Mailbox: "Archive", UIDValidity: 88, UID: 99}) {
		t.Errorf("trash input = %+v", service.trashInput)
	}

	sendArgs := map[string]any{
		"to": []any{"to@example.com"}, "cc": []any{"cc@example.com"}, "bcc": []any{"bcc@example.com"},
		"subject": "subject", "body": "plain body",
	}
	result, _ = sendMessageHandler(deps)(t.Context(), mailToolsRequest(sendArgs))
	if result.IsError {
		t.Fatalf("send failed: %s", mailToolsText(t, result))
	}
	wantSend := maildomain.SendInput{To: []string{"to@example.com"}, Cc: []string{"cc@example.com"}, Bcc: []string{"bcc@example.com"}, Subject: "subject", Body: "plain body"}
	if !reflect.DeepEqual(service.sendInput, wantSend) {
		t.Errorf("send input = %+v, want %+v", service.sendInput, wantSend)
	}
}

func TestSendMessageAcceptsCcOrBccOnly(t *testing.T) {
	for _, test := range []struct {
		name string
		args map[string]any
		want maildomain.SendInput
	}{
		{
			name: "cc only",
			args: map[string]any{"cc": []any{"cc@example.com"}, "body": "plain body"},
			want: maildomain.SendInput{Cc: []string{"cc@example.com"}, Body: "plain body"},
		},
		{
			name: "bcc only",
			args: map[string]any{"bcc": []any{"bcc@example.com"}, "body": "plain body"},
			want: maildomain.SendInput{Bcc: []string{"bcc@example.com"}, Body: "plain body"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &mailToolsFakeService{sendResult: maildomain.SendResult{Status: maildomain.SendAccepted, MessageID: "message-id"}}
			result, err := sendMessageHandler(mailToolsDeps(service))(t.Context(), mailToolsRequest(test.args))
			if err != nil || result.IsError {
				t.Fatalf("send returned err=%v result=%s", err, mailToolsText(t, result))
			}
			if service.sendCalls != 1 || !reflect.DeepEqual(service.sendInput, test.want) {
				t.Fatalf("send input/calls = %+v/%d, want %+v/1", service.sendInput, service.sendCalls, test.want)
			}
		})
	}
}

func TestSendMessageDefinitiveErrorPreservesSafeResult(t *testing.T) {
	service := &mailToolsFakeService{
		sendResult: maildomain.SendResult{
			Status:    maildomain.SendRejected,
			MessageID: "safe-message-id",
			Recipients: []maildomain.RecipientStatus{
				{Index: 0, Accepted: true},
				{Index: 1, Category: "rejected"},
			},
		},
		sendErr: &maildomain.Error{Code: maildomain.CodeAuthorization, Message: "recipient command was rejected"},
	}
	result, err := sendMessageHandler(mailToolsDeps(service))(t.Context(), mailToolsRequest(map[string]any{
		"to":   []any{"first@example.com", "second@example.com"},
		"body": "plain body",
	}))
	if err != nil || !result.IsError {
		t.Fatalf("send returned err=%v result=%#v", err, result)
	}
	payload := mailToolsJSON(t, result)
	if payload["status"] != string(maildomain.SendRejected) || payload["messageId"] != "safe-message-id" {
		t.Fatalf("rejection metadata = %#v", payload)
	}
	recipients, ok := payload["recipients"].([]any)
	if !ok || len(recipients) != 2 {
		t.Fatalf("recipient statuses = %#v", payload["recipients"])
	}
	text := mailToolsText(t, result)
	for _, address := range []string{"first@example.com", "second@example.com"} {
		if strings.Contains(text, address) {
			t.Fatalf("rejection payload leaked recipient %q: %s", address, text)
		}
	}
}

func TestSendMessageOutcomeUnknownRemainsUnknown(t *testing.T) {
	service := &mailToolsFakeService{
		sendResult: maildomain.SendResult{
			Status:         maildomain.SendOutcomeUnknown,
			MessageID:      "safe-message-id",
			Recipients:     []maildomain.RecipientStatus{{Index: 0, Accepted: true}},
			Reconciliation: "Check Sent before retrying.",
		},
		sendErr: &maildomain.Error{
			Code:           maildomain.CodeOutcomeUnknown,
			Message:        "message submission outcome is unknown",
			Reconciliation: "Check Sent before retrying.",
		},
	}
	result, err := sendMessageHandler(mailToolsDeps(service))(t.Context(), mailToolsRequest(map[string]any{
		"bcc":  []any{"recipient@example.com"},
		"body": "plain body",
	}))
	if err != nil || !result.IsError {
		t.Fatalf("send returned err=%v result=%#v", err, result)
	}
	payload := mailToolsJSON(t, result)
	if payload["status"] != string(maildomain.SendOutcomeUnknown) || payload["messageId"] != "safe-message-id" || payload["reconciliation"] == nil {
		t.Fatalf("outcome_unknown metadata = %#v", payload)
	}
}

func TestMailStructuredErrorsAndRedaction(t *testing.T) {
	const secret = "protocol-secret-value"
	service := &mailToolsFakeService{listErr: errors.New("raw IMAP response: " + secret)}
	deps := MailDeps{Service: service, Audit: security.NewAuditLogger(mailToolsDiscardWriter{}), Redactor: security.NewRedactor(secret)}
	result, _ := listMailboxesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{}))
	if !result.IsError || strings.Contains(mailToolsText(t, result), "raw IMAP") || strings.Contains(mailToolsText(t, result), secret) {
		t.Fatalf("raw untyped error leaked: %s", mailToolsText(t, result))
	}
	if code := mailToolsJSON(t, result)["code"]; code != string(maildomain.CodeInternalError) {
		t.Errorf("untyped error code = %#v", code)
	}

	service.listErr = &maildomain.Error{
		Code: maildomain.CodeOutcomeUnknown, Message: "sanitized " + secret,
		Retryable: true, Reconciliation: "check state " + secret,
	}
	result, _ = listMailboxesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{}))
	payload := mailToolsJSON(t, result)
	if payload["code"] != string(maildomain.CodeOutcomeUnknown) || payload["status"] != "outcome_unknown" || payload["retryable"] != true || payload["reconciliation"] == nil {
		t.Errorf("typed error payload = %#v", payload)
	}
	if strings.Contains(mailToolsText(t, result), secret) || !strings.Contains(mailToolsText(t, result), "[REDACTED]") {
		t.Errorf("typed error was not redacted: %s", mailToolsText(t, result))
	}

	service.listErr = nil
	service.getResult = maildomain.Message{MessageSummary: maildomain.MessageSummary{Mailbox: "INBOX", UIDValidity: 1, UID: 2}, Body: "body " + secret}
	result, _ = getMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{"mailbox": "INBOX", "uid_validity": 1, "uid": 2}))
	if strings.Contains(mailToolsText(t, result), secret) || !strings.Contains(mailToolsText(t, result), "[REDACTED]") {
		t.Errorf("success result was not redacted: %s", mailToolsText(t, result))
	}
}

func TestMailAuditContainsNoMailContent(t *testing.T) {
	const (
		mailbox     = "private-mailbox-sentinel"
		destination = "private-destination-sentinel"
		recipient   = "private-recipient-sentinel@example.com"
		subject     = "private-subject-sentinel"
	)
	var audit bytes.Buffer
	service := &mailToolsFakeService{
		flagsResult: maildomain.SetFlagsResult{Mailbox: mailbox, UIDValidity: 1, UID: 2},
		moveErr: &maildomain.Error{
			Code: maildomain.CodeOutcomeUnknown, Message: "mail move outcome is unknown", Reconciliation: "check both mailboxes",
		},
		trashResult: maildomain.MoveResult{Mailbox: mailbox, UIDValidity: 1, UID: 2, Destination: "Trash", Method: "move"},
		sendResult:  maildomain.SendResult{Status: maildomain.SendAccepted, MessageID: "private-message-id-sentinel"},
	}
	deps := MailDeps{Service: service, Audit: security.NewAuditLogger(&audit), Redactor: security.NewRedactor("unused-secret")}

	_, _ = setMessageFlagsHandler(deps)(t.Context(), mailToolsRequest(map[string]any{
		"mailbox": mailbox, "uid_validity": 1, "uid": 2, "operation": "add", "flags": []any{"seen"},
	}))
	_, _ = moveMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{
		"mailbox": mailbox, "uid_validity": 1, "uid": 2, "destination": destination,
	}))
	_, _ = trashMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{
		"mailbox": mailbox, "uid_validity": 1, "uid": 2,
	}))
	_, _ = sendMessageHandler(deps)(t.Context(), mailToolsRequest(map[string]any{
		"to": []any{recipient}, "subject": subject, "body": "private-body-sentinel",
	}))

	logText := audit.String()
	for _, forbidden := range []string{mailbox, destination, recipient, subject, "private-body-sentinel", "private-message-id-sentinel"} {
		if strings.Contains(logText, forbidden) {
			t.Errorf("audit leaked %q: %s", forbidden, logText)
		}
	}
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	if len(lines) != 4 {
		t.Fatalf("audit lines = %d, want 4: %s", len(lines), logText)
	}
	wantTools := []string{"set_message_flags", "move_message", "trash_message", "send_message"}
	for index, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid audit JSON: %v", err)
		}
		if entry["tool"] != wantTools[index] || entry["domain"] != "mail" || entry["resourceToken"] == "" {
			t.Errorf("audit entry %d = %#v", index, entry)
		}
		if index == 1 && entry["status"] != "outcome_unknown" {
			t.Errorf("outcome_unknown audit status = %#v", entry["status"])
		}
	}
}

func mailToolsContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
