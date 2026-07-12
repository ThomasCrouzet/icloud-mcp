package mcptools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// End-to-end redaction test (key security requirement): the password must
// not appear in ANY output, neither stderr nor the tools' JSON responses,
// even when the remote CalDAV server returns errors that echo the
// credentials in their body (real go-webdav behavior on a non-2xx
// text/plain response: the body is included in the returned error).

const (
	redactionPrincipalPath = "/121234567/principal/"
	redactionHomeSetPath   = "/121234567/calendars/"
	redactionCalendarPath  = redactionHomeSetPath + "home/"
)

// redactionTestServer serves the iCloud discovery normally, and can be
// configured to fail REPORT/PUT/DELETE with an error body containing the
// password and the raw Authorization header (simulating a buggy or hostile
// echoing server).
type redactionTestServer struct {
	password    string
	authFail401 bool
	reportFail  bool
	putFail     bool
	deleteFail  bool
}

func (h *redactionTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		if h.authFail401 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:">
  <response>
    <href>/</href>
    <propstat>
      <prop><current-user-principal><href>`+redactionPrincipalPath+`</href></current-user-principal></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == redactionPrincipalPath:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>`+redactionPrincipalPath+`</href>
    <propstat>
      <prop><C:calendar-home-set><href>`+redactionHomeSetPath+`</href></C:calendar-home-set></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == redactionHomeSetPath:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>`+redactionCalendarPath+`</href>
    <propstat>
      <prop>
        <resourcetype><collection/><C:calendar/></resourcetype>
        <displayname>Home</displayname>
        <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`)
	case r.Method == "REPORT":
		_, _ = io.ReadAll(r.Body)
		if h.reportFail {
			h.writeHostileError(w, r)
			return
		}
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:"></multistatus>`)
	case r.Method == http.MethodPut:
		_, _ = io.ReadAll(r.Body)
		if h.putFail {
			h.writeHostileError(w, r)
			return
		}
		w.Header().Set("ETag", `"etag-1"`)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		if h.deleteFail {
			h.writeHostileError(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// writeHostileError simulates a buggy CalDAV server that echoes the
// received credentials in its error response body, in THREE FORMS (raw
// password, raw Authorization header = base64(email:password), and a
// url-encoded form, e.g. a system echoing through a redirect query string).
// go-webdav (internal.Client.Do) includes this text/plain body in the Go
// error returned to the caller; that is the leak path this test exercises:
// all 3 forms must be redacted, not just the raw password.
func (h *redactionTestServer) writeHostileError(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = fmt.Fprintf(w, "internal error: received Authorization=%q, raw password=%q, url-encoded form=%q",
		r.Header.Get("Authorization"), h.password, url.QueryEscape(h.password))
}

func writeRedactionXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_, _ = io.WriteString(w, body)
}

// newTestRedactor mirrors exactly the redactor registration done in
// main.go: raw password, Basic auth base64 form, URL-encoded form.
func newTestRedactor(email, password string) *security.Redactor {
	return security.NewRedactor(
		password,
		base64.StdEncoding.EncodeToString([]byte(email+":"+password)),
		url.QueryEscape(password),
	)
}

// TestNoPasswordLeak_DiscoveryAuthFailure: this test is deliberately
// "vacuously true" as far as a real positive control goes. The simulated
// 401 response has an empty body, and above all: discovery.go NEVER
// includes the response body in its error for a 401; the message is a
// fixed constant (`c.propfind`, see discovery.go), read BEFORE any
// io.ReadAll of the body. No credential can therefore leak through THIS
// specific path, redaction or not: an honest positive control cannot be
// built here (a 401 body echoing the password would prove nothing, since
// our code never reads it). The REAL proof that redaction works on a
// response body echoing credentials is provided by
// TestNoPasswordLeak_HostileServerEchoesCredentials below
// (REPORT/PUT/DELETE via go-webdav, which DOES include the body in the
// returned error). This test remains useful to check that a 401 does not
// crash any tool and leaks nothing OTHER than the body (e.g. the error's
// stack/context).
func TestNoPasswordLeak_DiscoveryAuthFailure(t *testing.T) {
	const email = "user@example.com"
	const password = "SENTINEL-PW-abc123-XYZ" // gitleaks:allow, test sentinel, not a real secret

	h := &redactionTestServer{password: password, authFail401: true}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	authHTTP := webdav.HTTPClientWithBasicAuth(srv.Client(), email, password)
	ic := icloud.NewClient(authHTTP, srv.URL, func(string) bool { return true })
	svc := icloud.NewGuardedService(ic, 0, time.Millisecond)

	red := newTestRedactor(email, password)
	var stderrBuf bytes.Buffer
	stderr := security.NewRedactingWriter(&stderrBuf, red)
	audit := security.NewAuditLogger(stderr)

	deps := Deps{Service: svc, Audit: audit, Redactor: red}
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(false))
	Register(s, deps, false)

	combined := callAllTools(t, s, []toolCall{
		{"list_calendars", nil},
		{"search_events", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z"}},
		{"create_event", map[string]any{"title": "x", "start": "2026-07-01T00:00:00Z", "end": "2026-07-01T01:00:00Z", "calendar": redactionCalendarPath}},
		{"update_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath, "title": "y"}},
		{"delete_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath}},
	}, true /* all must fail (401) */)

	assertNoLeak(t, email, password, combined, stderrBuf.String())
}

func TestNoPasswordLeak_HostileServerEchoesCredentials(t *testing.T) {
	const email = "user@example.com"
	const password = "SENTINEL-PW-abc123-XYZ" // gitleaks:allow, test sentinel, not a real secret

	h := &redactionTestServer{password: password, reportFail: true, putFail: true, deleteFail: true}
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	authHTTP := webdav.HTTPClientWithBasicAuth(srv.Client(), email, password)
	ic := icloud.NewClient(authHTTP, srv.URL, func(string) bool { return true })

	// Positive control: without redaction, the raw error MUST contain the
	// 3 forms of the secret (raw, Basic Auth base64, url-encoded). We go
	// through CreateEvent (PUT via go-webdav, which includes the hostile
	// response body in its error, see writeHostileError).
	// NB: SearchEvents is no longer suitable as a positive control: its
	// request is a manual REPORT (reportCalendarQuery) that never includes
	// the response body in its error (only a generic unexpected-HTTP-status
	// message). Redaction of the go-webdav paths (create/update/delete) is
	// still covered by the combined assertion below.
	rawStart := time.Now()
	_, rawErr := ic.CreateEvent(context.Background(), redactionCalendarPath, &icloud.NewEvent{
		Title:     "x",
		StartTime: rawStart,
		EndTime:   rawStart.Add(time.Hour),
	})
	if rawErr == nil {
		t.Fatalf("positive control failed: expected a non-nil raw error")
	}
	rawBasicAuth := base64.StdEncoding.EncodeToString([]byte(email + ":" + password))
	rawURLEncoded := url.QueryEscape(password)
	for _, want := range []string{password, rawBasicAuth, rawURLEncoded} {
		if !strings.Contains(rawErr.Error(), want) {
			t.Fatalf("positive control failed: the raw (unredacted) error should contain %q, got: %v", want, rawErr)
		}
	}

	svc := icloud.NewGuardedService(ic, 0, time.Millisecond)
	red := newTestRedactor(email, password)
	var stderrBuf bytes.Buffer
	stderr := security.NewRedactingWriter(&stderrBuf, red)
	audit := security.NewAuditLogger(stderr)

	deps := Deps{Service: svc, Audit: audit, Redactor: red}
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(false))
	Register(s, deps, false)

	combined := callAllTools(t, s, []toolCall{
		{"list_calendars", nil}, // PROPFIND only, expected to succeed
		{"search_events", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": redactionCalendarPath}},
		{"create_event", map[string]any{"title": "x", "start": "2026-07-01T00:00:00Z", "end": "2026-07-01T01:00:00Z", "calendar": redactionCalendarPath}},
		{"update_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath, "title": "y"}},
		{"delete_event", map[string]any{"uid": "uid-1", "calendar": redactionCalendarPath}},
	}, false /* only list_calendars must succeed, the rest fail */)

	assertNoLeak(t, email, password, combined, stderrBuf.String())

	// A SUCCESSFUL create_event (no simulated failure) must not expose the
	// password in its success response either.
	h2 := &redactionTestServer{password: password}
	srv2 := httptest.NewTLSServer(h2)
	defer srv2.Close()
	authHTTP2 := webdav.HTTPClientWithBasicAuth(srv2.Client(), email, password)
	ic2 := icloud.NewClient(authHTTP2, srv2.URL, func(string) bool { return true })
	svc2 := icloud.NewGuardedService(ic2, 0, time.Millisecond)
	deps2 := Deps{Service: svc2, Audit: audit, Redactor: red}
	s2 := server.NewMCPServer("test2", "0.0.0", server.WithToolCapabilities(false))
	Register(s2, deps2, false)

	successCombined := callAllTools(t, s2, []toolCall{
		{"create_event", map[string]any{"title": "x", "start": "2026-07-01T00:00:00Z", "end": "2026-07-01T01:00:00Z", "calendar": redactionCalendarPath}},
	}, false)
	if !strings.Contains(successCombined, `"success": true`) {
		t.Errorf("the control create_event should have succeeded: %s", successCombined)
	}
	assertNoLeak(t, email, password, successCombined, stderrBuf.String())
}

type toolCall struct {
	name string
	args map[string]any
}

// callAllTools spins up an in-process MCP client against s, calls each tool
// in calls, and returns the concatenation of all textual contents of the
// responses (success AND error). allMustFail additionally asserts that
// every call fails (used for the 401 scenario).
func callAllTools(t *testing.T, s *server.MCPServer, calls []toolCall, allMustFail bool) string {
	t.Helper()
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "redaction-test", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	var combined strings.Builder
	for _, call := range calls {
		req := mcp.CallToolRequest{}
		req.Params.Name = call.name
		req.Params.Arguments = call.args
		res, err := c.CallTool(ctx, req)
		if err != nil {
			t.Fatalf("%s: unexpected protocol error: %v", call.name, err)
		}
		if allMustFail && !res.IsError {
			t.Errorf("%s: unexpected success (should fail)", call.name)
		}
		for _, content := range res.Content {
			if tc, ok := mcp.AsTextContent(content); ok {
				combined.WriteString(tc.Text)
				combined.WriteString("\n")
			}
		}
	}
	return combined.String()
}

// assertNoLeak checks that none of the 3 forms of the secret (raw password,
// base64(email:password) as echoed by a raw Authorization header, and
// url.QueryEscape(password)) appear in toolResults NOR in capturedStderr.
func assertNoLeak(t *testing.T, email, password, toolResults, capturedStderr string) {
	t.Helper()
	forms := map[string]string{
		"raw password":                           password,
		"base64 password (Basic Authorization)":  base64.StdEncoding.EncodeToString([]byte(email + ":" + password)),
		"url-encoded password (url.QueryEscape)": url.QueryEscape(password),
	}
	for label, form := range forms {
		if strings.Contains(toolResults, form) {
			t.Fatalf("%s appears in a tool response:\n%s", label, toolResults)
		}
		if strings.Contains(capturedStderr, form) {
			t.Fatalf("%s appears in stderr:\n%s", label, capturedStderr)
		}
	}
}

// TestRecoverRedactMiddleware_PanicDoesNotLeakSecret. The JSON-RPC error
// channel (triggered by a panic inside a tool handler) bypasses the stderr
// RedactingWriter: that writer only covers stderr, not the serialization of
// protocol errors on stdout. server.WithRecovery() does convert a panic
// into a Go error, but that error is then serialized AS IS (err.Error())
// into the JSON-RPC message, without going through redaction. This test
// registers a dummy tool that panics while carrying the password, and
// checks:
//   - positive control: WITHOUT RecoverRedactMiddleware (WithRecovery
//     alone, the vulnerable configuration), the password does leak into the
//     protocol error; otherwise this test would prove nothing;
//   - WITH RecoverRedactMiddleware, the password NEVER leaks: the panic is
//     absorbed and turned into a redacted error CallToolResult.
func TestRecoverRedactMiddleware_PanicDoesNotLeakSecret(t *testing.T) {
	const email = "user@example.com"
	const password = "SENTINEL-PW-panic-abc123-XYZ" // gitleaks:allow, test sentinel, not a real secret
	red := newTestRedactor(email, password)

	registerBoom := func(s *server.MCPServer) {
		s.AddTool(mcp.NewTool("boom"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic(fmt.Sprintf("internal error: received Authorization=%q, raw password=%q", "Basic xxx", password))
		})
	}

	callBoom := func(t *testing.T, s *server.MCPServer) (result string, callErr error) {
		t.Helper()
		c, err := client.NewInProcessClient(s)
		if err != nil {
			t.Fatalf("NewInProcessClient: %v", err)
		}
		defer func() { _ = c.Close() }()
		ctx := context.Background()
		if err := c.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "panic-test", Version: "0.0.0"}
		if _, err := c.Initialize(ctx, initReq); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		req := mcp.CallToolRequest{}
		req.Params.Name = "boom"
		res, err := c.CallTool(ctx, req)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, content := range res.Content {
			if tc, ok := mcp.AsTextContent(content); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), nil
	}

	t.Run("positive control: without RecoverRedactMiddleware, the panic leaks", func(t *testing.T) {
		vulnerable := server.NewMCPServer("vulnerable", "0.0.0",
			server.WithToolCapabilities(false),
			server.WithRecovery(),
		)
		registerBoom(vulnerable)
		text, callErr := callBoom(t, vulnerable)
		if callErr == nil {
			t.Fatalf("expected: protocol error (panic not absorbed into a CallToolResult), got result: %s", text)
		}
		if !strings.Contains(callErr.Error(), password) {
			t.Fatalf("positive control failed: the unprotected protocol error should contain the sentinel password, got: %v", callErr)
		}
	})

	t.Run("with RecoverRedactMiddleware, no leak", func(t *testing.T) {
		protected := server.NewMCPServer("protected", "0.0.0",
			server.WithToolCapabilities(false),
			server.WithRecovery(),
			server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)),
		)
		registerBoom(protected)
		text, callErr := callBoom(t, protected)
		if callErr != nil {
			t.Fatalf("unexpected protocol error (the panic should have been absorbed into a CallToolResult): %v", callErr)
		}
		if strings.Contains(text, password) {
			t.Fatalf("password appears in the tool response after panic:\n%s", text)
		}
	})
}
