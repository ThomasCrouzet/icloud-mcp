package mcptools

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// instructionsCase couples one effective plan with the domain groups whose
// tools it registers, in Calendar, Contacts, Mail order.
type instructionsCase struct {
	name    string
	plan    CapabilityPlan
	domains [3]bool
}

func instructionsCases(t *testing.T) []instructionsCase {
	t.Helper()
	permissions := func(value string, readOnly bool) CapabilityPlan {
		t.Helper()
		p, err := config.ParsePermissions(value)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{Permissions: p, ReadOnly: readOnly}
		return NewCapabilityPlanFromConfig(readOnly, cfg.Capabilities())
	}
	return []instructionsCase{
		{name: "default", plan: NewCapabilityPlan(false, false, false, false, false), domains: [3]bool{true, false, false}},
		{name: "read-only-default", plan: NewCapabilityPlan(true, false, false, false, false), domains: [3]bool{true, false, false}},
		{name: "calendar-only", plan: permissions("calendar.read,calendar.write,calendar.delete", false), domains: [3]bool{true, false, false}},
		{name: "contacts-only", plan: permissions("contacts.read", false), domains: [3]bool{false, true, false}},
		{name: "mail-only", plan: permissions("mail.read,mail.mutate,mail.send,mail.archive", false), domains: [3]bool{false, false, true}},
		{name: "send-only", plan: permissions("mail.send", false), domains: [3]bool{false, false, true}},
		{name: "read-only-mail", plan: permissions("mail.read", true), domains: [3]bool{false, false, true}},
		{name: "all-enabled", plan: NewCapabilityPlan(false, true, true, true, true), domains: [3]bool{true, true, true}},
		{name: "all-read-only", plan: NewCapabilityPlan(true, true, true, true, true), domains: [3]bool{true, true, true}},
	}
}

func unusedDial(context.Context) (net.Conn, error) { return nil, io.ErrClosedPipe }

// instructionsServer builds one in-process server from a plan with the same
// options that the production entry point uses.
func instructionsServer(t *testing.T, plan CapabilityPlan) *server.MCPServer {
	t.Helper()
	policy, err := maildomain.ParseRecipientPolicy("recipient@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	mail, err := maildomain.NewService(maildomain.Config{
		Address: "fixture@icloud.com", Password: "synthetic-contract-secret", RecipientPolicy: policy,
	}, unusedDial, unusedDial, true, true)
	if err != nil {
		t.Fatal(err)
	}
	var mailService maildomain.Service
	if plan.MailEnabled() {
		mailService = mail
	}
	b := &contractRefusingBackend{}
	red := security.NewRedactor("synthetic-contract-secret")
	s := server.NewMCPServer("instructions-contract", "test",
		server.WithToolCapabilities(false), server.WithInputSchemaValidation(),
		server.WithStrictInputSchemaDefault(), server.WithRecovery(),
		server.WithInstructions(plan.Instructions()),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	ConfigureMailNumberValidation(s)
	RegisterUnified(s, Deps{
		Service:         icloud.NewClient(b, "https://caldav.icloud.com", func(h string) bool { return h == "caldav.icloud.com" }),
		ContactsService: contacts.NewClient(b, "https://contacts.icloud.com", func(h string) bool { return h == "contacts.icloud.com" }),
		MailService:     mailService, Redactor: red, Audit: security.NewAuditLogger(io.Discard),
	}, plan)
	return s
}

func instructionsInitialize(t *testing.T, plan CapabilityPlan) mcp.InitializeResult {
	t.Helper()
	c, err := client.NewInProcessClient(instructionsServer(t, plan))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "instructions-contract", Version: "test"}
	result, err := c.Initialize(t.Context(), init)
	if err != nil {
		t.Fatal(err)
	}
	return *result
}

func containsName(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// TestInitializeInstructionsMatchRegisteredSurface checks the wire-level
// initialize text against the exact registered manifest for every capability
// profile in the contract matrix. The text must name only the domain groups
// that have registered tools, route the host to tools/list and
// icloud_capabilities, and keep the untrusted-content rule.
func TestInitializeInstructionsMatchRegisteredSurface(t *testing.T) {
	universe := NewCapabilityPlan(false, true, true, true, true).RegisteredTools()
	for _, c := range instructionsCases(t) {
		t.Run(c.name, func(t *testing.T) {
			text := instructionsInitialize(t, c.plan).Instructions
			if text == "" {
				t.Fatal("initialize response carries no instructions")
			}
			if len(text) > 600 {
				t.Fatalf("instructions exceed 600 bytes: %d", len(text))
			}
			for _, token := range []string{"tools/list", "icloud_capabilities"} {
				if !strings.Contains(text, token) {
					t.Fatalf("instructions do not route the host to %s: %q", token, text)
				}
			}
			lower := strings.ToLower(text)
			if !strings.Contains(lower, "untrusted") {
				t.Fatalf("instructions lost the untrusted-content rule: %q", text)
			}
			for _, claim := range []string{"always available", "is always"} {
				if strings.Contains(lower, claim) {
					t.Fatalf("instructions claim a universally available domain: %q", text)
				}
			}
			for _, token := range []string{"@", "file://", "INBOX", "/cal", "example.invalid", "@icloud.com"} {
				if strings.Contains(text, token) {
					t.Fatalf("instructions contain private or resource data %q: %q", token, text)
				}
			}
			if c.plan.ReadOnly() != strings.Contains(text, "read-only") {
				t.Fatalf("read-only statement disagrees with the plan: %q", text)
			}
			for index, domain := range []string{"Calendar", "Contacts", "Mail"} {
				if claimed := strings.Contains(text, domain); claimed != c.domains[index] {
					t.Fatalf("instructions claim %s=%t, registered=%t: %q", domain, claimed, c.domains[index], text)
				}
			}
			registered := c.plan.RegisteredTools()
			for _, name := range universe {
				if !containsName(registered, name) && strings.Contains(text, name) {
					t.Fatalf("instructions name unregistered tool %s: %q", name, text)
				}
			}
			t.Logf("instructions_case=%s tools=%d text=%q", c.name, len(registered), text)
		})
	}
}

// TestInitializeInstructionsStableAcrossRuns keeps the text deterministic so a
// host can cache the handshake result without ordering surprises.
func TestInitializeInstructionsStableAcrossRuns(t *testing.T) {
	plan := NewCapabilityPlan(false, true, true, true, true)
	first := instructionsInitialize(t, plan).Instructions
	for range 3 {
		if again := instructionsInitialize(t, plan).Instructions; again != first {
			t.Fatalf("instructions changed between runs: %q != %q", again, first)
		}
	}
	if strings.Count(first, "Calendar") != 1 || strings.Count(first, "Contacts") != 1 || strings.Count(first, "Mail") != 1 {
		t.Fatalf("enabled domain list is not stated exactly once per domain: %q", first)
	}
}
