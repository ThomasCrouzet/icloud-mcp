package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type contractProfile struct {
	name, scopes string
	plan         CapabilityPlan
}

func contractProfiles(t *testing.T) []contractProfile {
	t.Helper()
	return []contractProfile{
		{"default", "", NewCapabilityPlan(false, false, false, false, false)},
		{"read-only-default", "", NewCapabilityPlan(true, false, false, false, false)},
		{"calendar-only", "", replyScopePlan(t, "calendar.read,calendar.write,calendar.delete", false)},
		{"contacts-only", "", replyScopePlan(t, "contacts.read,contacts.write,contacts.delete", false)},
		{"mail-only", "", replyScopePlan(t, "mail.read,mail.mutate,mail.trash,mail.send,mail.archive", false)},
		{"send-only", "", replyScopePlan(t, "mail.send", false)},
		{"mixed-scoped", "calendar.read=/cal/allowed/;contacts.read=allowed-book;mail.read=INBOX;mail.mutate=INBOX;mail.archive=Sent",
			replyScopePlan(t, "calendar.read,contacts.read,mail.read,mail.mutate,mail.send,mail.archive", false)},
		{"all-enabled", "", NewCapabilityPlan(false, true, true, true, true)},
		{"all-read-only", "", NewCapabilityPlan(true, true, true, true, true)},
	}
}

type contractRefusingBackend struct {
	http, imap, smtp atomic.Int32
}

func (b *contractRefusingBackend) Do(*http.Request) (*http.Response, error) {
	b.http.Add(1)
	return nil, errors.New("unexpected synthetic HTTP request")
}

func contractManifestClient(t *testing.T, p contractProfile) (*client.Client, *contractRefusingBackend) {
	t.Helper()
	b := &contractRefusingBackend{}
	policy, err := maildomain.ParseRecipientPolicy("recipient@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	mail, err := maildomain.NewService(maildomain.Config{
		Address: "fixture@icloud.com", Password: "synthetic-contract-secret", RecipientPolicy: policy,
	}, func(context.Context) (net.Conn, error) {
		b.imap.Add(1)
		return nil, errors.New("unexpected synthetic IMAP dial")
	}, func(context.Context) (net.Conn, error) {
		b.smtp.Add(1)
		return nil, errors.New("unexpected synthetic SMTP dial")
	}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	red := security.NewRedactor("synthetic-contract-secret")
	s := server.NewMCPServer("contract-manifest", "test", server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(), server.WithRecovery(),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	RegisterUnified(s, Deps{
		Service:         icloud.NewClient(b, "https://caldav.icloud.com", func(h string) bool { return h == "caldav.icloud.com" }),
		ContactsService: contacts.NewClient(b, "https://contacts.icloud.com", func(h string) bool { return h == "contacts.icloud.com" }),
		MailService:     mail, Redactor: red, Audit: security.NewAuditLogger(io.Discard),
		DefaultLocation: time.UTC, ResourceScopes: mustScopes(t, p.scopes),
	}, p.plan)
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
	init.Params.ClientInfo = mcp.Implementation{Name: "contract-manifest", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c, b
}

func contractCompileSchema(t *testing.T, schema any) *jsonschema.Schema {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	const location = "https://contract.example.invalid/schema.json"
	if err := compiler.AddResource(location, doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		t.Fatalf("listed schema did not compile: %v", err)
	}
	return compiled
}

func contractWireTool(t *testing.T, tool mcp.Tool) map[string]any {
	t.Helper()
	var wire map[string]any
	if err := json.Unmarshal([]byte(contractReplayJSON(t, tool)), &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func contractValidateOutput(t *testing.T, tool mcp.Tool, result *mcp.CallToolResult) {
	t.Helper()
	output, declared := contractWireTool(t, tool)["outputSchema"]
	if !declared {
		return
	}
	if result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("%s has no successful structured output to validate", tool.Name)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewBufferString(contractReplayJSON(t, result.StructuredContent)))
	if err != nil {
		t.Fatal(err)
	}
	if err := contractCompileSchema(t, output).Validate(value); err != nil {
		t.Fatalf("%s structured output violates actual tools/list schema: %v", tool.Name, err)
	}
}

func contractCall(t *testing.T, c *client.Client, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = name, args
	return c.CallTool(t.Context(), req)
}

func TestContractManifestProfiles(t *testing.T) {
	profiles := contractProfiles(t)
	allClient, _ := contractManifestClient(t, contractProfile{name: "universe", plan: NewCapabilityPlan(false, true, true, true, true)})
	universe, err := allClient.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			c, backend := contractManifestClient(t, p)
			listed, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("contract_manifest=%s", contractReplayJSON(t, map[string]any{"profile": p.name, "scopes": p.scopes, "tools": listed.Tools}))
			names := make([]string, 0, len(listed.Tools))
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
				wire := contractWireTool(t, tool)
				contractCompileSchema(t, wire["inputSchema"])
				if output, declared := wire["outputSchema"]; declared {
					contractCompileSchema(t, output)
				}
			}
			sort.Strings(names)
			if !slices.Equal(names, p.plan.RegisteredTools()) {
				t.Fatalf("tools/list differs from plan: %v", names)
			}
			res, err := contractCall(t, c, "icloud_capabilities", map[string]any{})
			if err != nil || res == nil || res.IsError {
				t.Fatalf("capabilities: result=%v err=%v", res, err)
			}
			var got icloudCapabilitiesResponse
			if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
				t.Fatal(err)
			}
			wantDomains := icloudDomains{p.plan.CalendarReadEnabled() || p.plan.CalendarWritesEnabled(), p.plan.ContactsEnabled(), p.plan.MailEnabled()}
			wantGroups := icloudCapabilityGroups{p.plan.calendarRead, p.plan.calendarWrite, p.plan.calendarDelete,
				p.plan.contactsRead, p.plan.contactsWrite, p.plan.contactsDelete, p.plan.mailRead, p.plan.mailMutate,
				p.plan.mailTrash, p.plan.mailSend, p.plan.mailReply}
			if !slices.Equal(got.Tools, names) || got.ToolCount != len(names) || got.ReadOnly != p.plan.ReadOnly() ||
				!reflect.DeepEqual(got.Domains, wantDomains) || !reflect.DeepEqual(got.CapabilityGroups, wantGroups) {
				t.Fatalf("capabilities disagree with actual manifest: %+v", got)
			}
			for _, tool := range listed.Tools {
				if tool.Name == "icloud_capabilities" {
					contractValidateOutput(t, tool, res)
					contractOutputNegativeControl(t, tool, res)
				}
				if tool.Name == "calendar_capabilities" {
					local, err := contractCall(t, c, tool.Name, map[string]any{})
					if err != nil || local == nil || local.IsError {
						t.Fatalf("calendar capabilities: %v %v", local, err)
					}
					var calendar capabilitiesResponse
					if err := json.Unmarshal([]byte(resultText(t, local)), &calendar); err != nil {
						t.Fatal(err)
					}
					if calendar.ReadOnly != p.plan.ReadOnly() || calendar.DefaultTimezone != "UTC" || calendar.OutputFormat != "RFC3339_with_offset" {
						t.Fatalf("local calendar capabilities changed: %+v", calendar)
					}
					contractValidateOutput(t, tool, local)
					contractOutputNegativeControl(t, tool, local)
					t.Logf("contract_local_output=%s", contractReplayJSON(t, map[string]any{"profile": p.name, "tool": tool.Name, "result": local}))
				}
				if tool.Name == "icloud_capabilities" || tool.Name == "calendar_capabilities" {
					invalid, err := contractCall(t, c, tool.Name, map[string]any{"undeclared_contract_key": true})
					public := fmtContractError(err)
					if invalid != nil {
						public += resultText(t, invalid)
					}
					if !strings.Contains(public, "input schema validation failed") {
						t.Fatalf("unknown local argument was not refused by the production-equivalent SDK validator: %s", public)
					}
					t.Logf("contract_unknown=%s", contractReplayJSON(t, map[string]any{"profile": p.name, "tool": tool.Name, "result": invalid, "error": fmtContractError(err)}))
				}
			}
			t.Logf("contract_capabilities=%s", contractReplayJSON(t, map[string]any{"profile": p.name, "result": res}))
			for _, tool := range universe.Tools {
				if slices.Contains(names, tool.Name) {
					continue
				}
				res, err := contractCall(t, c, tool.Name, map[string]any{})
				t.Logf("contract_disabled=%s", contractReplayJSON(t, map[string]any{"profile": p.name, "tool": tool.Name, "result": res, "error": fmtContractError(err)}))
				if err == nil && (res == nil || !res.IsError) {
					t.Errorf("disabled %s dispatched", tool.Name)
				}
			}
			if backend.http.Load() != 0 || backend.imap.Load() != 0 || backend.smtp.Load() != 0 {
				t.Fatalf("local/disabled calls reached a backend: http=%d imap=%d smtp=%d", backend.http.Load(), backend.imap.Load(), backend.smtp.Load())
			}
			t.Log("contract_backend_counts={\"http\":0,\"imap\":0,\"smtp\":0}")
		})
	}
}

func contractOutputNegativeControl(t *testing.T, tool mcp.Tool, result *mcp.CallToolResult) {
	t.Helper()
	var mutated map[string]any
	if err := json.Unmarshal([]byte(contractReplayJSON(t, result.StructuredContent)), &mutated); err != nil {
		t.Fatal(err)
	}
	mutated["readOnly"] = "not-a-boolean"
	output := contractWireTool(t, tool)["outputSchema"]
	if err := contractCompileSchema(t, output).Validate(mutated); err == nil {
		t.Fatalf("%s output oracle accepted a reachable wrong-primitive mutation of actual structuredContent", tool.Name)
	}
	t.Logf("contract_output_negative_control=%s", tool.Name)
}

func fmtContractError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
