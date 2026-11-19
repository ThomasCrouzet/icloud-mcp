package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// instructionsProfile selects one shipped host profile plus an optional
// environment override. An override models a documented operator variant,
// such as Contacts without Calendar, without inventing product variables.
type instructionsProfile struct {
	label   string
	profile string
	env     map[string]string
	// domains lists the domain tool groups that must be present. The order is
	// Calendar, Contacts, Mail.
	domains [3]bool
}

func shippedInstructionsProfiles() []instructionsProfile {
	contactsOnly := map[string]string{
		"ICLOUD_MCP_PERMISSIONS":     "contacts.read,mail.read",
		"ICLOUD_MCP_ENABLE_CONTACTS": "true",
		"ICLOUD_EMAIL":               "instructions-calendar@example.invalid",
		"ICLOUD_PASSWORD":            "synthetic-instructions-password",
	}
	return []instructionsProfile{
		{label: "calendar-read-only", profile: "calendar-read-only", domains: [3]bool{true, false, false}},
		{label: "calendar-contacts-read-only", profile: "calendar-contacts-read-only", domains: [3]bool{true, true, false}},
		{label: "mail-read-only", profile: "mail-read-only", domains: [3]bool{false, false, true}},
		{label: "calendar-scoped-write", profile: "calendar-scoped-write", domains: [3]bool{true, false, false}},
		{label: "mail-exact-send", profile: "mail-exact-send", domains: [3]bool{false, false, true}},
		{label: "contacts-and-mail-read-only", profile: "mail-read-only", env: contactsOnly, domains: [3]bool{false, true, true}},
	}
}

func clearICloudEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ICLOUD_") {
			t.Setenv(key, "")
		}
	}
}

// loadInstructionsConfig reads one shipped host entry. Each file secret
// reference receives a synthetic value inside a test directory.
func loadInstructionsConfig(t *testing.T, name string) *config.Config {
	t.Helper()
	clearICloudEnvironment(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "profiles", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var host struct {
		Servers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &host); err != nil {
		t.Fatal(err)
	}
	for key, value := range host.Servers["icloud"].Env {
		if strings.HasPrefix(value, "file://") {
			file := filepath.Join(home, key)
			synthetic := "synthetic-instructions-password"
			if key == "ICLOUD_EMAIL" || key == "ICLOUD_MAIL_ADDRESS" {
				synthetic = strings.ToLower(key) + "@example.invalid"
			}
			if err := os.WriteFile(file, []byte(synthetic), 0o600); err != nil {
				t.Fatal(err)
			}
			value = "file://" + file
		}
		t.Setenv(key, value)
	}
	return mustLoadConfig(t)
}

func mustLoadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// declaredDomains is the capability report shape used by this file.
type declaredDomains struct {
	Calendar bool `json:"calendar"`
	Contacts bool `json:"contacts"`
	Mail     bool `json:"mail"`
}

// instructionsPeer drives the production server over bounded stdio with one
// request at a time. Sequential framing matters because EOF cancels active
// handlers, exactly as a real host that keeps the pipe open behaves.
type instructionsPeer struct {
	t      *testing.T
	stdin  *io.PipeWriter
	lines  *bufio.Scanner
	served chan error
	nextID int
}

func newInstructionsPeer(t *testing.T, cfg *config.Config) *instructionsPeer {
	t.Helper()
	plan := mcptools.NewCapabilityPlanFromConfig(cfg.ReadOnly, cfg.Capabilities())
	var calendar *icloud.Client
	if plan.CalendarReadEnabled() || plan.CalendarWritesEnabled() {
		calendar = icloud.NewClient(&countingFailDoer{}, security.ICloudBaseURL, security.IsICloudHost)
	}
	addressBooks, mail, err := newOptionalServices(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ContactsEnabled() != (addressBooks != nil) || plan.MailEnabled() != (mail != nil) {
		t.Fatal("optional service construction disagrees with the plan")
	}
	red := security.NewRedactor("synthetic-instructions-secret")
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- runServer(cfg, security.AuditFormatJSON, "", calendar, addressBooks, mail, stdinReader, stdoutWriter)
		_ = stdoutWriter.Close()
	}()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
	})
	_ = red
	return &instructionsPeer{t: t, stdin: stdinWriter, lines: bufio.NewScanner(stdoutReader), served: served}
}

func (p *instructionsPeer) request(method string, params map[string]any) map[string]any {
	p.t.Helper()
	p.nextID++
	id := p.nextID
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.stdin.Write(append(encoded, '\n')); err != nil {
		p.t.Fatalf("write %s: %v", method, err)
	}
	if !p.lines.Scan() {
		p.t.Fatalf("no response for %s: %v", method, p.lines.Err())
	}
	var response map[string]any
	if err := json.Unmarshal(p.lines.Bytes(), &response); err != nil {
		p.t.Fatalf("decode %s response %q: %v", method, p.lines.Text(), err)
	}
	if got, ok := response["id"].(float64); !ok || int(got) != id {
		p.t.Fatalf("%s response id = %v, want %d", method, response["id"], id)
	}
	if rpcErr, ok := response["error"]; ok {
		p.t.Fatalf("%s returned a JSON-RPC error: %v", method, rpcErr)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		p.t.Fatalf("%s has no result object: %v", method, response)
	}
	return result
}

// shutdown closes the input pipe and waits for the server to exit cleanly.
func (p *instructionsPeer) shutdown() {
	p.t.Helper()
	_ = p.stdin.Close()
	select {
	case err := <-p.served:
		if err != nil {
			p.t.Fatalf("server exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		p.t.Fatal("server did not stop after EOF")
	}
}

func (p *instructionsPeer) notify(method string) {
	p.t.Helper()
	if _, err := p.stdin.Write([]byte(`{"jsonrpc":"2.0","method":"` + method + `"}` + "\n")); err != nil {
		p.t.Fatalf("write %s: %v", method, err)
	}
}

// initializeStream returns the initialize result, the exact tools/list names,
// and the local domain report for one effective configuration.
func initializeStream(t *testing.T, cfg *config.Config) (map[string]any, []string, declaredDomains) {
	t.Helper()
	peer := newInstructionsPeer(t, cfg)
	init := peer.request("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "instructions-probe", "version": "test"},
	})
	peer.notify("notifications/initialized")
	listed := peer.request("tools/list", map[string]any{})
	rawTools, ok := listed["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list tools = %#v", listed["tools"])
	}
	names := make([]string, 0, len(rawTools))
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tools/list entry = %#v", raw)
		}
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	sort.Strings(names)

	called := peer.request("tools/call", map[string]any{
		"name": "icloud_capabilities", "arguments": map[string]any{},
	})
	if called["isError"] == true {
		t.Fatalf("icloud_capabilities failed: %v", called)
	}
	content, ok := called["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("icloud_capabilities content = %#v", called["content"])
	}
	textItem, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("icloud_capabilities item = %#v", content[0])
	}
	text, _ := textItem["text"].(string)
	var report struct {
		Domains declaredDomains `json:"domains"`
		Tools   []string        `json:"tools"`
		Count   int             `json:"toolCount"`
	}
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		t.Fatalf("icloud_capabilities payload %q: %v", text, err)
	}
	sorted := append([]string(nil), report.Tools...)
	sort.Strings(sorted)
	if strings.Join(sorted, ",") != strings.Join(names, ",") || report.Count != len(names) {
		t.Fatalf("icloud_capabilities tools %v count %d disagree with tools/list %v", sorted, report.Count, names)
	}
	peer.shutdown()
	return init, names, report.Domains
}

// disabledActionTokens maps one action word to the condition that removes it
// from every tool name and description.
func disabledActionTokens(plan mcptools.CapabilityPlan) map[string]bool {
	return map[string]bool{
		"calendar.write":  !plan.CalendarCreateUpdateEnabled(),
		"calendar.delete": !plan.CalendarDeleteEnabled(),
		"contacts.write":  !plan.ContactsCreateUpdateEnabled(),
		"contacts.delete": !plan.ContactsDeleteEnabled(),
		"mail.flag":       !plan.MailMutationsEnabled(),
		"mail.send":       !plan.MailSendEnabled(),
		"mail.archive":    !plan.MailArchiveEnabled(),
	}
}

// TestInitializeInstructionsMatchEffectiveSurface guards the initialize text
// for every shipped profile. The text must name exactly the domains whose tools
// are registered, point the host at tools/list and icloud_capabilities, and keep
// the untrusted-content guidance.
func TestInitializeInstructionsMatchEffectiveSurface(t *testing.T) {
	for _, profile := range shippedInstructionsProfiles() {
		t.Run(profile.label, func(t *testing.T) {
			cfg := loadInstructionsConfig(t, profile.profile)
			for key, value := range profile.env {
				t.Setenv(key, value)
			}
			if len(profile.env) > 0 {
				cfg = mustLoadConfig(t)
			}
			init, names, domains := initializeStream(t, cfg)
			instructions, _ := init["instructions"].(string)
			if instructions == "" {
				t.Fatal("initialize response has no instructions")
			}
			if len(instructions) > 600 {
				t.Fatalf("instructions exceed 600 bytes: %d", len(instructions))
			}
			for _, token := range []string{"tools/list", "icloud_capabilities"} {
				if !strings.Contains(instructions, token) {
					t.Fatalf("instructions do not route the host to %s: %q", token, instructions)
				}
			}
			if !strings.Contains(strings.ToLower(instructions), "untrusted") {
				t.Fatalf("instructions lost the untrusted-content guidance: %q", instructions)
			}
			for _, token := range []string{"@", "file://", "INBOX", "/cal", "example.invalid", "@icloud.com"} {
				if strings.Contains(instructions, token) {
					t.Fatalf("instructions contain private or resource data %q: %q", token, instructions)
				}
			}
			observed := [3]bool{domains.Calendar, domains.Contacts, domains.Mail}
			if observed != profile.domains {
				t.Fatalf("declared domains = %v, want %v (tools/list %v)", observed, profile.domains, names)
			}
			for index, domain := range []string{"Calendar", "Contacts", "Mail"} {
				if claimed := strings.Contains(instructions, domain); claimed != profile.domains[index] {
					t.Fatalf("instructions claim %s=%t while registered=%t: %q", domain, claimed, profile.domains[index], instructions)
				}
			}
			plan := mcptools.NewCapabilityPlanFromConfig(cfg.ReadOnly, cfg.Capabilities())
			for token, absent := range disabledActionTokens(plan) {
				if !absent {
					continue
				}
				if strings.Contains(strings.ToLower(instructions), token) {
					t.Fatalf("instructions advertise disabled capability %q: %q", token, instructions)
				}
			}
			for _, tool := range []string{"create_event", "update_event", "delete_event", "create_contact", "update_contact", "delete_contact", "set_message_flags", "move_message", "trash_message", "send_message", "reply_message"} {
				if strings.Contains(instructions, tool) && !containsString(names, tool) {
					t.Fatalf("instructions name disabled tool %s: %q", tool, instructions)
				}
			}
			if strings.Contains(strings.ToLower(instructions), "always available") || strings.Contains(strings.ToLower(instructions), "is always") {
				t.Fatalf("instructions still claim a universally available domain: %q", instructions)
			}
			t.Logf("instructions_profile=%s tools=%d text=%q", profile.label, len(names), instructions)
		})
	}
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
