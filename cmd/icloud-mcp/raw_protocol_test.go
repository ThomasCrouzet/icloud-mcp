package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// rawProtocolPeer drives the production stdio server with raw JSON-RPC
// frames. It deliberately uses no MCP SDK client so the negotiated era and
// wire shape are observed exactly as a third-party host sees them.
type rawProtocolPeer struct {
	t      *testing.T
	stdin  *io.PipeWriter
	lines  *bufio.Scanner
	nextID int
}

func newRawProtocolPeer(t *testing.T) *rawProtocolPeer {
	t.Helper()
	red := security.NewRedactor("synthetic-raw-protocol-secret")
	s := newMCPServer(red)
	mcptools.RegisterUnified(s, mcptools.Deps{
		Service:         &icloud.MockService{},
		Audit:           security.NewAuditLogger(io.Discard),
		Redactor:        red,
		DefaultLocation: time.UTC,
	}, mcptools.NewCapabilityPlan(true, false, false, false, false))

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	go func() {
		_ = serveBoundedStdio(s, stdinReader, stdoutWriter, log.New(io.Discard, "", 0), red)
		_ = stdoutWriter.Close()
	}()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
	})
	return &rawProtocolPeer{t: t, stdin: stdinWriter, lines: bufio.NewScanner(stdoutReader)}
}

func (p *rawProtocolPeer) request(method string, params map[string]any) map[string]any {
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
	return response
}

func modernMeta(version string) map[string]any {
	meta := map[string]any{
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "raw-probe", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	if version != "" {
		meta["io.modelcontextprotocol/protocolVersion"] = version
	}
	return meta
}

func responseResult(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if rpcErr, ok := response["error"]; ok {
		t.Fatalf("unexpected JSON-RPC error: %v", rpcErr)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", response)
	}
	return result
}

// TestRawModernProtocolWithoutInitialize covers server/discover, tools/list,
// and tools/call on protocol 2026-07-28 with no initialize handshake.
func TestRawModernProtocolWithoutInitialize(t *testing.T) {
	peer := newRawProtocolPeer(t)

	discover := responseResult(t, peer.request("server/discover", map[string]any{"_meta": modernMeta("2026-07-28")}))
	versions, ok := discover["supportedVersions"].([]any)
	if !ok || len(versions) == 0 {
		t.Fatalf("supportedVersions = %v", discover["supportedVersions"])
	}
	if versions[0] != "2026-07-28" {
		t.Fatalf("newest supported version = %v, want 2026-07-28", versions[0])
	}
	capabilities, ok := discover["capabilities"].(map[string]any)
	if !ok || capabilities["tools"] == nil {
		t.Fatalf("capabilities = %v", discover["capabilities"])
	}

	listed := responseResult(t, peer.request("tools/list", map[string]any{"_meta": modernMeta("2026-07-28")}))
	tools, ok := listed["tools"].([]any)
	if !ok || len(tools) != 7 {
		t.Fatalf("modern tools/list returned %v tools, want 7", len(tools))
	}

	called := responseResult(t, peer.request("tools/call", map[string]any{
		"name":      "icloud_capabilities",
		"arguments": map[string]any{},
		"_meta":     modernMeta("2026-07-28"),
	}))
	if called["isError"] == true {
		t.Fatalf("modern tools/call failed: %v", called)
	}
	content, ok := called["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("modern tools/call content = %v", called["content"])
	}
}

// TestRawModernProtocolRejectsUnknownVersion covers the normative error for
// a version the server does not implement.
func TestRawModernProtocolRejectsUnknownVersion(t *testing.T) {
	peer := newRawProtocolPeer(t)
	response := peer.request("tools/list", map[string]any{"_meta": modernMeta("2099-01-01")})
	rpcErr, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown version returned no error: %v", response)
	}
	code, _ := rpcErr["code"].(float64)
	if code != -32022 {
		t.Fatalf("error code = %v, want -32022", rpcErr["code"])
	}
	text, _ := json.Marshal(rpcErr)
	if !strings.Contains(string(text), "2099-01-01") {
		t.Fatalf("error does not name the requested version: %s", text)
	}
}

// TestRawLegacyInitializeStillWorks pins the older handshake path.
func TestRawLegacyInitializeStillWorks(t *testing.T) {
	peer := newRawProtocolPeer(t)
	init := responseResult(t, peer.request("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"clientInfo":      map[string]any{"name": "legacy-probe", "version": "1"},
		"capabilities":    map[string]any{},
	}))
	if init["protocolVersion"] != "2025-11-25" {
		t.Fatalf("negotiated version = %v", init["protocolVersion"])
	}
	if _, err := peer.stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	listed := responseResult(t, peer.request("tools/list", nil))
	tools, ok := listed["tools"].([]any)
	if !ok || len(tools) != 7 {
		t.Fatalf("legacy tools/list returned %v tools, want 7", len(tools))
	}
}
