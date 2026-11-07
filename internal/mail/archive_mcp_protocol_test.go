package mail_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestProtocolArchiveMCPToSMTPAndIMAP(t *testing.T) {
	for _, tc := range []struct {
		name, scope, outcome string
		enabled              bool
		preference           any
		omit, rejected       bool
		want                 [5]int32
	}{
		{"stored", "Private Sent", "stored", true, true, false, false, [5]int32{2, 1, 1, 1, 1}},
		{"failed", "Private Sent", "failed", true, true, false, false, [5]int32{2, 1, 1, 1, 1}},
		{"unknown", "Private Sent", "unknown", true, true, false, false, [5]int32{2, 1, 1, 1, 1}},
		{"false", "Other", "not_attempted", true, false, false, false, [5]int32{0, 0, 1, 1, 0}},
		{"omitted", "Other", "not_attempted", true, nil, true, false, [5]int32{0, 0, 1, 1, 0}},
		{"denied", "Other", "", true, true, false, true, [5]int32{1, 1, 0, 0, 0}},
		{"disabled", "Private Sent", "", false, true, false, true, [5]int32{}},
		{"malformed-string", "Private Sent", "", true, "true", false, true, [5]int32{}},
		{"malformed-null", "Private Sent", "", true, nil, false, true, [5]int32{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, counts := maildomain.NewArchiveMCPFixture(t, tc.scope, tc.outcome, tc.enabled)
			red := security.NewRedactor("fixture@icloud.com", "synthetic-archive-secret")
			var audit bytes.Buffer
			s := server.NewMCPServer("archive-protocol", "test", server.WithToolCapabilities(false),
				server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(),
				server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(red)))
			permissions := "mail.send"
			if tc.enabled {
				permissions += ",mail.archive"
			}
			p, err := config.ParsePermissions(permissions)
			if err != nil {
				t.Fatal(err)
			}
			scopes, err := config.ParseResourceScopes("mail.archive=" + tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Permissions: p}
			mcptools.RegisterUnified(s, mcptools.Deps{Service: &icloud.MockService{}, MailService: svc, Redactor: red, Audit: security.NewAuditLogger(&audit), ResourceScopes: scopes},
				mcptools.NewCapabilityPlanFromConfig(false, cfg.Capabilities()))
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
			init.Params.ClientInfo = mcp.Implementation{Name: "archive-protocol-test", Version: "test"}
			if _, err := c.Initialize(t.Context(), init); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"to": []string{"to@example.invalid"}, "body": "synthetic archive body", "idempotency_key": "full-mcp-key"}
			if !tc.omit {
				args["archive_sent"] = tc.preference
			}
			call := func(arguments map[string]any) (*mcp.CallToolResult, map[string]any) {
				req := mcp.CallToolRequest{}
				req.Params.Name = "send_message"
				req.Params.Arguments = arguments
				res, err := c.CallTool(t.Context(), req)
				if err != nil {
					t.Errorf("MCP dispatch failed: %v", err)
					return nil, nil
				}
				var payload map[string]any
				for _, content := range res.Content {
					if text, ok := content.(mcp.TextContent); ok {
						_ = json.Unmarshal([]byte(text.Text), &payload)
					}
				}
				return res, payload
			}
			var wg sync.WaitGroup
			results := make(chan map[string]any, 2)
			for range 2 {
				wg.Go(func() {
					res, payload := call(args)
					if res == nil {
						return
					}
					if res.IsError != tc.rejected {
						t.Errorf("unexpected MCP error state: %+v", payload)
					}
					if !tc.rejected && (payload["status"] != "accepted" || payload["archivalStatus"] != tc.outcome) {
						t.Errorf("SMTP/archival outcome lost: %+v", payload)
					}
					results <- payload
				})
			}
			wg.Wait()
			close(results)
			var id any
			for payload := range results {
				if !tc.rejected {
					if id != nil && payload["messageId"] != id {
						t.Error("duplicate changed Message-ID")
					}
					id = payload["messageId"]
				}
				t.Logf("mcp_payload=%v", payload)
			}
			if !tc.rejected {
				args["archive_sent"] = tc.omit || tc.preference == false
				res, payload := call(args)
				if res == nil || !res.IsError || payload["code"] != "conflict" {
					t.Errorf("changed preference did not conflict through MCP: %+v", payload)
				}
			}
			t.Logf("imap/list/smtp/data/append=%v audit=%s", counts(), audit.String())
			if counts() != tc.want {
				t.Errorf("MCP protocol side effects = %v want %v", counts(), tc.want)
			}
			for _, private := range []string{"Private Sent", "to@example.invalid", "fixture@icloud.com", "synthetic-archive-secret", "private-server-detail"} {
				if strings.Contains(audit.String(), private) {
					t.Errorf("private value in audit: %q", private)
				}
			}
		})
	}
}
