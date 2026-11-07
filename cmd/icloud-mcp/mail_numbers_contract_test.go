package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
	transport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type contractNumericMail struct {
	maildomain.Service
	mu     sync.Mutex
	values []uint64
}

func (s *contractNumericMail) SetMessageFlags(_ context.Context, in maildomain.SetFlagsInput) (maildomain.SetFlagsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, in.ExpectedModSeq)
	return maildomain.SetFlagsResult{Mailbox: in.Mailbox, UIDValidity: in.UIDValidity, UID: in.UID, Flags: in.Flags}, nil
}

func (s *contractNumericMail) snapshot() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64{}, s.values...)
}

func contractNumericTransport(t *testing.T, s *server.MCPServer, mode string) func(string, int, string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	if mode == "inprocess" {
		tr := transport.NewInProcessTransport(s)
		if err := tr.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		return func(method string, id int, params string) json.RawMessage {
			res, err := tr.SendRequest(ctx, transport.JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(id), Method: method, Params: json.RawMessage(params)})
			if err != nil || res.Error != nil {
				t.Fatalf("transport: %v %+v", err, res)
			}
			return res.Result
		}
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := serveBoundedStdio(s, inR, outW, log.New(io.Discard, "", 0), security.NewRedactor("synthetic-numeric-secret"))
		_ = outW.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outR.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("stdio did not stop")
		}
	})
	// Close both pipes at the deadline so a broken transport cannot stall the suite.
	stop := context.AfterFunc(ctx, func() { _ = inW.Close(); _ = outR.Close() })
	t.Cleanup(func() { stop() })
	reader := bufio.NewReader(outR)
	return func(method string, id int, params string) json.RawMessage {
		if _, err := fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", id, method, params); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil || len(response.Error) > 0 {
			t.Fatalf("stdio response: %s %v", line, err)
		}
		return response.Result
	}
}

func TestContractMailNumbersTransports(t *testing.T) {
	for _, mode := range []string{"inprocess", "bounded-stdio"} {
		t.Run(mode, func(t *testing.T) {
			red := security.NewRedactor("synthetic-numeric-secret")
			s := newMCPServer(red)
			backend := &contractNumericMail{}
			mcptools.RegisterMail(s, mcptools.MailDeps{Service: backend, Redactor: red, Audit: security.NewAuditLogger(io.Discard)}, false, true, false, false, false)
			request := contractNumericTransport(t, s, mode)
			_ = request("initialize", 1, `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"numeric-contract","version":"test"}}`)
			beforeSchema := request("tools/list", 2, `{}`)
			var listed struct {
				Tools []struct {
					Name   string          `json:"name"`
					Schema json.RawMessage `json:"inputSchema"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(beforeSchema, &listed); err != nil {
				t.Fatal(err)
			}
			var input json.RawMessage
			for _, tool := range listed.Tools {
				if tool.Name == "set_message_flags" {
					input = tool.Schema
				}
			}
			// Hash from the independently preserved accepted-main raw schema.
			if hash := fmt.Sprintf("%x", sha256.Sum256(input)); hash != "254b39f46c7865dba72516cbfb61bf05cf4500c10ceab0bf440d51334554659a" {
				t.Fatalf("published schema changed: %s %s", hash, input)
			}
			for i, tc := range []struct{ name, token, want string }{
				{"exact-maximum", "18446744073709551615", "18446744073709551615"},
				{"nearby", "18446744073709551614", "18446744073709551614"},
				{"binary64-nearby", "18446744073709549568", "18446744073709549568"},
				{"integral-decimal", "1.0", "1"}, {"integral-exponent", "1e0", "1"}, {"negative-exponent-integral", "10e-1", "1"},
				{"maximum-decimal", "18446744073709551615.0", "18446744073709551615"},
				{"maximum-exponent", "184467440737095516150e-1", "18446744073709551615"},
				{"above-binary64", "9007199254740993", "9007199254740993"},
				{"overflow", "18446744073709551616", ""}, {"decimal-overflow", "18446744073709551616.0", ""},
				{"fraction", "1.5", ""}, {"hidden-fraction", "1.0000000000000000001", ""},
				{"large-fraction", "18446744073709551615.5", ""}, {"negative", "-1", ""}, {"zero", "0", ""},
				{"huge-negative-exponent", "1e-1000000000", ""}, {"positive-exponent-overflow", "1e300", ""},
				{"wrong-string", `"18446744073709551615"`, ""}, {"wrong-bool", "true", ""}, {"wrong-null", "null", ""}, {"wrong-array", "[]", ""}, {"wrong-object", "{}", ""},
				{"unknown-key", "18446744073709551615", ""}, {"missing-optional", "MISSING", "0"}, {"missing-arguments", "NO_ARGUMENTS", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := `{"mailbox":"INBOX","uid_validity":1,"uid":2,"operation":"add","flags":["seen"]`
					if tc.token != "MISSING" {
						args += `,"expected_modseq":` + tc.token
					}
					if tc.name == "unknown-key" {
						args += `,"unexpected":1`
					}
					args += "}"
					params := `{"name":"set_message_flags","arguments":` + args + `}`
					if tc.token == "NO_ARGUMENTS" {
						params = `{"name":"set_message_flags"}`
					}
					before := len(backend.snapshot())
					response := request("tools/call", 10+i, params)
					var result mcp.CallToolResult
					// A JSON number outside binary64 can be refused by the SDK decoder itself.
					if err := json.Unmarshal(response, &result); err != nil {
						t.Fatal(err)
					}
					values := backend.snapshot()
					calls := len(values) - before
					valid := tc.want != ""
					t.Logf("numeric_transport=%s case=%s token=%s calls=%d result=%s", mode, tc.name, tc.token, calls, response)
					if result.IsError == valid || calls != map[bool]int{true: 1, false: 0}[valid] {
						t.Errorf("accepted=%v calls=%d want=%v", !result.IsError, calls, valid)
					}
					if valid && calls == 1 && strconv.FormatUint(values[len(values)-1], 10) != tc.want {
						t.Errorf("exact backend value %d != %s", values[len(values)-1], tc.want)
					}
				})
			}
			if after := request("tools/list", 100, `{}`); string(after) != string(beforeSchema) {
				t.Fatal("calls changed schemas")
			}
		})
	}
}

func TestContractMailNumericUIDSpellings(t *testing.T) {
	red := security.NewRedactor("synthetic-numeric-secret")
	s := newMCPServer(red)
	backend := &contractNumericMail{}
	mcptools.RegisterMail(s, mcptools.MailDeps{Service: backend, Redactor: red, Audit: security.NewAuditLogger(io.Discard)}, false, true, false, false, false)
	request := contractNumericTransport(t, s, "inprocess")
	_ = request("initialize", 1, `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"numeric-uid","version":"test"}}`)
	for i, token := range []string{"1.0", "1e0", "10e-1"} {
		response := request("tools/call", i+2, `{"name":"set_message_flags","arguments":{"mailbox":"INBOX","uid_validity":`+token+`,"uid":`+token+`,"operation":"add","flags":["seen"]}}`)
		if strings.Contains(string(response), `"isError":true`) {
			t.Errorf("integral UID spelling rejected: %s", response)
		}
	}
	if len(backend.snapshot()) != 3 {
		t.Fatal("integral UID spellings never reached service")
	}
}
