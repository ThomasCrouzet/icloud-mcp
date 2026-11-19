package mcptools

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const deletePreflightHome = "/preflight/calendars/"
const deletePreflightCalendar = deletePreflightHome + "home/"
const deletePreflightResource = deletePreflightCalendar + "excluded-series.ics"
const deletePreflightETag = `"v2"`

// deletePreflightFixture is a weekly UTC series with one EXDATE and no
// scheduling metadata.
const deletePreflightFixture = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Preflight fixture//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:excluded-series\r\nSUMMARY:Standup\r\n" +
	"DTSTART:20260706T090000Z\r\nDTEND:20260706T091500Z\r\nDTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=5\r\nEXDATE:20260713T090000Z\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// deletePreflightDAV serves discovery, REPORT, and GET for one recurring
// object. It fails the test on any PUT or DELETE, so a dry run that mutates
// cannot pass.
type deletePreflightDAV struct {
	t         *testing.T
	mutations atomic.Int32
	mu        sync.Mutex
	reports   int
}

func (d *deletePreflightDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:"><response><href>/</href>`+
			`<propstat><prop><current-user-principal><href>`+redactionPrincipalPath+`</href></current-user-principal></prop>`+
			`<status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == redactionPrincipalPath:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+
			`<response><href>`+redactionPrincipalPath+`</href><propstat><prop><C:calendar-home-set><href>`+
			deletePreflightHome+`</href></C:calendar-home-set></prop><status>HTTP/1.1 200 OK</status></propstat>`+
			`</response></multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == deletePreflightHome:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+
			`<response><href>`+deletePreflightCalendar+`</href><propstat><prop>`+
			`<resourcetype><collection/><C:calendar/></resourcetype><displayname>Home</displayname>`+
			`<C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>`+
			`</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	case r.Method == "REPORT":
		_, _ = io.ReadAll(r.Body)
		d.mu.Lock()
		d.reports++
		d.mu.Unlock()
		var escaped strings.Builder
		if err := xml.EscapeText(&escaped, []byte(deletePreflightFixture)); err != nil {
			d.t.Fatal(err)
		}
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+
			`<response><href>`+deletePreflightResource+`</href><propstat><prop>`+
			`<getetag>`+deletePreflightETag+`</getetag><C:calendar-data>`+escaped.String()+`</C:calendar-data>`+
			`</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	case r.Method == http.MethodGet && r.URL.Path == deletePreflightResource:
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("ETag", deletePreflightETag)
		_, _ = io.WriteString(w, deletePreflightFixture)
	case r.Method == http.MethodPut, r.Method == http.MethodDelete:
		d.mutations.Add(1)
		d.t.Errorf("dry run reached a mutation: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusPreconditionFailed)
	default:
		d.t.Errorf("unexpected DAV request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// newDeletePreflightProtocol starts JSON-RPC dispatch over a synthetic TLS DAV
// server with one property-scoped calendar.
func newDeletePreflightProtocol(t *testing.T) (*client.Client, *deletePreflightDAV) {
	t.Helper()
	dav := &deletePreflightDAV{t: t}
	srv := httptest.NewTLSServer(dav)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := srv.Client()
	httpClient.Timeout = 30 * time.Second
	service := icloud.NewClient(httpClient, srv.URL, func(host string) bool { return host == base.Hostname() })
	scopes := mustScopes(t, "calendar.read="+deletePreflightCalendar+";calendar.delete="+deletePreflightCalendar)
	redactor := security.NewRedactor("synthetic-preflight-secret")
	s := server.NewMCPServer("delete-preflight", "test", server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(redactor)))
	RegisterUnified(s, Deps{Service: service, Redactor: redactor, DefaultLocation: time.UTC,
		Audit: security.NewAuditLogger(io.Discard), ResourceScopes: scopes}, NewCapabilityPlan(false, false, false, false, false))
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
	init.Params.ClientInfo = mcp.Implementation{Name: "delete-preflight", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c, dav
}

func callDeletePreflight(t *testing.T, c *client.Client, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "delete_event"
	req.Params.Arguments = args
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("delete_event dispatch: %v", err)
	}
	return res
}

// TestProtocolDeleteAlreadyExcludedDryRunEnforcesETag covers #220 through MCP
// dispatch: a stale caller ETag on an already-excluded occurrence returns
// concurrent_modification, and a matching ETag returns the no-op preview. No
// PUT or DELETE reaches the server in either case.
func TestProtocolDeleteAlreadyExcludedDryRunEnforcesETag(t *testing.T) {
	c, dav := newDeletePreflightProtocol(t)

	stale := callDeletePreflight(t, c, map[string]any{
		"calendar": deletePreflightCalendar, "uid": "excluded-series",
		"scope": "occurrence", "recurrence_id": "2026-07-13T09:00:00Z",
		"etag": `"v1"`, "dry_run": true,
	})
	if !stale.IsError {
		t.Fatalf("stale ETag dry run succeeded: %s", resultText(t, stale))
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, stale)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != string(icloud.CodeConcurrentModification) {
		t.Fatalf("stale ETag code = %q, want %q: %s", payload.Code, icloud.CodeConcurrentModification, resultText(t, stale))
	}

	matching := callDeletePreflight(t, c, map[string]any{
		"calendar": deletePreflightCalendar, "uid": "excluded-series",
		"scope": "occurrence", "recurrence_id": "2026-07-13T09:00:00Z",
		"etag": deletePreflightETag, "dry_run": true,
	})
	if matching.IsError {
		t.Fatalf("matching ETag dry run failed: %s", resultText(t, matching))
	}
	var preview deleteEventResponse
	if err := json.Unmarshal([]byte(resultText(t, matching)), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Success || !preview.DryRun || preview.WouldMutate || !preview.AlreadyExcluded {
		t.Fatalf("matching dry run = %+v, want dryRun+alreadyExcluded and wouldMutate=false", preview)
	}

	if got := dav.mutations.Load(); got != 0 {
		t.Fatalf("dry runs performed %d mutation(s)", got)
	}
}

// TestProtocolDeleteExcludedOccurrenceNoETagKeepsPreflight covers the
// documented point-in-time dry run without a caller ETag.
func TestProtocolDeleteExcludedOccurrenceNoETagKeepsPreflight(t *testing.T) {
	c, dav := newDeletePreflightProtocol(t)
	res := callDeletePreflight(t, c, map[string]any{
		"calendar": deletePreflightCalendar, "uid": "excluded-series",
		"scope": "occurrence", "recurrence_id": "2026-07-13T09:00:00Z", "dry_run": true,
	})
	if res.IsError {
		t.Fatalf("no-ETag dry run failed: %s", resultText(t, res))
	}
	var preview deleteEventResponse
	if err := json.Unmarshal([]byte(resultText(t, res)), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || preview.WouldMutate || !preview.AlreadyExcluded {
		t.Fatalf("no-ETag dry run = %+v", preview)
	}
	if got := dav.mutations.Load(); got != 0 {
		t.Fatalf("dry run performed %d mutation(s)", got)
	}
}
