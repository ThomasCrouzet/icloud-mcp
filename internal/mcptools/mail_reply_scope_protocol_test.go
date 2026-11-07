package mcptools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const replyScopeMailbox = "Private source"

type replyScopeProtocol struct {
	client *client.Client
	audit  bytes.Buffer
}

func newReplyScopeProtocol(t *testing.T, svc maildomain.Service, plan CapabilityPlan, scope string) *replyScopeProtocol {
	t.Helper()
	f := &replyScopeProtocol{}
	red := security.NewRedactor("synthetic-reply-secret")
	s := server.NewMCPServer("reply-scope", "test", server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	RegisterUnified(s, Deps{Service: &icloud.MockService{}, MailService: svc, Redactor: red,
		Audit: security.NewAuditLogger(&f.audit), ResourceScopes: mustScopes(t, scope)}, plan)
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
	init.Params.ClientInfo = mcp.Implementation{Name: "reply-scope-test", Version: "test"}
	if _, err := f.client.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *replyScopeProtocol) call(t *testing.T, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mailToolsRequest(args)
	req.Params.Name = "reply_message"
	res, err := f.client.CallTool(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mcp_result=%s", encoded)
	return res
}

func replyScopeArgs() map[string]any {
	return map[string]any{"mailbox": replyScopeMailbox, "uid_validity": 9, "uid": 2, "body": "Synthetic reply"}
}

func replyScopePlan(t *testing.T, permissions string, readOnly bool) CapabilityPlan {
	t.Helper()
	p, err := config.ParsePermissions(permissions)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Permissions: p, ReadOnly: readOnly}
	return NewCapabilityPlanFromConfig(readOnly, cfg.Capabilities())
}

func assertReplyScopePrivate(t *testing.T, res *mcp.CallToolResult, audit string) {
	t.Helper()
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{replyScopeMailbox, "source-subject", "source-body", "reply@example.invalid", "<source@example.invalid>"} {
		if strings.Contains(string(encoded), value) || strings.Contains(audit, value) {
			t.Errorf("source information disclosed: response=%s audit=%s", encoded, audit)
		}
	}
}

func TestProtocolMailReplyScopePermissions(t *testing.T) {
	for _, registration := range []string{"legacy", "permissions"} {
		t.Run(registration, func(t *testing.T) {
			plan := NewCapabilityPlan(false, false, true, false, true)
			if registration == "permissions" {
				plan = replyScopePlan(t, "mail.read,mail.send", false)
			}
			for _, tc := range []struct {
				name, scope string
				allowed     bool
			}{
				{"default", "", true},
				{"wildcard", "mail.read=*", true},
				{"allowed", "mail.read=" + replyScopeMailbox, true},
				{"forbidden", "mail.read=INBOX", false},
				{"mutate-does-not-grant-read", "mail.read=INBOX;mail.mutate=" + replyScopeMailbox, false},
				{"archive-does-not-grant-read", "mail.read=INBOX;mail.archive=" + replyScopeMailbox, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					svc := &mailToolsFakeService{replyResult: maildomain.SendResult{Status: maildomain.SendAccepted, MessageID: "<reply@example.invalid>"}}
					f := newReplyScopeProtocol(t, svc, plan, tc.scope)
					res := f.call(t, replyScopeArgs())
					wantCalls := 0
					if tc.allowed {
						wantCalls = 1
						if res.IsError || mailToolsJSON(t, res)["status"] != string(maildomain.SendAccepted) {
							t.Errorf("allowed reply rejected: %s", mailToolsText(t, res))
						}
						if svc.replyInput.Mailbox != replyScopeMailbox || svc.replyInput.UIDValidity != 9 || svc.replyInput.UID != 2 {
							t.Errorf("source identity changed: %+v", svc.replyInput)
						}
					} else {
						if !res.IsError || mailToolsJSON(t, res)["status"] != string(maildomain.SendRejected) || !strings.Contains(mailToolsText(t, res), "outside the configured resource scope") {
							t.Errorf("forbidden reply not safely denied: %s", mailToolsText(t, res))
						}
						assertReplyScopePrivate(t, res, f.audit.String())
						if !strings.Contains(f.audit.String(), `"status":"denied"`) || !strings.Contains(f.audit.String(), `"domain":"mail"`) {
							t.Errorf("missing denied Mail audit: %s", f.audit.String())
						}
					}
					t.Logf("backend_reply=%d backend_get=%d backend_send=%d", svc.replyCalls, svc.getCalls, svc.sendCalls)
					if svc.replyCalls != wantCalls || svc.getCalls != 0 || svc.sendCalls != 0 {
						t.Errorf("backend calls: reply=%d get=%d send=%d, want %d/0/0", svc.replyCalls, svc.getCalls, svc.sendCalls, wantCalls)
					}
				})
			}
		})
	}
}

func TestProtocolMailReplyScopeCapabilityAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, permissions string
		readOnly          bool
		read, send        bool
	}{
		{"send-only", "mail.send", false, false, true},
		{"read-only-permission", "mail.read", false, true, false},
		{"global-read-only", "mail.read,mail.send", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &mailToolsFakeService{}
			f := newReplyScopeProtocol(t, svc, replyScopePlan(t, tc.permissions, tc.readOnly), "mail.read=INBOX")
			listed, err := f.client.ListTools(t.Context(), mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			has := map[string]bool{}
			for _, tool := range listed.Tools {
				has[tool.Name] = true
			}
			if has["reply_message"] || has["get_message"] != tc.read || has["send_message"] != tc.send {
				t.Errorf("unexpected tool surface: %v", has)
			}
			req := mailToolsRequest(replyScopeArgs())
			req.Params.Name = "reply_message"
			res, callErr := f.client.CallTool(t.Context(), req)
			t.Logf("absent_tool_result=%+v error=%v", res, callErr)
			if callErr == nil && (res == nil || !res.IsError) {
				t.Error("absent reply tool was callable")
			}
			if svc.replyCalls != 0 || svc.getCalls != 0 || svc.sendCalls != 0 {
				t.Error("absent tool reached Mail service")
			}
		})
	}
}

type replyScopeBackend struct {
	maildomain.Service
	reply, get, imap, metadata, smtp atomic.Int32
}

func (b *replyScopeBackend) ReplyMessage(ctx context.Context, input maildomain.ReplyInput) (maildomain.SendResult, error) {
	b.reply.Add(1)
	return b.Service.ReplyMessage(ctx, input)
}

func (b *replyScopeBackend) GetMessage(ctx context.Context, input maildomain.GetMessageInput) (maildomain.Message, error) {
	b.get.Add(1)
	return b.Service.GetMessage(ctx, input)
}

func newReplyScopeBackend(t *testing.T) *replyScopeBackend {
	t.Helper()
	b := &replyScopeBackend{}
	policy, err := maildomain.ParseRecipientPolicy("reply@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	b.Service, err = maildomain.NewService(maildomain.Config{Address: "fixture@icloud.com", Password: "synthetic-reply-secret", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) {
			b.imap.Add(1)
			local, remote := net.Pipe()
			go b.serveIMAP(remote)
			return local, nil
		}, func(context.Context) (net.Conn, error) {
			b.smtp.Add(1)
			return nil, errors.New("synthetic SMTP boundary, no connection")
		}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *replyScopeBackend) serveIMAP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}
	write := func(text string) bool { _, err := fmt.Fprint(conn, text); return err == nil }
	if !write("* OK [CAPABILITY IMAP4rev1] ready\r\n") {
		return
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return
		}
		tag := fields[0]
		var response string
		switch {
		case fields[1] == "LOGIN":
			response = tag + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
		case fields[1] == "EXAMINE":
			response = "* 1 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 3] next\r\n" + tag + " OK [READ-ONLY] examined\r\n"
		case strings.Contains(line, "UID FETCH 2 ") && strings.Contains(line, "BODYSTRUCTURE"):
			b.metadata.Add(1)
			headers := "Message-ID: <source@example.invalid>\r\nReply-To: reply@example.invalid\r\n\r\n"
			response = fmt.Sprintf("* 1 FETCH (UID 2 FLAGS () ENVELOPE (NIL \"source-subject\" NIL NIL NIL NIL NIL NIL NIL NIL) BODYSTRUCTURE (\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"7BIT\" 0 0) BODY[HEADER.FIELDS (MESSAGE-ID IN-REPLY-TO REFERENCES REPLY-TO)]<0> {%d}\r\n%s)\r\n%s OK metadata\r\n", len(headers), headers, tag)
		case strings.Contains(line, "UID FETCH 3 "):
			b.metadata.Add(1)
			response = tag + " OK no matching UID\r\n"
		case strings.Contains(line, "UID FETCH 2 ") && strings.Contains(line, "BODY.PEEK[1.MIME]"):
			header := "Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 7bit\r\n\r\n"
			body := "source-body"
			response = fmt.Sprintf("* 1 FETCH (UID 2 BODY[1.MIME]<0> {%d}\r\n%s BODY[1]<0> {%d}\r\n%s)\r\n%s OK body\r\n", len(header), header, len(body), body, tag)
		case fields[1] == "LOGOUT":
			write("* BYE closing\r\n" + tag + " OK logout\r\n")
			return
		default:
			response = tag + " BAD unexpected fixture command\r\n"
		}
		if !write(response) {
			return
		}
	}
}

func TestProtocolMailReplyScopeSourceIdentity(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		for _, tc := range []struct {
			name       string
			valid, uid any
			calls      [5]int32
			code       string
		}{
			// The adapter strips Message-ID brackets, so the current service rejects
			// this valid source before SMTP. Scope denial must precede even that read.
			{"valid-source", 9, 2, [5]int32{1, 0, 1, 1, 0}, "validation"},
			{"stale-validity", 8, 2, [5]int32{1, 0, 1, 0, 0}, "concurrent_modification"},
			{"forged-uid", 9, 3, [5]int32{1, 0, 1, 1, 0}, "not_found"},
			{"zero-uid", 9, 0, [5]int32{}, ""},
			{"fractional-uid", 9, 2.5, [5]int32{}, ""},
			{"overflow-uid", 9, 4294967296, [5]int32{}, ""},
			{"zero-validity", 0, 2, [5]int32{}, ""},
			{"string-uid", 9, "2", [5]int32{}, ""},
		} {
			t.Run(fmt.Sprintf("allowed-%v/%s", allowed, tc.name), func(t *testing.T) {
				b := newReplyScopeBackend(t)
				scope := "mail.read=INBOX"
				if allowed {
					scope = "mail.read=" + replyScopeMailbox
				}
				f := newReplyScopeProtocol(t, b, replyScopePlan(t, "mail.read,mail.send", false), scope)
				args := replyScopeArgs()
				args["uid_validity"], args["uid"] = tc.valid, tc.uid
				res := f.call(t, args)
				if !res.IsError {
					t.Error("invalid identity or synthetic SMTP boundary must reject")
				}
				want := tc.calls
				if !allowed {
					want = [5]int32{}
					assertReplyScopePrivate(t, res, f.audit.String())
					if tc.calls[0] != 0 {
						payload := mailToolsJSON(t, res)
						if len(payload) != 3 || payload["code"] != "validation" || payload["status"] != "rejected" || payload["message"] != "validation: mailbox is outside the configured resource scope" {
							t.Errorf("denied source existence affected the response: %s", mailToolsText(t, res))
						}
					}
				}
				got := [5]int32{b.reply.Load(), b.get.Load(), b.imap.Load(), b.metadata.Load(), b.smtp.Load()}
				t.Logf("backend_reply/get/imap/metadata/smtp=%v", got)
				if got != want {
					t.Errorf("backend calls = %v, want %v", got, want)
				}
				if allowed && tc.code != "" && mailToolsJSON(t, res)["code"] != tc.code {
					t.Errorf("identity error changed: %s", mailToolsText(t, res))
				}
			})
		}
	}
}
