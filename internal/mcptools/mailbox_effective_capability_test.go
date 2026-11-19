package mcptools

import (
	"io"
	"testing"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// mailboxListingFixture lists one readable mailbox, one mutation destination,
// and one archive destination. The fixture mirrors the reported reproducer.
func mailboxListingFixture() *mailToolsFakeService {
	return &mailToolsFakeService{
		listResult: maildomain.ListMailboxesResult{Mailboxes: []maildomain.Mailbox{
			{Name: "INBOX"},
			{Name: "Private"},
			{Name: "Sent", Attributes: []string{"\\Sent"}},
		}},
	}
}

// mailboxAccessPlan builds the effective authorization data that the listing
// receives. It uses the same constructor as the production boot path.
func mailboxAccessPlan(t *testing.T, permissions string, readOnly bool) CapabilityPlan {
	t.Helper()
	return replyScopePlan(t, permissions, readOnly)
}

// mailboxListingClient registers the Mail tools from one effective plan and
// runs list_mailboxes over the MCP protocol. A plan without Mail read has no
// list_mailboxes tool, which is itself part of the reported contract.
func mailboxListingClient(t *testing.T, plan CapabilityPlan, scopes string) []maildomain.Mailbox {
	t.Helper()
	service := mailboxListingFixture()
	red := security.NewRedactor("synthetic-mailbox-secret")
	s := server.NewMCPServer("mailbox-listing", "test",
		server.WithToolCapabilities(false), server.WithInputSchemaValidation(),
		server.WithStrictInputSchemaDefault(), server.WithRecovery(),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	ConfigureMailNumberValidation(s)
	RegisterMail(s, MailDeps{
		Service: service, Redactor: red, Audit: security.NewAuditLogger(io.Discard),
		ResourceScopes: mustScopes(t, scopes), Plan: plan,
	}, plan.MailReadEnabled(), plan.MailMutationsEnabled(), plan.MailTrashEnabled(), plan.MailSendEnabled(), plan.MailReplyEnabled())
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
	init.Params.ClientInfo = mcp.Implementation{Name: "mailbox-listing", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	request := mcp.CallToolRequest{}
	request.Params.Name = "list_mailboxes"
	request.Params.Arguments = map[string]any{}
	result, err := c.CallTool(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("list_mailboxes failed: %s", resultText(t, result))
	}
	var listed maildomain.ListMailboxesResult
	decodeResult(t, result, &listed)
	return listed.Mailboxes
}

// TestListMailboxesHonoursEffectiveCapabilities covers the reported reproducer.
// A Mail read-only profile keeps stale mutation and archive scopes, so the
// listing must ignore them instead of disclosing Private and Sent.
func TestListMailboxesHonoursEffectiveCapabilities(t *testing.T) {
	const stale = "mail.read=INBOX;mail.mutate=Private;mail.archive=Sent"
	listed := mailboxListingClient(t, mailboxAccessPlan(t, "mail.read", true), stale)
	if len(listed) != 1 || listed[0].Name != "INBOX" {
		t.Fatalf("read-only listing = %+v, want only INBOX", listed)
	}
	if modes := listed[0].AccessModes; len(modes) != 1 || modes[0] != "read" {
		t.Fatalf("INBOX accessModes = %v, want [read]", modes)
	}
}

// TestListMailboxesReportsGrantedDestinations keeps legitimate destination
// discovery. A granted and scoped mutation still lists its destination with an
// accurate mode, never as readable.
func TestListMailboxesReportsGrantedDestinations(t *testing.T) {
	listed := mailboxListingClient(t, mailboxAccessPlan(t, "mail.read,mail.mutate", false),
		"mail.read=INBOX;mail.mutate=Private")
	byName := mailboxNames(listed)
	if _, ok := byName["Sent"]; ok {
		t.Fatalf("ungranted archive destination was listed: %+v", listed)
	}
	if modes := byName["Private"]; len(modes) != 1 || modes[0] != "mutate" {
		t.Fatalf("Private accessModes = %v, want [mutate]", modes)
	}
	// A present mail.mutate scope restricts mutation to Private, so INBOX keeps
	// the read mode only. It is never advertised as mutable.
	if modes := byName["INBOX"]; len(modes) != 1 || modes[0] != "read" {
		t.Fatalf("INBOX accessModes = %v, want [read]", modes)
	}
}

// TestListMailboxesHidesDisabledActions keeps every disabled action out of the
// listing under global read-only and an explicit permission subset.
func TestListMailboxesHidesDisabledActions(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		permissions string
		readOnly    bool
		scopes      string
		want        map[string][]string
	}{
		{
			name: "global-read-only", permissions: "mail.read,mail.mutate,mail.archive", readOnly: true,
			scopes: "mail.read=INBOX;mail.mutate=Private;mail.archive=Sent",
			want:   map[string][]string{"INBOX": {"read"}},
		},
		{
			name: "send-without-archive", permissions: "mail.read,mail.send", readOnly: false,
			scopes: "mail.read=INBOX;mail.archive=Sent",
			want:   map[string][]string{"INBOX": {"read"}},
		},
		{
			name: "mutate-without-archive", permissions: "mail.read,mail.mutate", readOnly: false,
			scopes: "mail.read=INBOX;mail.mutate=Private;mail.archive=Sent",
			want:   map[string][]string{"INBOX": {"read"}, "Private": {"mutate"}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := mailboxNames(mailboxListingClient(t,
				mailboxAccessPlan(t, testCase.permissions, testCase.readOnly), testCase.scopes))
			if len(got) != len(testCase.want) {
				t.Fatalf("listing = %#v, want %#v", got, testCase.want)
			}
			for name, wantModes := range testCase.want {
				modes, ok := got[name]
				if !ok {
					t.Fatalf("%s was not listed: %#v", name, got)
				}
				if len(modes) != len(wantModes) {
					t.Fatalf("%s accessModes = %v, want %v", name, modes, wantModes)
				}
				for index, mode := range wantModes {
					if modes[index] != mode {
						t.Fatalf("%s accessModes = %v, want %v", name, modes, wantModes)
					}
				}
				if modes[0] == "mutate" || modes[0] == "archive" {
					for _, mode := range modes {
						if mode == "read" {
							t.Fatalf("%s was advertised as readable: %v", name, modes)
						}
					}
				}
			}
			t.Logf("mailbox_listing=%s result=%#v", testCase.name, got)
		})
	}
}

// TestListMailboxesArchiveWithoutRead keeps the documented union policy for a
// genuine archive destination: it is listed with archive only, and it stays
// unreadable. list_mailboxes is a Mail read tool, so this case runs the filter
// and the read checks directly.
func TestListMailboxesArchiveWithoutRead(t *testing.T) {
	plan := mailboxAccessPlan(t, "mail.send,mail.archive", false)
	if plan.MailReadEnabled() {
		t.Fatal("archive-only plan must not enable Mail read")
	}
	scopes := mustScopes(t, "mail.read=INBOX;mail.archive=Sent")
	listed := mailboxNames(filterMailboxes(mailboxListingFixture().listResult.Mailboxes, scopes, plan))
	if len(listed) != 1 {
		t.Fatalf("archive-only listing = %#v, want only Sent", listed)
	}
	if modes := listed["Sent"]; len(modes) != 1 || modes[0] != "archive" {
		t.Fatalf("Sent accessModes = %v, want [archive]", modes)
	}

	deps := MailDeps{
		Service: mailboxListingFixture(), Redactor: security.NewRedactor("unused-secret"),
		ResourceScopes: scopes, Plan: plan,
	}
	for _, tool := range []string{"get_message", "search_messages"} {
		denied := callMailTool(t, deps, tool, map[string]any{"mailbox": "Sent", "uid_validity": 9, "uid": 2})
		if !denied.IsError {
			t.Fatalf("%s read an archive-only destination", tool)
		}
	}
}

// TestListMailboxesKeepsReadChecksIndependent confirms that a listed mutation
// destination is not a read grant.
func TestListMailboxesKeepsReadChecksIndependent(t *testing.T) {
	plan := mailboxAccessPlan(t, "mail.read,mail.mutate", false)
	deps := MailDeps{
		Service: mailboxListingFixture(), Redactor: security.NewRedactor("unused-secret"),
		Audit:          security.NewAuditLogger(io.Discard),
		ResourceScopes: mustScopes(t, "mail.read=INBOX;mail.mutate=Private"), Plan: plan,
	}
	listed, err := listMailboxesHandler(deps)(t.Context(), mailToolsRequest(map[string]any{}))
	if err != nil || listed.IsError {
		t.Fatalf("list_mailboxes: %v %s", err, resultText(t, listed))
	}
	var result maildomain.ListMailboxesResult
	decodeResult(t, listed, &result)
	if _, ok := mailboxNames(result.Mailboxes)["Private"]; !ok {
		t.Fatalf("granted mutation destination was not listed: %+v", result.Mailboxes)
	}
	for _, tool := range []string{"get_message", "search_messages"} {
		denied := callMailTool(t, deps, tool, map[string]any{"mailbox": "Private", "uid_validity": 9, "uid": 2})
		if !denied.IsError {
			t.Fatalf("%s read a mutation-only destination", tool)
		}
	}
}

// callMailTool dispatches one Mail read handler directly so the independent
// mail.read check is exercised without a tool-registration detour.
func callMailTool(t *testing.T, deps MailDeps, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	handlers := map[string]server.ToolHandlerFunc{
		"get_message":     getMessageHandler(deps),
		"search_messages": searchMessagesHandler(deps),
	}
	handler, ok := handlers[tool]
	if !ok {
		t.Fatalf("unknown tool %s", tool)
	}
	result, err := handler(t.Context(), mailToolsRequest(args))
	if err != nil {
		t.Fatal(err)
	}
	return result
}
