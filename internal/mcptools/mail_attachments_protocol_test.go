package mcptools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

// attachmentFakeService adds the attachment surface to the shared fake.
type attachmentFakeService struct {
	mailToolsFakeService
	getAttachment   maildomain.AttachmentResult
	getInput        maildomain.GetAttachmentInput
	stageInput      maildomain.StageInput
	draftInput      maildomain.DraftInput
	attachmentCalls int
}

func (f *attachmentFakeService) GetAttachment(_ context.Context, input maildomain.GetAttachmentInput) (maildomain.AttachmentResult, error) {
	f.attachmentCalls++
	f.getInput = input
	return f.getAttachment, nil
}

func (f *attachmentFakeService) StageAttachment(_ context.Context, input maildomain.StageInput) (maildomain.StageResult, error) {
	f.attachmentCalls++
	f.stageInput = input
	return maildomain.StageResult{Handle: "stg_" + strings.Repeat("a", 32), Filename: "a.txt", ContentType: "text/plain", Size: 3}, nil
}

func (f *attachmentFakeService) CreateDraft(_ context.Context, input maildomain.DraftInput) (maildomain.DraftResult, error) {
	f.attachmentCalls++
	f.draftInput = input
	return maildomain.DraftResult{Status: "stored", Mailbox: "Drafts", UIDValidity: 3, UID: 4, MessageID: "draft@example.invalid"}, nil
}

func attachmentToolNames(t *testing.T, f *replyScopeProtocol) ([]string, map[string]mcp.Tool) {
	t.Helper()
	listed, err := f.client.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tools := map[string]mcp.Tool{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		tools[tool.Name] = tool
	}
	return names, tools
}

func callAttachmentTool(t *testing.T, f *replyScopeProtocol, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = args
	result, err := f.client.CallTool(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProtocolAttachmentToolsFollowPermissions(t *testing.T) {
	for _, tc := range []struct {
		permissions string
		readOnly    bool
		present     []string
		absent      []string
		sendRefs    bool
	}{
		{"mail.read,mail.send", false, nil, []string{"get_attachment", "stage_attachment", "create_draft"}, false},
		{"mail.read,mail.attachments.read", false, []string{"get_attachment"}, []string{"stage_attachment", "create_draft"}, false},
		{"mail.read,mail.send,mail.attachments.send", false, nil, []string{"get_attachment", "create_draft"}, true},
		{"mail.read,mail.drafts", false, []string{"stage_attachment", "create_draft"}, []string{"get_attachment"}, false},
		{"mail.read,mail.send,mail.attachments.read,mail.attachments.send,mail.drafts", true, []string{"get_attachment"}, []string{"stage_attachment", "create_draft", "send_message"}, false},
	} {
		t.Run(tc.permissions, func(t *testing.T) {
			svc := &attachmentFakeService{}
			f := newReplyScopeProtocol(t, svc, replyScopePlan(t, tc.permissions, tc.readOnly), "")
			names, tools := attachmentToolNames(t, f)
			for _, name := range tc.present {
				if !slices.Contains(names, name) {
					t.Errorf("%s missing from %v", name, names)
				}
			}
			for _, name := range tc.absent {
				if slices.Contains(names, name) {
					t.Errorf("%s registered in %v", name, names)
				}
			}
			if send, ok := tools["send_message"]; ok {
				encoded, _ := json.Marshal(send)
				if got := strings.Contains(string(encoded), `"attachments"`); got != tc.sendRefs {
					t.Errorf("send_message attachments property = %v, want %v", got, tc.sendRefs)
				}
				if !tc.sendRefs {
					result := callAttachmentTool(t, f, "send_message", map[string]any{
						"to": []string{"to@example.invalid"}, "body": "x",
						"attachments": []any{map[string]any{"mailbox": "INBOX", "uid_validity": 1, "uid": 2, "part_id": "2"}},
					})
					if !result.IsError || svc.sendCalls != 0 {
						t.Errorf("send_message accepted attachments without the permission")
					}
				}
			}
		})
	}
}

func TestProtocolSendMessageAttachmentReferences(t *testing.T) {
	svc := &attachmentFakeService{}
	svc.sendResult = maildomain.SendResult{Status: maildomain.SendAccepted, MessageID: "id@example.invalid"}
	f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.send,mail.attachments.send", false), "mail.read=INBOX")
	result := callAttachmentTool(t, f, "send_message", map[string]any{
		"to": []string{"to@example.invalid"}, "body": "x",
		"attachments": []any{map[string]any{"mailbox": "INBOX", "uid_validity": 7, "uid": 9, "part_id": "1.2"}},
	})
	if result.IsError || len(svc.sendInput.Attachments) != 1 || svc.sendInput.Attachments[0] != (maildomain.AttachmentRef{Mailbox: "INBOX", UIDValidity: 7, UID: 9, PartID: "1.2"}) {
		t.Fatalf("send result = %+v, input = %+v", result, svc.sendInput.Attachments)
	}
	for _, bad := range []map[string]any{
		{"mailbox": "Private", "uid_validity": 7, "uid": 9, "part_id": "2"},
		{"mailbox": "INBOX", "uid_validity": 7, "uid": 9, "part_id": "0"},
		{"mailbox": "INBOX", "uid_validity": 7, "uid": 9, "part_id": "2", "extra": true},
		{"mailbox": "INBOX", "uid_validity": 0, "uid": 9, "part_id": "2"},
	} {
		calls := svc.sendCalls
		result := callAttachmentTool(t, f, "send_message", map[string]any{"to": []string{"to@example.invalid"}, "body": "x", "attachments": []any{bad}})
		if !result.IsError || svc.sendCalls != calls {
			t.Errorf("attachment reference %v was accepted", bad)
		}
	}
}

func TestProtocolGetAttachmentContent(t *testing.T) {
	image := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	for _, tc := range []struct {
		name     string
		result   maildomain.AttachmentResult
		wantType string
	}{
		{"image", maildomain.AttachmentResult{Delivery: maildomain.DeliveryImage, Data: image, DataMIME: "image/png", ContentType: "image/png", PartID: "2"}, "image"},
		{"resource", maildomain.AttachmentResult{Delivery: maildomain.DeliveryResource, Data: []byte("%PDF"), DataMIME: "application/pdf", ContentType: "application/pdf", PartID: "3", Filename: "a b.pdf"}, "resource"},
		{"text", maildomain.AttachmentResult{Delivery: maildomain.DeliveryText, Text: "hello", ContentType: "text/plain", PartID: "4"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &attachmentFakeService{getAttachment: tc.result}
			f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.attachments.read", false), "")
			result := callAttachmentTool(t, f, "get_attachment", map[string]any{"mailbox": "INBOX", "uid_validity": 1, "uid": 2, "part_id": tc.result.PartID})
			if result.IsError || result.StructuredContent == nil {
				t.Fatalf("result = %+v", result)
			}
			if tc.wantType == "" {
				if len(result.Content) != 1 || !strings.Contains(result.Content[0].(mcp.TextContent).Text, `"text": "hello"`) && !strings.Contains(result.Content[0].(mcp.TextContent).Text, `"text":"hello"`) {
					t.Fatalf("text result = %+v", result.Content)
				}
				return
			}
			if len(result.Content) != 2 {
				t.Fatalf("content = %+v", result.Content)
			}
			switch content := result.Content[1].(type) {
			case mcp.ImageContent:
				decoded, _ := base64.StdEncoding.DecodeString(content.Data)
				if tc.wantType != "image" || content.MIMEType != "image/png" || string(decoded) != string(image) {
					t.Fatalf("image content = %+v", content)
				}
			case mcp.EmbeddedResource:
				blob, ok := content.Resource.(mcp.BlobResourceContents)
				if tc.wantType != "resource" || !ok || blob.MIMEType != "application/pdf" || !strings.HasPrefix(blob.URI, "icloud-mail-attachment:///3/") {
					t.Fatalf("resource content = %+v", content)
				}
			default:
				t.Fatalf("unexpected content %T", content)
			}
		})
	}
	svc := &attachmentFakeService{}
	f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.attachments.read", false), "mail.read=INBOX")
	result := callAttachmentTool(t, f, "get_attachment", map[string]any{"mailbox": "Private", "uid_validity": 1, "uid": 2, "part_id": "2"})
	if !result.IsError || svc.attachmentCalls != 0 {
		t.Fatal("get_attachment ignored the mail.read resource scope")
	}
}

func TestProtocolStageAndDraftArguments(t *testing.T) {
	svc := &attachmentFakeService{}
	f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.drafts", false), "")
	result := callAttachmentTool(t, f, "stage_attachment", map[string]any{"filename": "a.txt", "content_type": "text/plain", "data": "YWJj"})
	if result.IsError || svc.stageInput.Filename != "a.txt" || svc.stageInput.Data != "YWJj" {
		t.Fatalf("stage = %+v input %+v", result, svc.stageInput)
	}
	result = callAttachmentTool(t, f, "stage_attachment", map[string]any{"handle": "stg_bad", "data": "YWJj"})
	if !result.IsError {
		t.Fatal("stage_attachment accepted an invalid handle")
	}
	handle := "stg_" + strings.Repeat("b", 32)
	result = callAttachmentTool(t, f, "create_draft", map[string]any{"to": []string{"anyone@example.invalid"}, "body": "x", "staged": []string{handle}})
	if result.IsError || len(svc.draftInput.Staged) != 1 || svc.draftInput.Staged[0] != handle {
		t.Fatalf("draft = %+v input %+v", result, svc.draftInput)
	}
	if !strings.Contains(f.audit.String(), `"tool":"create_draft"`) {
		t.Fatalf("create_draft was not audited: %s", f.audit.String())
	}
}

func TestProtocolGetAttachmentRedactsResourceURI(t *testing.T) {
	// The fixture redactor treats this value as a credential.
	svc := &attachmentFakeService{getAttachment: maildomain.AttachmentResult{
		Delivery: maildomain.DeliveryResource, Data: []byte("%PDF"), DataMIME: "application/pdf",
		ContentType: "application/pdf", PartID: "3", Filename: "synthetic-reply-secret.pdf",
	}}
	f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.attachments.read", false), "")
	result := callAttachmentTool(t, f, "get_attachment", map[string]any{"mailbox": "INBOX", "uid_validity": 1, "uid": 2, "part_id": "3"})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || strings.Contains(string(encoded), "synthetic-reply-secret") {
		t.Fatalf("result leaks a redacted value: %s", encoded)
	}
	if len(result.Content) != 2 {
		t.Fatalf("content = %+v", result.Content)
	}
	resource, ok := result.Content[1].(mcp.EmbeddedResource)
	if !ok {
		t.Fatalf("content = %T", result.Content[1])
	}
	blob, ok := resource.Resource.(mcp.BlobResourceContents)
	if !ok || blob.URI != "icloud-mail-attachment:///3/%5BREDACTED%5D.pdf" || blob.Blob != base64.StdEncoding.EncodeToString([]byte("%PDF")) {
		t.Fatalf("resource = %+v", resource.Resource)
	}
}

func TestProtocolAttachmentReferencesOutsideReadScope(t *testing.T) {
	svc := &attachmentFakeService{}
	svc.replyResult = maildomain.SendResult{Status: maildomain.SendAccepted, MessageID: "reply@example.invalid"}
	f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.read,mail.send,mail.attachments.send,mail.drafts", false), "mail.read=INBOX")
	outside := []any{map[string]any{"mailbox": "Private", "uid_validity": 7, "uid": 9, "part_id": "2"}}
	inside := []any{map[string]any{"mailbox": "INBOX", "uid_validity": 7, "uid": 9, "part_id": "2"}}

	result := callAttachmentTool(t, f, "create_draft", map[string]any{"body": "x", "attachments": outside})
	if !result.IsError || svc.attachmentCalls != 0 {
		t.Fatalf("create_draft accepted an out-of-scope attachment: calls = %d", svc.attachmentCalls)
	}
	result = callAttachmentTool(t, f, "reply_message", map[string]any{"mailbox": "INBOX", "uid_validity": 7, "uid": 9, "body": "x", "attachments": outside})
	if !result.IsError || svc.replyCalls != 0 {
		t.Fatalf("reply_message accepted an out-of-scope attachment: calls = %d", svc.replyCalls)
	}
	// In-scope controls reach the service.
	result = callAttachmentTool(t, f, "create_draft", map[string]any{"body": "x", "attachments": inside})
	if result.IsError || svc.attachmentCalls != 1 {
		t.Fatalf("create_draft in scope = %+v, calls = %d", result, svc.attachmentCalls)
	}
	result = callAttachmentTool(t, f, "reply_message", map[string]any{"mailbox": "INBOX", "uid_validity": 7, "uid": 9, "body": "x", "attachments": inside})
	if result.IsError || svc.replyCalls != 1 {
		t.Fatalf("reply_message in scope = %+v, calls = %d", result, svc.replyCalls)
	}
	if strings.Contains(f.audit.String(), "Private") {
		t.Fatalf("audit names a raw mailbox: %s", f.audit.String())
	}
}
