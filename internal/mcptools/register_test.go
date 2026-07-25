package mcptools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// newTestServer builds a *server.MCPServer with the tools registered (via
// mcptools.Register) against a MockService, exactly the production wiring
// minus the real iCloud client.
func newTestServer(readOnly bool) *server.MCPServer {
	s := server.NewMCPServer("icloud-mcp-test", "test", server.WithToolCapabilities(false))
	deps := Deps{
		Service:  &icloud.MockService{},
		Audit:    security.NewAuditLogger(&discardWriter{}),
		Redactor: security.NewRedactor("unused-secret"),
	}
	Register(s, deps, readOnly)
	return s
}

// listToolNames connects to the server through an in-process MCP client (as
// a real stdio client would), initializes the session, then lists the
// exposed tools. This exercises the required READ_ONLY behavior: the write
// tools must be removed from tools/list, not merely rejected at execution
// time.
func listToolNames(t *testing.T, s *server.MCPServer) []string {
	t.Helper()
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		names[i] = tool.Name
	}
	return names
}

func TestRegister_ReadOnlyExposesOnlyReadTools(t *testing.T) {
	s := newTestServer(true)
	names := listToolNames(t, s)

	want := map[string]bool{
		"list_calendars": true, "search_events": true, "get_event": true,
		"find_free_slots": true, "validate_event": true, "calendar_capabilities": true,
		"icloud_capabilities": true,
	}
	if len(names) != len(want) {
		t.Fatalf("READ_ONLY: %d tools registered, want %d: %v", len(names), len(want), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected tool in READ_ONLY mode: %q", n)
		}
		// Mutations must stay absent.
		if n == "create_event" || n == "update_event" || n == "delete_event" {
			t.Errorf("mutation tool present in READ_ONLY: %q", n)
		}
	}
}

func TestRegister_FullModeExposesAllTools(t *testing.T) {
	s := newTestServer(false)
	names := listToolNames(t, s)

	want := map[string]bool{
		"list_calendars": true, "search_events": true, "get_event": true,
		"find_free_slots": true, "validate_event": true, "calendar_capabilities": true,
		"create_event": true, "update_event": true, "delete_event": true,
		"icloud_capabilities": true,
	}
	if len(names) != len(want) {
		t.Fatalf("full mode: %d tools registered, want %d: %v", len(names), len(want), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected tool: %q", n)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestCapabilityPlanAllCombinationsAndRegistrationFormula(t *testing.T) {
	for bits := 0; bits < 32; bits++ {
		readOnly := bits&1 != 0
		contactsEnabled := bits&2 != 0
		mailEnabled := bits&4 != 0
		mailMutations := bits&8 != 0
		mailSend := bits&16 != 0
		name := fmt.Sprintf("ro=%t/contacts=%t/mail=%t/mutations=%t/send=%t",
			readOnly, contactsEnabled, mailEnabled, mailMutations, mailSend)
		t.Run(name, func(t *testing.T) {
			plan := NewCapabilityPlan(readOnly, contactsEnabled, mailEnabled, mailMutations, mailSend)
			globalWrites := !readOnly
			effectiveMutation := globalWrites && mailEnabled && mailMutations
			effectiveSend := globalWrites && mailEnabled && mailSend
			wantCount := 7
			if globalWrites {
				wantCount += 3
			}
			if contactsEnabled {
				wantCount += 3
				if globalWrites {
					wantCount += 3
				}
			}
			if mailEnabled {
				wantCount += 3
			}
			if effectiveMutation {
				wantCount += 3
			}
			if effectiveSend {
				wantCount++
			}

			if plan.ToolCount() != wantCount {
				t.Fatalf("plan count = %d, want formula result %d", plan.ToolCount(), wantCount)
			}
			if plan.ContactsWritesEnabled() != (contactsEnabled && globalWrites) ||
				plan.MailMutationsEnabled() != effectiveMutation || plan.MailSendEnabled() != effectiveSend {
				t.Fatalf("effective capability gates are inconsistent: %#v", plan)
			}

			s := server.NewMCPServer("icloud-mcp-test", "test", server.WithToolCapabilities(false))
			registered := RegisterUnified(s, Deps{
				Service:         &icloud.MockService{},
				ContactsService: &fakeContactsService{},
				MailService:     &mailToolsFakeService{},
				Audit:           security.NewAuditLogger(&discardWriter{}),
				Redactor:        security.NewRedactor("unused-secret"),
			}, plan)
			listed := listToolNames(t, s)
			sort.Strings(listed)
			if !slices.Equal(registered, plan.RegisteredTools()) || !slices.Equal(listed, registered) {
				t.Fatalf("plan/registration/tools-list mismatch: plan=%v registered=%v listed=%v",
					plan.RegisteredTools(), registered, listed)
			}
		})
	}
}

func TestCapabilityPlanDefaultCompatibility(t *testing.T) {
	readWrite := NewCapabilityPlan(false, false, false, false, false)
	readOnly := NewCapabilityPlan(true, false, false, false, false)
	if readWrite.ToolCount() != 10 {
		t.Fatalf("default tool count = %d, want 10", readWrite.ToolCount())
	}
	if readOnly.ToolCount() != 7 {
		t.Fatalf("default read-only tool count = %d, want 7", readOnly.ToolCount())
	}
	if got := NewCapabilityPlan(false, true, true, true, true).ToolCount(); got != 23 {
		t.Fatalf("maximum tool count = %d, want 23", got)
	}
	if !slices.Contains(readWrite.RegisteredTools(), "icloud_capabilities") {
		t.Fatal("default plan must include icloud_capabilities")
	}
}

func TestRegister_DeleteEventHasDestructiveAnnotation(t *testing.T) {
	s := newTestServer(false)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "delete_event" {
			continue
		}
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("delete_event should have DestructiveHint=true")
		}
		return
	}
	t.Fatal("delete_event tool not found")
}

func TestRegister_UpdateEventIsNotIdempotentHint(t *testing.T) {
	// update_event bumps SEQUENCE and rewrites DTSTAMP on every successful
	// call, and a conditional PUT means a blind retry can hit a 412
	// concurrent_modification instead of repeating the same effect. The
	// IdempotentHint annotation must stay false so hosts that auto-retry
	// idempotent tools do not retry this one.
	s := newTestServer(false)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "update_event" {
			continue
		}
		if tool.Annotations.IdempotentHint != nil && *tool.Annotations.IdempotentHint {
			t.Errorf("update_event should not have IdempotentHint=true")
		}
		return
	}
	t.Fatal("update_event tool not found")
}
