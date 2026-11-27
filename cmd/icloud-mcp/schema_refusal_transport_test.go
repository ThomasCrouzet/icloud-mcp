package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestSchemaRefusalTransports proves that the production server rewrites an
// input schema refusal on the in-process and the bounded stdio transport.
func TestSchemaRefusalTransports(t *testing.T) {
	const marker = "ZQXMARKER"
	for _, mode := range []string{"inprocess", "bounded-stdio"} {
		t.Run(mode, func(t *testing.T) {
			red := security.NewRedactor("synthetic-schema-secret")
			s := newMCPServer(red, mcptools.NewCapabilityPlan(false, false, true, true, false))
			backend := &contractNumericMail{}
			mcptools.RegisterMail(s, mcptools.MailDeps{Service: backend, Redactor: red, Audit: security.NewAuditLogger(io.Discard)}, true, true, false, false, false)
			request := contractNumericTransport(t, s, mode)
			_ = request("initialize", 1, `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"schema-refusal","version":"test"}}`)
			flags := `"mailbox":"INBOX","uid_validity":1,"uid":2,"operation":"add","flags":["seen"]`
			for i, tc := range []struct{ name, params, want string }{
				{"unknown key", `{"name":"set_message_flags","arguments":{` + flags + `,"` + marker + `":1}}`, "unknown argument"},
				{"wrong type", `{"name":"set_message_flags","arguments":{"mailbox":"INBOX","uid_validity":"` + marker + `","uid":2,"operation":"add","flags":["seen"]}}`, "invalid value for uid_validity"},
				{"enum", `{"name":"set_message_flags","arguments":{"mailbox":"INBOX","uid_validity":1,"uid":2,"operation":"` + marker + `","flags":["seen"]}}`, "invalid value for operation"},
				{"pattern", `{"name":"search_messages","arguments":{"mailbox":"INBOX","since":"` + marker + `1"}}`, "invalid value for since"},
				{"missing required", `{"name":"set_message_flags","arguments":{"mailbox":"INBOX"}}`, "missing required argument flags, operation, uid, uid_validity"},
				{"no arguments", `{"name":"set_message_flags"}`, "missing required argument"},
				{"arguments not an object", `{"name":"set_message_flags","arguments":"` + marker + `"}`, "arguments do not match the input schema"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := len(backend.snapshot())
					raw := request("tools/call", 10+i, tc.params)
					var result mcp.CallToolResult
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					var wire map[string]any
					if err := json.Unmarshal(raw, &wire); err != nil {
						t.Fatal(err)
					}
					if _, structured := wire["structuredContent"]; structured || !result.IsError || len(result.Content) != 1 {
						t.Fatalf("refusal shape: %s", raw)
					}
					text, ok := result.Content[0].(mcp.TextContent)
					if !ok {
						t.Fatalf("refusal content: %s", raw)
					}
					var payload struct {
						Code      string `json:"code"`
						Message   string `json:"message"`
						Retryable bool   `json:"retryable"`
					}
					if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
						t.Fatalf("refusal is not a JSON payload: %s", raw)
					}
					if payload.Code != "validation" || payload.Retryable || !strings.Contains(payload.Message, tc.want) {
						t.Fatalf("payload = %+v, want validation with %q", payload, tc.want)
					}
					if strings.Contains(string(raw), marker) {
						t.Fatalf("refusal repeats caller input: %s", raw)
					}
					if calls := len(backend.snapshot()) - before; calls != 0 {
						t.Fatalf("refusal reached the backend: %d calls", calls)
					}
				})
			}
		})
	}
}

// TestNewMCPServerSchemaRefusalHookOrder fails when the production server
// registers an observer that can see a raw SDK refusal: an OnSuccess hook, or
// an OnAfterCallTool hook before the rewrite.
func TestNewMCPServerSchemaRefusalHookOrder(t *testing.T) {
	s := newMCPServer(security.NewRedactor("synthetic-schema-secret"), mcptools.NewCapabilityPlan(false, false, true, true, false))
	hooks := s.GetHooks()
	if hooks == nil || len(hooks.OnAfterCallTool) == 0 {
		t.Fatal("production server has no schema refusal hook")
	}
	if len(hooks.OnSuccess) != 0 {
		t.Fatalf("production server has %d OnSuccess hooks; they would see raw SDK refusals", len(hooks.OnSuccess))
	}
	result := mcp.NewToolResultError("input schema validation failed: /mailbox: 'ZQXMARKER' does not match pattern")
	request := &mcp.CallToolRequest{}
	request.Params.Name = "list_mailboxes"
	hooks.OnAfterCallTool[0](t.Context(), 1, request, result)
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.HasPrefix(text, `{"code":"validation"`) || strings.Contains(text, "ZQXMARKER") {
		t.Fatalf("first OnAfterCallTool hook is not the schema refusal rewrite: %s", text)
	}
}
