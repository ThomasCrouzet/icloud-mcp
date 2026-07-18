package mcptools

import (
	"context"
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

	want := map[string]bool{"list_calendars": true, "search_events": true}
	if len(names) != 2 {
		t.Fatalf("READ_ONLY: %d tools registered, want 2: %v", len(names), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected tool in READ_ONLY mode: %q", n)
		}
	}
}

func TestRegister_FullModeExposesAllFiveTools(t *testing.T) {
	s := newTestServer(false)
	names := listToolNames(t, s)

	want := map[string]bool{
		"list_calendars": true, "search_events": true,
		"create_event": true, "update_event": true, "delete_event": true,
	}
	if len(names) != 5 {
		t.Fatalf("full mode: %d tools registered, want 5: %v", len(names), names)
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
