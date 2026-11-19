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
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const foldWireHome = "/fold/calendars/"
const foldWireCalendar = foldWireHome + "home/"
const foldWireResource = foldWireCalendar + "paris-series.ics"
const foldWireETag = `"fold-v1"`

// foldWireFixture is a weekly Europe/Paris series that spans the 2026-10-25
// fall-back day. Its master start is early enough that a one-sided patch keeps
// a valid interval.
const foldWireFixture = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Fold wire fixture//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:paris-series\r\nSUMMARY:Paris series\r\n" +
	"DTSTART;TZID=Europe/Paris:20261025T000000\r\n" +
	"DTEND;TZID=Europe/Paris:20261025T230000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=6\r\n" +
	"DTSTAMP:20261001T000000Z\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// foldWireDAV serves discovery, REPORT, and GET for one named-TZID series and
// records PUT traffic. Every PUT body is decoded so the test can prove instant
// round-trip equality on the wire.
type foldWireDAV struct {
	t          *testing.T
	mu         sync.Mutex
	putBodies  []string
	putIfMatch []string
	deletes    int
	// stored is the body the server keeps after a successful PUT, so a later
	// GET returns exactly what was written.
	stored string
	etag   string
}

func (d *foldWireDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:"><response><href>/</href>`+
			`<propstat><prop><current-user-principal><href>`+redactionPrincipalPath+`</href></current-user-principal></prop>`+
			`<status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == redactionPrincipalPath:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+
			`<response><href>`+redactionPrincipalPath+`</href><propstat><prop><C:calendar-home-set><href>`+
			foldWireHome+`</href></C:calendar-home-set></prop><status>HTTP/1.1 200 OK</status></propstat>`+
			`</response></multistatus>`)
	case r.Method == "PROPFIND" && r.URL.Path == foldWireHome:
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+
			`<response><href>`+foldWireCalendar+`</href><propstat><prop>`+
			`<resourcetype><collection/><C:calendar/></resourcetype><displayname>Home</displayname>`+
			`<C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>`+
			`</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	case r.Method == "REPORT":
		_, _ = io.ReadAll(r.Body)
		var escaped strings.Builder
		if err := xml.EscapeText(&escaped, []byte(foldWireFixture)); err != nil {
			d.t.Fatal(err)
		}
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+
			`<response><href>`+foldWireResource+`</href><propstat><prop>`+
			`<getetag>`+foldWireETag+`</getetag><C:calendar-data>`+escaped.String()+`</C:calendar-data>`+
			`</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	case r.Method == http.MethodGet && r.URL.Path == foldWireResource:
		d.mu.Lock()
		body, etag := d.stored, d.etag
		d.mu.Unlock()
		if body == "" {
			body, etag = foldWireFixture, foldWireETag
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, body)
	case r.Method == http.MethodPut && r.URL.Path == foldWireResource:
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		current := d.etag
		if current == "" {
			current = foldWireETag
		}
		d.mu.Unlock()
		if r.Header.Get("If-Match") != current {
			// A stale precondition must never store the body.
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		d.mu.Lock()
		d.putBodies = append(d.putBodies, string(body))
		d.putIfMatch = append(d.putIfMatch, r.Header.Get("If-Match"))
		d.stored = string(body)
		d.etag = `"fold-v2"`
		d.mu.Unlock()
		w.Header().Set("ETag", `"fold-v2"`)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		d.mu.Lock()
		d.deletes++
		d.mu.Unlock()
		d.t.Errorf("unexpected DELETE %s", r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	default:
		d.t.Errorf("unexpected DAV request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *foldWireDAV) snapshot() ([]string, []string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.putBodies...), append([]string(nil), d.putIfMatch...), d.deletes
}

// newFoldWireProtocol starts JSON-RPC dispatch against the synthetic series.
func newFoldWireProtocol(t *testing.T) (*client.Client, *foldWireDAV) {
	t.Helper()
	dav := &foldWireDAV{t: t}
	srv := httptest.NewTLSServer(dav)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := srv.Client()
	httpClient.Timeout = 30 * time.Second
	service := icloud.NewClient(httpClient, srv.URL, func(host string) bool { return host == base.Hostname() })
	scopes := mustScopes(t, "calendar.read="+foldWireCalendar+";calendar.write="+foldWireCalendar)
	redactor := security.NewRedactor("synthetic-fold-secret")
	s := server.NewMCPServer("fold-wire", "test", server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(redactor)))
	RegisterUnified(s, Deps{Service: service, Redactor: redactor, DefaultLocation: time.UTC,
		Audit: security.NewAuditLogger(io.Discard), ResourceScopes: scopes},
		NewCapabilityPlan(false, false, false, false, false))
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
	init.Params.ClientInfo = mcp.Implementation{Name: "fold-wire", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c, dav
}

func callUpdateFold(t *testing.T, c *client.Client, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "update_event"
	req.Params.Arguments = args
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("update_event dispatch: %v", err)
	}
	return res
}

// TestProtocolUpdateSecondFoldNeverPuts is the #223 wire test. A second-fold
// instant returns validation with no PUT, and the first-fold instant stores a
// value that re-reads as the requested instant.
func TestProtocolUpdateSecondFoldNeverPuts(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-25T02:30:00+01:00 is the second Paris 02:30 (01:30Z).
	second := time.Date(2026, 10, 25, 2, 30, 0, 0, paris)
	if _, offset := second.Zone(); offset != 3600 {
		t.Fatalf("fixture offset = %d, want the second fold +01:00", offset)
	}
	first := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC).Add(-2 * time.Hour)

	c, dav := newFoldWireProtocol(t)
	res := callUpdateFold(t, c, map[string]any{
		"calendar": foldWireCalendar, "uid": "paris-series",
		"start": second.Format(time.RFC3339), "etag": foldWireETag,
	})
	if !res.IsError {
		t.Fatalf("second-fold update succeeded: %s", resultText(t, res))
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != string(icloud.CodeValidation) {
		t.Fatalf("code = %q, want validation: %s", payload.Code, resultText(t, res))
	}
	if !strings.Contains(payload.Message, "Europe/Paris") {
		t.Fatalf("message does not name the timezone: %s", payload.Message)
	}
	if puts, _, deletes := dav.snapshot(); len(puts) != 0 || deletes != 0 {
		t.Fatalf("rejected input reached the wire: puts=%d deletes=%d\n%s", len(puts), deletes, puts)
	}

	// The first-fold instant is representable and must round-trip.
	res = callUpdateFold(t, c, map[string]any{
		"calendar": foldWireCalendar, "uid": "paris-series",
		"start": first.Format(time.RFC3339), "etag": foldWireETag,
	})
	if res.IsError {
		t.Fatalf("first-fold update failed: %s", resultText(t, res))
	}
	puts, ifMatches, _ := dav.snapshot()
	if len(puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(puts))
	}
	if ifMatches[0] != foldWireETag {
		t.Fatalf("If-Match = %q, want the caller ETag", ifMatches[0])
	}
	cal, err := ical.NewDecoder(strings.NewReader(puts[0])).Decode()
	if err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	var master *ical.Component
	for _, child := range cal.Children {
		if child.Name == ical.CompEvent && child.Props.Get(ical.PropRecurrenceID) == nil {
			master = child
			break
		}
	}
	if master == nil {
		t.Fatalf("PUT has no master VEVENT:\n%s", puts[0])
	}
	stored := master.Props.Get(ical.PropDateTimeStart)
	if stored == nil {
		t.Fatalf("PUT lost DTSTART:\n%s", puts[0])
	}
	if tzid := stored.Params.Get(ical.PropTimezoneID); tzid != "Europe/Paris" {
		t.Fatalf("DTSTART TZID = %q, want Europe/Paris", tzid)
	}
	// Re-read with the project policy, which resolves a repeated wall time as
	// the first fold (RFC 5545 section 3.3.5).
	wall, err := time.ParseInLocation("20060102T150405", stored.Value, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	reRead, _ := icloud.ResolveWallClock(wall, paris)
	if !reRead.Equal(first) {
		t.Fatalf("stored value %q re-reads as %s, want %s", stored.Value,
			reRead.UTC().Format(time.RFC3339), first.UTC().Format(time.RFC3339))
	}
}

// TestProtocolUpdateFirstFoldKeepsSchedulingGuards proves the fold check does
// not weaken the ETag guard: a stale ETag still fails without a PUT.
func TestProtocolUpdateFirstFoldKeepsSchedulingGuards(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC).Add(-2 * time.Hour)
	c, dav := newFoldWireProtocol(t)
	res := callUpdateFold(t, c, map[string]any{
		"calendar": foldWireCalendar, "uid": "paris-series",
		"start": first.In(paris).Format(time.RFC3339), "etag": `"stale"`,
	})
	if !res.IsError {
		t.Fatalf("stale ETag update succeeded: %s", resultText(t, res))
	}
	puts, ifMatches, deletes := dav.snapshot()
	if len(puts) != 0 || deletes != 0 {
		t.Fatalf("stale ETag stored a mutation: puts=%d deletes=%d", len(puts), deletes)
	}
	for _, ifMatch := range ifMatches {
		if ifMatch != `"stale"` {
			t.Fatalf("If-Match = %q, want the caller ETag", ifMatch)
		}
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != string(icloud.CodeConcurrentModification) {
		t.Fatalf("code = %q, want concurrent_modification: %s", payload.Code, resultText(t, res))
	}
}
