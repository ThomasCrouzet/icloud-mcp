package mcptools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestProtocolMailArchiveArgument(t *testing.T) {
	for _, tc := range []struct {
		name          string
		value         any
		present, want bool
	}{
		{"true", true, true, true}, {"false", false, true, false}, {"omitted", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &mailToolsFakeService{sendResult: maildomain.SendResult{Status: maildomain.SendAccepted}}
			f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.send,mail.archive", false), "")
			listed, err := f.client.ListTools(t.Context(), mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, tool := range listed.Tools {
				if tool.Name != "send_message" {
					continue
				}
				encoded, err := json.Marshal(tool)
				if err != nil {
					t.Fatal(err)
				}
				var schema struct {
					Input map[string]any `json:"inputSchema"`
				}
				if err := json.Unmarshal(encoded, &schema); err != nil {
					t.Fatal(err)
				}
				property := schema.Input["properties"].(map[string]any)["archive_sent"].(map[string]any)
				if property["type"] != "boolean" || property["default"] != false {
					t.Fatalf("archive schema: %s", encoded)
				}
				found = true
			}
			if !found {
				t.Fatal("send_message missing")
			}
			args := map[string]any{"to": []string{"to@example.invalid"}, "body": "synthetic body"}
			if tc.present {
				args["archive_sent"] = tc.value
			}
			req := mailToolsRequest(args)
			req.Params.Name = "send_message"
			res, err := f.client.CallTool(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("mcp_result=%s service_send=%d archive=%v", mailToolsText(t, res), svc.sendCalls, svc.sendInput.ArchiveSent)
			if res.IsError || svc.sendCalls != 1 || svc.sendInput.ArchiveSent != tc.want {
				t.Fatalf("schema/handler mismatch: %s input=%+v calls=%d", mailToolsText(t, res), svc.sendInput, svc.sendCalls)
			}
		})
	}
}

func TestProtocolMailArchiveMalformed(t *testing.T) {
	for _, value := range []any{"true", "false", 1, 0, nil, []any{true}, map[string]any{"value": true}} {
		t.Run(stringMustJSON(t, value), func(t *testing.T) {
			svc := &mailToolsFakeService{}
			f := newReplyScopeProtocol(t, svc, replyScopePlan(t, "mail.send,mail.archive", false), "")
			args := map[string]any{"to": []string{"to@example.invalid"}, "body": "synthetic body", "archive_sent": value}
			req := mailToolsRequest(args)
			req.Params.Name = "send_message"
			res, err := f.client.CallTool(t.Context(), req)
			if err == nil && (res == nil || !res.IsError) {
				t.Fatalf("strict MCP accepted %v", value)
			}
			res, err = sendMessageHandler(MailDeps{Service: svc, Redactor: security.NewRedactor("synthetic-secret")})(t.Context(), req)
			if err != nil || !res.IsError || !strings.Contains(mailToolsText(t, res), "must be a boolean") {
				t.Fatalf("handler accepted/coerced invalid boolean: %v %v", res, err)
			}
			if svc.sendCalls != 0 {
				t.Fatalf("malformed input reached service: %d", svc.sendCalls)
			}
		})
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProtocolMailArchiveRealPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, scope      string
		enabled, archive bool
		imap, smtp       int32
	}{
		{"allowed-positive-boundary", "Private Sent", true, true, 1, 1},
		{"denied-before-smtp", "Other", true, true, 1, 0},
		{"capability-disabled", "Private Sent", false, true, 0, 0},
		{"false-needs-no-archive", "Other", false, false, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var imap, smtp atomic.Int32
			policy, err := maildomain.ParseRecipientPolicy("to@example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			svc, err := maildomain.NewService(maildomain.Config{Address: "fixture@icloud.com", Password: "synthetic-archive-secret", RecipientPolicy: policy, EnableSentCopy: tc.enabled, ArchiveScope: []string{tc.scope}},
				func(context.Context) (net.Conn, error) {
					imap.Add(1)
					local, remote := net.Pipe()
					go func() {
						defer func() { _ = remote.Close() }()
						_ = remote.SetDeadline(time.Now().Add(3 * time.Second))
						if _, err := fmt.Fprint(remote, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
							return
						}
						r := bufio.NewReader(remote)
						for {
							line, err := r.ReadString('\n')
							if err != nil {
								return
							}
							fields := strings.Fields(line)
							if len(fields) < 2 {
								return
							}
							var response string
							switch fields[1] {
							case "LOGIN":
								response = fields[0] + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
							case "LIST":
								response = "* LIST (\\Sent) \"/\" \"Private Sent\"\r\n" + fields[0] + " OK listed\r\n"
							case "LOGOUT":
								_, _ = fmt.Fprint(remote, "* BYE closing\r\n"+fields[0]+" OK logout\r\n")
								return
							default:
								response = fields[0] + " BAD unexpected\r\n"
							}
							if _, err := fmt.Fprint(remote, response); err != nil {
								return
							}
						}
					}()
					return local, nil
				}, func(context.Context) (net.Conn, error) {
					smtp.Add(1)
					return nil, errors.New("synthetic SMTP boundary")
				}, false, true)
			if err != nil {
				t.Fatal(err)
			}
			permissions := "mail.send"
			if tc.enabled {
				permissions += ",mail.archive"
			}
			f := newReplyScopeProtocol(t, svc, replyScopePlan(t, permissions, false), "mail.archive="+tc.scope)
			req := mailToolsRequest(map[string]any{"to": []string{"to@example.invalid"}, "body": "synthetic body", "archive_sent": tc.archive})
			req.Params.Name = "send_message"
			res, err := f.client.CallTool(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("mcp=%s imap=%d smtp=%d audit=%s", mailToolsText(t, res), imap.Load(), smtp.Load(), f.audit.String())
			if !res.IsError || imap.Load() != tc.imap || smtp.Load() != tc.smtp {
				t.Fatalf("preflight boundary mismatch: %s imap=%d smtp=%d", mailToolsText(t, res), imap.Load(), smtp.Load())
			}
			if strings.Contains(mailToolsText(t, res), "Private Sent") || strings.Contains(f.audit.String(), "Private Sent") || strings.Contains(f.audit.String(), "to@example.invalid") || !strings.Contains(f.audit.String(), `"resourceToken":`) {
				t.Fatalf("unsafe response/audit: %s %s", mailToolsText(t, res), f.audit.String())
			}
		})
	}
}
