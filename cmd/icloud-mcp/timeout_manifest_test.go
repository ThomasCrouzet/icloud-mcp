package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type timeoutContacts struct{ contacts.Service }
type timeoutMail struct{ maildomain.Service }

func timeoutMailCapabilities() config.CapabilitySet {
	return config.CapabilitySet{MailRead: true, MailSend: true}
}

func timeoutProtocolClient(t *testing.T, s *server.MCPServer) *client.Client {
	t.Helper()
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "timeout-protocol", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c
}

func timeoutPayload(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("missing result: %+v", res)
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("non-text result: %+v", res)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatal(err)
	}
	t.Logf("mcp_result=%s isError=%v", text.Text, res.IsError)
	return payload
}

// Registration and read-only annotations are independent of the runtime classifier.
// No second list of effectful tool names can silently omit a new tool here.
func TestProtocolTimeoutRegisteredManifest(t *testing.T) {
	plans := map[string]mcptools.CapabilityPlan{
		"legacy-all": mcptools.NewCapabilityPlan(false, true, true, true, true),
		"read-only":  mcptools.NewCapabilityPlan(true, true, true, true, true),
		"permissions-all": mcptools.NewCapabilityPlanFromConfig(false, config.CapabilitySet{
			CalendarRead: true, CalendarWrite: true, CalendarDelete: true,
			ContactsRead: true, ContactsWrite: true, ContactsDelete: true,
			MailRead: true, MailMutate: true, MailTrash: true, MailSend: true, MailArchive: true,
		}),
		"send-only": mcptools.NewCapabilityPlanFromConfig(false, config.CapabilitySet{
			MailSend: true,
		}),
		"delete-only": mcptools.NewCapabilityPlanFromConfig(false, config.CapabilitySet{
			CalendarDelete: true, ContactsDelete: true, MailTrash: true,
		}),
	}
	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			red := security.NewRedactor("synthetic-timeout-secret")
			s := newMCPServer(red)
			mcptools.RegisterUnified(s, mcptools.Deps{
				Service: &icloud.MockService{}, ContactsService: &timeoutContacts{}, MailService: &timeoutMail{},
				Audit: security.NewAuditLogger(io.Discard), Redactor: red,
			}, plan)
			c := timeoutProtocolClient(t, s)
			listed, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.Tools) != plan.ToolCount() {
				t.Fatalf("manifest size = %d, want %d", len(listed.Tools), plan.ToolCount())
			}
			for _, tool := range listed.Tools {
				t.Run(tool.Name, func(t *testing.T) {
					if tool.Annotations.ReadOnlyHint == nil {
						t.Fatal("registered tool has no read-only classification")
					}
					mutation := !*tool.Annotations.ReadOnlyHint
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					h := timeoutMiddleware(toolTimeout)(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						t.Error("cancelled manifest probe dispatched a handler")
						return nil, nil
					})
					var req mcp.CallToolRequest
					req.Params.Name = tool.Name
					res, err := h(ctx, req)
					if err != nil || res == nil || !res.IsError {
						t.Fatalf("result=%+v err=%v", res, err)
					}
					payload := timeoutPayload(t, res)
					hint, _ := payload["reconciliation"].(string)
					if payload["code"] != "timeout" || payload["retryable"] != false || (hint != "") != mutation {
						t.Errorf("registered mutation=%v has incorrect timeout classification: %v", mutation, payload)
					}
					if isMutationToolName(tool.Name) != mutation {
						t.Errorf("runtime classification disagrees with registered %s", tool.Name)
					}
					if tool.Name == "send_message" || tool.Name == "reply_message" {
						assertMailTimeoutHint(t, payload)
					}
				})
			}
		})
	}
	if isMutationToolName("") || isMutationToolName("unregistered_tool") {
		t.Fatal("unknown tool classified as mutation")
	}
}
