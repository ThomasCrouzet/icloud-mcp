package mcptools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Keep this slice compilable at the original #90 baseline. The SMTP dial is a
// counted refusal boundary, not successful SMTP delivery or archival proof.
func TestContractArchiveReplay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		wantIMAP int32
	}{
		{"absent", map[string]any{}, 0},
		{"false", map[string]any{"archive_sent": false}, 0},
		{"true", map[string]any{"archive_sent": true}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var imap, smtp atomic.Int32
			policy, err := maildomain.ParseRecipientPolicy("recipient@example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			svc, err := maildomain.NewService(maildomain.Config{
				Address: "fixture@icloud.com", Password: "synthetic-contract-secret", RecipientPolicy: policy,
				EnableSentCopy: true, ArchiveScope: []string{"Sent"},
			}, func(context.Context) (net.Conn, error) {
				imap.Add(1)
				local, remote := net.Pipe()
				go contractArchiveIMAP(remote)
				return local, nil
			}, func(context.Context) (net.Conn, error) {
				smtp.Add(1)
				return nil, errors.New("synthetic SMTP dial refusal")
			}, false, true)
			if err != nil {
				t.Fatal(err)
			}
			c := contractReplayClient(t, svc)
			listed, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var property map[string]any
			for _, tool := range listed.Tools {
				if tool.Name == "send_message" {
					encoded, err := json.Marshal(tool)
					if err != nil {
						t.Fatal(err)
					}
					var wire struct {
						Input struct {
							Properties map[string]map[string]any `json:"properties"`
						} `json:"inputSchema"`
					}
					if err := json.Unmarshal(encoded, &wire); err != nil {
						t.Fatal(err)
					}
					property = wire.Input.Properties["archive_sent"]
				}
			}
			if property["type"] != "boolean" || property["default"] != false {
				t.Fatalf("actual tools/list archive_sent contract: %v", property)
			}
			args := map[string]any{"to": []string{"recipient@example.invalid"}, "body": "Synthetic contract body"}
			for key, value := range tc.args {
				args[key] = value
			}
			req := mcp.CallToolRequest{}
			req.Params.Name, req.Params.Arguments = "send_message", args
			res, err := c.CallTool(t.Context(), req)
			t.Logf("archive_case=%s result=%s error=%v imap_dials=%d smtp_dials=%d", tc.name, contractReplayJSON(t, res), err, imap.Load(), smtp.Load())
			if err != nil || res == nil || !res.IsError || imap.Load() != tc.wantIMAP || smtp.Load() != 1 {
				t.Fatalf("advertised archive_sent did not reach the real service SMTP refusal boundary")
			}
			if strings.Contains(contractReplayJSON(t, res), "unknown argument") {
				t.Fatal("handler rejected a published argument")
			}
		})
	}
}

func contractReplayClient(t *testing.T, svc maildomain.Service) *client.Client {
	t.Helper()
	red := security.NewRedactor("synthetic-contract-secret")
	s := server.NewMCPServer("contract-replay", "test", server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(), server.WithRecovery(),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	RegisterUnified(s, Deps{Service: &icloud.MockService{}, MailService: svc, Redactor: red,
		Audit: security.NewAuditLogger(io.Discard)}, NewCapabilityPlan(false, false, true, false, true))
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
	init.Params.ClientInfo = mcp.Implementation{Name: "contract-replay", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c
}

func contractReplayJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func contractArchiveIMAP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] fixture\r\n"); err != nil {
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
		var response string
		switch fields[1] {
		case "LOGIN":
			response = fields[0] + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
		case "LIST":
			response = "* LIST (\\Sent) \"/\" \"Sent\"\r\n" + fields[0] + " OK listed\r\n"
		case "LOGOUT":
			_, _ = fmt.Fprint(conn, "* BYE closing\r\n"+fields[0]+" OK logout\r\n")
			return
		default:
			response = fields[0] + " BAD unexpected fixture command\r\n"
		}
		if _, err := fmt.Fprint(conn, response); err != nil {
			return
		}
	}
}
