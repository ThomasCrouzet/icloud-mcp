package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestVersionNonEmpty(t *testing.T) {
	if version == "" {
		t.Fatal("version must be non-empty (overridden at release via -ldflags)")
	}
}

func TestToolAndDiscoveryTimeoutsOrdered(t *testing.T) {
	// Production invariant: tool timeout < HTTP client timeout (30s) and
	// discovery timeout < tool timeout so boot fails cleanly first.
	if toolTimeout >= 30*time.Second {
		t.Fatalf("toolTimeout = %v, want < 30s HTTP timeout", toolTimeout)
	}
	if discoveryTimeout >= toolTimeout {
		t.Fatalf("discoveryTimeout = %v, want < toolTimeout %v", discoveryTimeout, toolTimeout)
	}
}

func TestTimeoutMiddleware(t *testing.T) {
	mw := timeoutMiddleware(30 * time.Millisecond)
	// Handler ignores cancellation; middleware must still return at the deadline.
	slow := mw(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		time.Sleep(500 * time.Millisecond)
		return mcp.NewToolResultText("late"), nil
	})
	start := time.Now()
	res, err := slow(context.Background(), mcp.CallToolRequest{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want nil tool error result", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("result = %+v, want isError timeout payload", res)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("elapsed = %v, want preemptive return near 30ms", elapsed)
	}
}

func TestTimeoutMiddlewarePasses(t *testing.T) {
	mw := timeoutMiddleware(time.Second)
	h := mw(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline on context")
		}
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := h(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected result content")
	}
}

// TestBootRedactorMasksEnabledDomainVariants mirrors main's credential pairs,
// including Mail SASL PLAIN forms.
func TestBootRedactorMasksEnabledDomainVariants(t *testing.T) {
	cfg := &config.Config{
		Email:        "calendar-user@example.com",
		Password:     "calendar-app-secret",
		EnableMail:   true,
		MailAddress:  "mail-user@icloud.com",
		MailPassword: "mail-app-secret",
	}
	red := newBootRedactor(cfg)
	samples := []string{
		cfg.Email,
		cfg.Password,
		base64.StdEncoding.EncodeToString([]byte(cfg.Email + ":" + cfg.Password)),
		cfg.MailAddress,
		cfg.MailPassword,
		base64.StdEncoding.EncodeToString([]byte("\x00" + cfg.MailAddress + "\x00" + cfg.MailPassword)),
		base64.StdEncoding.EncodeToString([]byte(cfg.MailAddress + "\x00" + cfg.MailAddress + "\x00" + cfg.MailPassword)),
	}
	for _, s := range samples {
		out := red.Redact("leak:" + s)
		if strings.Contains(out, s) {
			t.Errorf("secret still present after redact in %q", out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("expected [REDACTED] in %q", out)
		}
	}
}

func TestBootRedactorExcludesDisabledMailCredentials(t *testing.T) {
	cfg := &config.Config{
		Email:        "calendar-user@example.com",
		Password:     "calendar-app-secret",
		EnableMail:   false,
		MailAddress:  "disabled-mail@icloud.com",
		MailPassword: "disabled-mail-secret",
	}
	output := newBootRedactor(cfg).Redact(cfg.MailAddress + " " + cfg.MailPassword)
	if output != cfg.MailAddress+" "+cfg.MailPassword {
		t.Fatalf("disabled Mail credentials were unexpectedly registered: %q", output)
	}
}

func TestOptionalServiceConstructionIsLazyAndGated(t *testing.T) {
	base := config.Config{
		Email:        "calendar-user@example.com",
		Password:     "calendar-app-secret",
		MailAddress:  "mail-user@icloud.com",
		MailPassword: "mail-app-secret",
		Timeout:      time.Second,
	}
	contactsService, mailService, err := newOptionalServices(&base)
	if err != nil || contactsService != nil || mailService != nil {
		t.Fatalf("disabled optional services = contacts:%T mail:%T err:%v", contactsService, mailService, err)
	}

	contactsConfig := base
	contactsConfig.EnableContacts = true
	contactsService, mailService, err = newOptionalServices(&contactsConfig)
	if err != nil || contactsService == nil || mailService != nil {
		t.Fatalf("Contacts construction = contacts:%T mail:%T err:%v", contactsService, mailService, err)
	}

	mailConfig := base
	mailConfig.EnableMail = true
	if err := mailConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	contactsService, mailService, err = newOptionalServices(&mailConfig)
	if err != nil || contactsService != nil || mailService == nil {
		t.Fatalf("Mail construction = contacts:%T mail:%T err:%v", contactsService, mailService, err)
	}

	readOnlySendConfig := base
	readOnlySendConfig.EnableMail = true
	readOnlySendConfig.EnableMailSend = true
	readOnlySendConfig.ReadOnly = true
	readOnlySendConfig.SMTPAllowedRecipients = []string{"allowed@example.com"}
	if err := readOnlySendConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	_, mailService, err = newOptionalServices(&readOnlySendConfig)
	if err != nil || mailService == nil || readOnlySendConfig.EffectiveMailSend() {
		t.Fatalf("read-only Mail send construction = mail:%T effective:%t err:%v",
			mailService, readOnlySendConfig.EffectiveMailSend(), err)
	}

	sendConfig := base
	sendConfig.EnableMail = true
	sendConfig.EnableMailSend = true
	sendConfig.SMTPAllowedRecipients = []string{"allowed@example.com"}
	if err := sendConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	_, mailService, err = newOptionalServices(&sendConfig)
	if err != nil || mailService == nil || !sendConfig.EffectiveMailSend() {
		t.Fatalf("enabled Mail send construction = mail:%T effective:%t err:%v",
			mailService, sendConfig.EffectiveMailSend(), err)
	}
}

// TestRegisterReadOnlyWiring ensures production unified registration exposes
// the seven default read/local tools and hides writes.
func TestRegisterReadOnlyWiring(t *testing.T) {
	s := server.NewMCPServer("icloud-mcp-test", "test",
		server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(timeoutMiddleware(toolTimeout)),
		server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(security.NewRedactor("x"))),
	)
	plan := mcptools.NewCapabilityPlan(true, false, false, false, false)
	mcptools.RegisterUnified(s, mcptools.Deps{
		Service:         &icloud.MockService{},
		Audit:           security.NewAuditLogger(ioDiscard{}),
		Redactor:        security.NewRedactor("secret-password-xx"),
		DefaultLocation: time.UTC,
		Version:         version,
		HealthEnabled:   false,
	}, plan)
	tools := s.ListTools()
	if len(tools) != 7 || tools["icloud_capabilities"] == nil {
		t.Fatalf("default read-only inventory = %v, want 7 tools including icloud_capabilities", tools)
	}
	for _, name := range []string{"create_event", "update_event", "delete_event"} {
		if tools[name] != nil {
			t.Errorf("read-only inventory contains %q", name)
		}
	}
}

func TestProductionServerEnforcesStrictInputSchemas(t *testing.T) {
	redactor := security.NewRedactor("unused-secret")
	mcpServer := newMCPServer(redactor)
	svc := &icloud.MockService{}
	mcptools.RegisterUnified(mcpServer, mcptools.Deps{
		Service: svc, Redactor: redactor, DefaultLocation: time.UTC,
	}, mcptools.NewCapabilityPlan(true, false, false, false, false))

	searchTool := mcpServer.ListTools()["search_events"]
	if searchTool == nil {
		t.Fatal("search_events was not registered")
	}
	additionalProperties, ok := searchTool.Tool.InputSchema.AdditionalProperties.(bool)
	if !ok || additionalProperties {
		t.Fatalf("search_events additionalProperties = %#v, want false", searchTool.Tool.InputSchema.AdditionalProperties)
	}

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
	initialize.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "test"}
	if _, err := inProcess.Initialize(ctx, initialize); err != nil {
		t.Fatal(err)
	}
	request := mcp.CallToolRequest{}
	request.Params.Name = "search_events"
	request.Params.Arguments = map[string]any{
		"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
		"calendar": "/cal/home/", "unknown_argument": true,
	}
	result, err := inProcess.CallTool(ctx, request)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if svc.SearchCallCount != 0 {
		t.Fatalf("schema-invalid call reached service %d time(s)", svc.SearchCallCount)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
