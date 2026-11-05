package mcptools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestStructuredResultsForRepresentativeTools(t *testing.T) {
	calendar := &icloud.MockService{Events: []icloud.Event{{
		UID: "structured", Title: "Structured", StartTime: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		EndTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}}}
	contactsService := &fakeContactsService{}
	mailService := &mailToolsFakeService{
		listResult: maildomain.ListMailboxesResult{Mailboxes: []maildomain.Mailbox{{Name: "INBOX"}}},
		searchResult: maildomain.SearchResult{Messages: []maildomain.MessageSummary{{
			Mailbox: "INBOX", UIDValidity: 1, UID: 1, Subject: "Structured",
		}}},
	}
	mcpServer := server.NewMCPServer("structured", "test", server.WithToolCapabilities(false))
	RegisterUnified(mcpServer, Deps{
		Service: calendar, ContactsService: contactsService, MailService: mailService,
		Audit: security.NewAuditLogger(&discardWriter{}), Redactor: security.NewRedactor("unused-secret"),
	}, NewCapabilityPlan(false, true, true, true, true))

	inProcess, err := client.NewInProcessClient(mcpServer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inProcess.Close() }()
	ctx := context.Background()
	if err := inProcess.Start(ctx); err != nil {
		t.Fatal(err)
	}
	initialize := mcp.InitializeRequest{}
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "structured", Version: "test"}
	if _, err := inProcess.Initialize(ctx, initialize); err != nil {
		t.Fatal(err)
	}
	listed, err := inProcess.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "search_events", "search_contacts", "search_messages":
			if tool.OutputSchema.Type != "object" || len(tool.OutputSchema.Properties) == 0 {
				t.Errorf("%s output schema = %#v", tool.Name, tool.OutputSchema)
			}
		}
	}

	calls := []struct {
		name string
		args map[string]any
	}{
		{"search_events", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-08T00:00:00Z", "calendar": "/cal/home/"}},
		{"search_contacts", map[string]any{"limit": float64(5)}},
		{"search_messages", map[string]any{"mailbox": "INBOX", "limit": float64(5)}},
	}
	for _, call := range calls {
		request := mcp.CallToolRequest{}
		request.Params.Name = call.name
		request.Params.Arguments = call.args
		result, err := inProcess.CallTool(ctx, request)
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		if result.IsError {
			t.Fatalf("%s error: %s", call.name, resultText(t, result))
		}
		if result.StructuredContent == nil {
			t.Fatalf("%s returned no structured content", call.name)
		}
		if _, ok := result.StructuredContent.(map[string]any); !ok {
			t.Fatalf("%s structured content type = %T", call.name, result.StructuredContent)
		}
		if len(result.Content) == 0 {
			t.Fatalf("%s lost its text fallback", call.name)
		}
	}
}

var _ contacts.Service = (*fakeContactsService)(nil)
