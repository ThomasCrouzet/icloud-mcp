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

// newTestServer construit un *server.MCPServer avec les tools enregistrés
// (via mcptools.Register) contre un MockService, exactement le câblage
// utilisé en production, moins le vrai client iCloud.
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

// listToolNames se connecte au serveur via un client MCP in-process (comme
// un vrai client stdio le ferait), initialise la session, puis liste les
// tools exposés, c'est le test exigé par la spec DoD : READ_ONLY doit
// retirer les tools d'écriture de tools/list, pas juste les refuser à
// l'exécution.
func listToolNames(t *testing.T, s *server.MCPServer) []string {
	t.Helper()
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient : %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start : %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize : %v", err)
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools : %v", err)
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
		t.Fatalf("READ_ONLY : %d tools enregistrés, want 2 : %v", len(names), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("tool inattendu en mode READ_ONLY : %q", n)
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
		t.Fatalf("mode complet : %d tools enregistrés, want 5 : %v", len(names), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("tool inattendu : %q", n)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("tools manquants : %v", want)
	}
}

func TestRegister_DeleteEventHasDestructiveAnnotation(t *testing.T) {
	s := newTestServer(false)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient : %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start : %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize : %v", err)
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools : %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "delete_event" {
			continue
		}
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("delete_event devrait avoir DestructiveHint=true")
		}
		return
	}
	t.Fatal("tool delete_event non trouvé")
}
