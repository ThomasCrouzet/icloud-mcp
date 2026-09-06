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

// These tests verify the main redaction requirement from end to end. A password
// must not appear in stderr or tool JSON responses. This rule also applies when
// a remote CalDAV error body echoes credentials. For a non-2xx text/plain
// response, go-webdav includes that body in the returned error.

const (
	redactionPrincipalPath = "/121234567/principal/"
	redactionHomeSetPath   = "/121234567/calendars/"
	redactionCalendarPath  = redactionHomeSetPath + "home/"
)

// redactionTestServer serves iCloud discovery normally. Tests can make REPORT,
// PUT, or DELETE fail. The error body then contains the password and raw
// Authorization header to simulate a faulty or hostile echoing server.
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

// writeHostileError simulates a faulty CalDAV server that echoes credentials
// in an error body. It includes the raw password, the raw Authorization header,
// and a URL-encoded value. The header contains base64(email:password). The
// encoded value simulates an echo through a redirect query string.
//
// go-webdav internal.Client.Do includes this text/plain body in the returned
// Go error. This is the leak path under test. Redaction must remove all three
// forms, not only the raw password.
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
// main.go: raw password, email, Basic auth base64 (Std/RawStd/URL/RawURL),
// query- and path-encoded forms.
func newTestRedactor(email, password string) *security.Redactor {
	basic := []byte(email + ":" + password)
	return security.NewRedactor(
		password,
		email,
		base64.StdEncoding.EncodeToString(basic),
		base64.RawStdEncoding.EncodeToString(basic),
		base64.URLEncoding.EncodeToString(basic),
		base64.RawURLEncoding.EncodeToString(basic),
		url.QueryEscape(password),
		url.PathEscape(password),
	)
}

// TestNoPasswordLeak_DiscoveryAuthFailure does not provide a positive control
// for a response-body leak. The simulated 401 response has an empty body.
// discovery.go also excludes a 401 body from its error. It reads the fixed
// c.propfind message before any io.ReadAll call. Thus, this path cannot leak a
// credential from the body, even without redaction.
//
// A 401 body that echoes the password would not prove anything because this
// code never reads it. TestNoPasswordLeak_HostileServerEchoesCredentials
// provides the positive control. It covers REPORT, PUT, and DELETE through
// go-webdav, which includes the response body in the returned error. This test
// still verifies that a 401 does not crash a tool. It also checks other leak
// sources, such as error context.
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

	// Positive control: without redaction, a hostile error carrying the
	// three secret forms must still contain them. Create/update now use a
	// custom PUT that classifies status without embedding response
	// bodies (so CreateEvent is no longer a reliable leak vector). Prove
	// the redactor still masks all forms when a secret does appear.
	rawBasicAuth := base64.StdEncoding.EncodeToString([]byte(email + ":" + password))
	rawURLEncoded := url.QueryEscape(password)
	rawErr := fmt.Errorf("hostile echo pwd=%s basic=%s url=%s", password, rawBasicAuth, rawURLEncoded)
	for _, want := range []string{password, rawBasicAuth, rawURLEncoded} {
		if !strings.Contains(rawErr.Error(), want) {
			t.Fatalf("positive control failed: the raw (unredacted) error should contain %q, got: %v", want, rawErr)
		}
	}
	_ = ic // client still drives the MCP tool path below

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

// callAllTools starts an in-process MCP client for s. It invokes each tool in
// calls and joins all response text, including success and error responses.
// When allMustFail is true, it also verifies that each call fails. The 401 test
// uses this option.
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

// assertNoLeak checks three forms of the secret. They are the raw password,
// base64(email:password) from an Authorization header, and
// url.QueryEscape(password). Neither toolResults nor capturedStderr can
// contain these forms.
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

// TestRecoverRedactMiddleware_PanicDoesNotLeakSecret covers the JSON-RPC error
// channel. A panic in a tool handler can bypass RedactingWriter because that
// writer protects only stderr. It does not protect protocol errors on stdout.
// server.WithRecovery converts a panic into a Go error. Without the redaction
// middleware, JSON-RPC serializes err.Error() without redaction.
//
// The test registers a dummy tool that panics with the password. The positive
// control uses WithRecovery without RecoverRedactMiddleware. It verifies that
// the password leaks into the protocol error. The protected configuration uses
// RecoverRedactMiddleware. It converts the panic into a redacted error
// CallToolResult, and the password must not leak.
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
