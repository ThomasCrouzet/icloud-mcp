package mcptools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
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

var setupProfiles = []struct {
	name, permissions string
	readOnly          bool
}{
	{"calendar-read-only", "calendar.read", true},
	{"calendar-contacts-read-only", "calendar.read,contacts.read", true},
	{"mail-read-only", "mail.read", true},
	{"calendar-scoped-write", "calendar.read,calendar.write", false},
	{"mail-exact-send", "mail.send", false},
}

// Read the shipped host entry, not a separately reconstructed test policy.
func loadSetupProfile(t *testing.T, name string) (*config.Config, map[string]string) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ICLOUD_") {
			t.Setenv(key, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join("..", "..", "examples", "profiles", name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var host struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&host); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatal("host JSON has trailing content")
	}
	entry, ok := host.Servers["icloud"]
	if !ok || len(host.Servers) != 1 || !filepath.IsAbs(entry.Command) {
		t.Fatal("expected one absolute-path icloud stdio process")
	}
	product, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for line := range strings.SplitSeq(string(product), "\n") {
		if strings.HasPrefix(line, "ICLOUD_") {
			key, _, _ := strings.Cut(line, "=")
			allowed[key] = true
		}
	}
	secrets := map[string]string{
		"ICLOUD_EMAIL": "profile-calendar@example.invalid", "ICLOUD_PASSWORD": "synthetic-calendar-password",
		"ICLOUD_MAIL_ADDRESS": "profile-mail@example.invalid", "ICLOUD_MAIL_PASSWORD": "synthetic-mail-password",
	}
	for key, value := range entry.Env {
		if !allowed[key] {
			t.Fatalf("unsupported profile variable %s", key)
		}
		if synthetic, secret := secrets[key]; secret {
			if !strings.HasPrefix(value, "file:///") {
				t.Fatalf("%s must use an absolute file secret reference", key)
			}
			file := filepath.Join(home, key)
			if err := os.WriteFile(file, []byte(synthetic), 0o600); err != nil {
				t.Fatal(err)
			}
			value = "file://" + file
			t.Logf("synthetic_fixture=%s mode=0600 sha256=%x", key, sha256.Sum256([]byte(synthetic)))
		}
		t.Setenv(key, value)
	}
	t.Logf("profile_source=%s sha256=%x", path, sha256.Sum256(data))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, entry.Env
}

func setupRuntime(t *testing.T, cfg *config.Config) (*contractRuntime, *bytes.Buffer) {
	t.Helper()
	f := &contractRuntime{calendar: newContractDAV(false), contacts: newContractDAV(true), mail: &contractMailBackend{}, tools: map[string]mcp.Tool{}}
	plan := NewCapabilityPlanFromConfig(cfg.ReadOnly, cfg.Capabilities())
	var calendar icloud.Service
	if plan.CalendarReadEnabled() || plan.CalendarWritesEnabled() {
		calendar = icloud.NewClient(f.calendar, security.ICloudBaseURL, security.IsICloudHost)
	}
	var people contacts.Service
	if plan.ContactsEnabled() {
		people = contacts.NewClient(f.contacts, security.ContactsBaseURL, security.IsContactsHost)
	}
	var mail maildomain.Service
	if plan.MailEnabled() {
		policy, err := maildomain.RecipientPolicyFromExact(cfg.SMTPAllowedRecipients)
		if cfg.EffectiveMailSend() && err != nil {
			t.Fatal(err)
		}
		mail, err = maildomain.NewService(maildomain.Config{Address: cfg.MailAddress, Password: cfg.MailPassword, RecipientPolicy: policy},
			func(context.Context) (net.Conn, error) {
				f.mail.mu.Lock()
				f.mail.imapDials++
				f.mail.mu.Unlock()
				local, remote := net.Pipe()
				go func() { f.mail.finish(f.mail.serveIMAP(remote)) }()
				return local, nil
			}, func(context.Context) (net.Conn, error) {
				f.mail.mu.Lock()
				f.mail.smtpDials++
				f.mail.mu.Unlock()
				local, remote := net.Pipe()
				go func() { f.mail.finish(f.mail.serveSMTP(remote)) }()
				return local, nil
			}, cfg.EffectiveMailWrite(), cfg.EffectiveMailSend())
		if err != nil {
			t.Fatal(err)
		}
	}
	var audit bytes.Buffer
	red := security.NewRedactor(cfg.Email, cfg.Password, cfg.MailAddress, cfg.MailPassword)
	s := server.NewMCPServer("setup-profile", "test", server.WithToolCapabilities(false), server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(), server.WithRecovery(), server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	ConfigureMailNumberValidation(s)
	RegisterUnified(s, Deps{Service: calendar, ContactsService: people, MailService: mail, Redactor: red, Audit: security.NewAuditLogger(&audit), DefaultLocation: cfg.DefaultLocation, ResourceScopes: cfg.ResourceScopes}, plan)
	var err error
	f.client, err = client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Close() })
	if err := f.client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "setup-profile", Version: "test"}
	if _, err := f.client.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	for _, tool := range contractListExact(t, f.client).Tools {
		f.tools[tool.Name] = tool
	}
	return f, &audit
}

func TestSetupProfiles(t *testing.T) {
	for _, p := range setupProfiles {
		t.Run(p.name, func(t *testing.T) {
			cfg, env := loadSetupProfile(t, p.name)
			if env["ICLOUD_MCP_PERMISSIONS"] != p.permissions || env["ICLOUD_MCP_READ_ONLY"] != map[bool]string{true: "true", false: "false"}[p.readOnly] {
				t.Fatal("profile changed its explicit intended permissions or read-only control")
			}
			if strings.HasPrefix(p.name, "mail-") && (cfg.Email != "" || cfg.Password != "" || env["ICLOUD_EMAIL"] != "" || env["ICLOUD_PASSWORD"] != "") {
				t.Fatal("Mail-only profile depends on Calendar credentials")
			}
			for _, variant := range []string{"exact", "unrelated-gates"} {
				t.Run(variant, func(t *testing.T) {
					if variant == "unrelated-gates" {
						for _, gate := range []string{"ICLOUD_MCP_ENABLE_CONTACTS", "ICLOUD_MCP_ENABLE_MAIL", "ICLOUD_MCP_ENABLE_MAIL_WRITE", "ICLOUD_MCP_ENABLE_MAIL_SEND"} {
							t.Setenv(gate, "true")
						}
						var err error
						cfg, err = config.Load()
						if err != nil {
							t.Fatal(err)
						}
					}
					f, _ := setupRuntime(t, cfg)
					before := f.counts()
					wantPermissions, err := config.ParsePermissions(p.permissions)
					if err != nil {
						t.Fatal(err)
					}
					want := NewCapabilityPlanFromConfig(p.readOnly, wantPermissions.Capabilities(p.readOnly))
					names := make([]string, 0, len(f.tools))
					for name := range f.tools {
						names = append(names, name)
					}
					sort.Strings(names)
					if !slices.Equal(names, want.RegisteredTools()) {
						t.Fatalf("manifest=%v want=%v", names, want.RegisteredTools())
					}
					res := f.call(t, "icloud_capabilities", map[string]any{})
					var got icloudCapabilitiesResponse
					decodeResult(t, res, &got)
					groups := icloudCapabilityGroups{want.calendarRead, want.calendarWrite, want.calendarDelete, want.contactsRead, want.contactsWrite, want.contactsDelete, want.mailRead, want.mailMutate, want.mailTrash, want.mailSend, want.mailReply}
					if res.IsError || !slices.Equal(got.Tools, names) || got.ToolCount != len(names) || got.ReadOnly != p.readOnly || got.CapabilityGroups != groups || got.Domains != (icloudDomains{want.CalendarReadEnabled() || want.CalendarWritesEnabled(), want.ContactsEnabled(), want.MailEnabled()}) {
						t.Fatalf("capabilities disagree: %+v", got)
					}
					denied := 0
					for _, name := range NewCapabilityPlan(false, true, true, true, true).RegisteredTools() {
						if slices.Contains(names, name) {
							continue
						}
						result, err := contractCall(t, f.client, name, map[string]any{})
						if err == nil && (result == nil || !result.IsError) {
							t.Fatalf("disabled %s dispatched", name)
						}
						denied++
					}
					if !reflect.DeepEqual(before, f.counts()) {
						t.Fatal("initialize, capabilities or denied calls accessed a backend")
					}
					t.Logf("setup_manifest=%s", contractReplayJSON(t, map[string]any{"profile": p.name, "variant": variant, "capabilities": got, "denied": denied, "counters": f.counts()}))
				})
			}
		})
	}
}

func TestSetupProfilesMissingGates(t *testing.T) {
	for name, gate := range map[string]string{"calendar-contacts-read-only": "ICLOUD_MCP_ENABLE_CONTACTS", "mail-read-only": "ICLOUD_MCP_ENABLE_MAIL", "mail-exact-send": "ICLOUD_MCP_ENABLE_MAIL"} {
		t.Run(name, func(t *testing.T) {
			_, _ = loadSetupProfile(t, name)
			t.Setenv(gate, "")
			cfg, err := config.Load()
			if err == nil || cfg != nil || !strings.Contains(err.Error(), gate) {
				t.Fatalf("missing domain gate did not fail closed: %v", err)
			}
			t.Logf("missing_gate=%s error=%s backend_constructed=false", gate, err)
		})
	}
}

func TestSetupProfilesReadOperations(t *testing.T) {
	for _, name := range []string{"calendar-read-only", "calendar-contacts-read-only", "mail-read-only"} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := loadSetupProfile(t, name)
			f, _ := setupRuntime(t, cfg)
			for _, tool := range []string{"list_calendars", "list_address_books", "list_mailboxes"} {
				if _, enabled := f.tools[tool]; !enabled {
					continue
				}
				result := f.call(t, tool, map[string]any{})
				if result.IsError {
					t.Fatalf("allowed read %s: %s", tool, resultText(t, result))
				}
				if tool == "list_mailboxes" {
					var listed maildomain.ListMailboxesResult
					decodeResult(t, result, &listed)
					if len(listed.Mailboxes) != 1 || listed.Mailboxes[0].Name != "INBOX" {
						t.Fatalf("Mail profile disclosed out-of-scope mailbox: %+v", listed)
					}
					before := f.counts()
					denied := f.call(t, "get_message", map[string]any{"mailbox": "forbidden-private", "uid_validity": 9, "uid": 2})
					// Mail read handlers currently wrap scope errors as validation results.
					if !denied.IsError || contractPayload(t, denied)["code"] != "validation" || !strings.Contains(resultText(t, denied), "outside the configured resource scope") || !reflect.DeepEqual(before, f.counts()) || strings.Contains(resultText(t, denied), "forbidden-private") {
						t.Fatal("Mail read scope denial accessed or disclosed the forbidden mailbox")
					}
				}
			}
			if f.calendar.snapshot()["mutations"] != 0 || f.contacts.snapshot()["mutations"] != 0 {
				t.Fatal("read operation mutated DAV")
			}
			counts := f.mail.snapshot()
			for _, effect := range []string{"stores", "moves", "appends", "smtp", "data"} {
				if counts[effect] != 0 {
					t.Fatalf("read operation performed %s", effect)
				}
			}
			t.Logf("setup_reads=%s", contractReplayJSON(t, f.counts()))
		})
	}
}

func TestSetupProfilesScopedCalendarEffects(t *testing.T) {
	cfg, env := loadSetupProfile(t, "calendar-scoped-write")
	if env["ICLOUD_MCP_RESOURCE_SCOPES"] != "calendar.read=/cal/allowed/;calendar.write=/cal/allowed/" {
		t.Fatal("Calendar example must explicitly scope reads and writes")
	}
	f, audit := setupRuntime(t, cfg)
	for _, name := range []string{"create_event", "update_event"} {
		t.Run(name, func(t *testing.T) {
			args := f.arguments(name, "")
			args["calendar"] = "/cal/forbidden-private/"
			before := f.counts()
			denied := f.call(t, name, args)
			if !denied.IsError || contractPayload(t, denied)["code"] != "authorization" || !reflect.DeepEqual(before, f.counts()) || strings.Contains(resultText(t, denied), "forbidden-private") || strings.Contains(audit.String(), "forbidden-private") {
				t.Fatal("scope denial accessed or disclosed the forbidden resource")
			}
			t.Logf("scope_deny=%s counters=%s result=%s", name, contractReplayJSON(t, f.counts()), resultText(t, denied))
			args["calendar"] = contractCalendar
			if name == "create_event" {
				args["client_uid"] = "profile-created"
			}
			allowed := f.call(t, name, args)
			if allowed.IsError {
				t.Fatalf("allowed Calendar write: %s", resultText(t, allowed))
			}
		})
	}
	if f.calendar.snapshot()["mutations"] != 2 {
		t.Fatal("allowed create/update did not each mutate once")
	}
	t.Logf("setup_calendar_effects=%s audit=%s", contractReplayJSON(t, f.counts()), audit.String())
}

func TestSetupProfilesExactRecipientEffects(t *testing.T) {
	cfg, env := loadSetupProfile(t, "mail-exact-send")
	if env["ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS"] != "recipient@example.invalid" || cfg.SMTPRecipientPolicy.AllowAll() {
		t.Fatal("send profile must use the exact synthetic recipient")
	}
	f, audit := setupRuntime(t, cfg)
	for _, field := range []string{"to", "cc", "bcc", "mixed"} {
		t.Run(field, func(t *testing.T) {
			args := map[string]any{"to": []string{"recipient@example.invalid"}, "body": "Synthetic profile body"}
			if field == "mixed" {
				args["to"] = []string{"recipient@example.invalid", "forbidden-private@example.invalid"}
			} else {
				args[field] = []string{"forbidden-private@example.invalid"}
			}
			before := f.counts()
			denied := f.call(t, "send_message", args)
			if !denied.IsError || contractPayload(t, denied)["code"] != "authorization" || !reflect.DeepEqual(before, f.counts()) || strings.Contains(resultText(t, denied), "forbidden-private") || strings.Contains(audit.String(), "forbidden-private") {
				t.Fatal("recipient denial reached backend or disclosed recipient")
			}
			t.Logf("recipient_deny=%s counters=%s result=%s", field, contractReplayJSON(t, f.counts()), resultText(t, denied))
		})
	}
	allowed := f.call(t, "send_message", map[string]any{"to": []string{"recipient@example.invalid"}, "body": "Synthetic profile body"})
	counts := f.mail.snapshot()
	if allowed.IsError || counts["smtp"] != 1 || counts["data"] != 1 || counts["tls"] != 1 || counts["imap"] != 0 || counts["appends"] != 0 {
		t.Fatalf("allowed exact send did not submit once without IMAP: %s %v", resultText(t, allowed), counts)
	}
	t.Logf("setup_send_effects=%s audit=%s", contractReplayJSON(t, f.counts()), audit.String())
}

func TestSetupProfilesLegacyUnset(t *testing.T) {
	_, _ = loadSetupProfile(t, "calendar-read-only")
	t.Setenv("ICLOUD_MCP_READ_ONLY", "")
	t.Setenv("ICLOUD_MCP_PERMISSIONS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	f, _ := setupRuntime(t, cfg)
	for _, name := range []string{"create_event", "update_event", "delete_event"} {
		if _, ok := f.tools[name]; !ok {
			t.Fatalf("legacy-unset compatibility changed: %s absent", name)
		}
	}
	t.Logf("legacy_unset_tools=%s", contractReplayJSON(t, NewCapabilityPlanFromConfig(cfg.ReadOnly, cfg.Capabilities()).RegisteredTools()))
}
