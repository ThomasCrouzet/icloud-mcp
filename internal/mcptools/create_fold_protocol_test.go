package mcptools

import (
	"context"
	"encoding/json"
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

// TestCreateEventHandlerRejectsSecondFoldEnd reproduces the reviewer case:
// timezone=Europe/Paris, start on the first fold, end on the second fold. The
// stored DTEND would re-read one hour earlier, so the call must fail with a
// typed validation error before create reaches the service.
func TestCreateEventHandlerRejectsSecondFoldEnd(t *testing.T) {
	for _, test := range []struct {
		name      string
		start     string
		end       string
		timezone  string
		wantError bool
	}{{
		name: "second fold end", start: "2026-10-25T02:30:00+02:00", end: "2026-10-25T02:30:00+01:00",
		timezone: "Europe/Paris", wantError: true,
	}, {
		name: "unambiguous end after the fold", start: "2026-10-25T02:30:00+02:00", end: "2026-10-25T04:30:00+02:00",
		timezone: "Europe/Paris",
	}, {
		name: "normal hour", start: "2026-10-26T09:00:00+01:00", end: "2026-10-26T10:00:00+01:00",
		timezone: "Europe/Paris",
	}, {
		name: "UTC event", start: "2026-10-25T01:30:00Z", end: "2026-10-25T02:30:00Z",
	}} {
		t.Run(test.name, func(t *testing.T) {
			svc := &icloud.MockService{CreatedUID: "fold-create-uid"}
			handler := createEventHandler(testDeps(svc))
			res, err := handler(t.Context(), newReq(map[string]any{
				"title": "Fold create", "start": test.start, "end": test.end,
				"calendar": "/cal/home/", "timezone": test.timezone,
			}))
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			if test.wantError {
				if !res.IsError {
					t.Fatalf("second-fold end was accepted: %s", resultText(t, res))
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
				if svc.CreateCallCount != 0 {
					t.Fatalf("rejected input reached CreateEvent %d time(s)", svc.CreateCallCount)
				}
				return
			}
			if res.IsError {
				t.Fatalf("valid create failed: %s", resultText(t, res))
			}
			if svc.CreateCallCount != 1 {
				t.Fatalf("CreateEvent calls = %d, want 1", svc.CreateCallCount)
			}
		})
	}
}

// TestProtocolCreateSecondFoldEndNeverPuts proves the reviewer case against a
// synthetic DAV server: validation, no PUT, with If-None-Match create guard
// intact for the accepted case.
func TestProtocolCreateSecondFoldEndNeverPuts(t *testing.T) {
	dav := &createFoldDAV{t: t}
	c := newCreateFoldProtocol(t, dav)

	res := callCreateFold(t, c, map[string]any{
		"title": "Fold create", "start": "2026-10-25T02:30:00+02:00",
		"end": "2026-10-25T02:30:00+01:00", "calendar": foldWireCalendar,
		"timezone": "Europe/Paris", "client_uid": "fold-create",
	})
	if !res.IsError {
		t.Fatalf("second-fold end was accepted: %s", resultText(t, res))
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != string(icloud.CodeValidation) {
		t.Fatalf("code = %q, want validation: %s", payload.Code, resultText(t, res))
	}
	if puts, _, _ := dav.snapshot(); len(puts) != 0 {
		t.Fatalf("rejected create reached the wire: %s", puts[0])
	}

	// The same call with an unambiguous end must store values that re-read
	// exactly, and must still send If-None-Match: *.
	res = callCreateFold(t, c, map[string]any{
		"title": "Fold create", "start": "2026-10-25T02:30:00+02:00",
		"end": "2026-10-25T04:30:00+02:00", "calendar": foldWireCalendar,
		"timezone": "Europe/Paris", "client_uid": "fold-create",
	})
	if res.IsError {
		t.Fatalf("first-fold create failed: %s", resultText(t, res))
	}
	puts, ifNoneMatch, _ := dav.snapshot()
	if len(puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(puts))
	}
	if ifNoneMatch[0] != "*" {
		t.Fatalf("If-None-Match = %q, want *", ifNoneMatch[0])
	}
	assertStoredCreateInstants(t, puts[0], map[string]time.Time{
		"DTSTART": time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC).Add(-2 * time.Hour),
		"DTEND":   time.Date(2026, 10, 25, 4, 30, 0, 0, time.UTC).Add(-2 * time.Hour),
	})
}

// createFoldDAV serves discovery and one calendar collection, records PUT
// traffic, and fails the test on any unexpected method.
type createFoldDAV struct {
	t       *testing.T
	mu      sync.Mutex
	puts    []string
	ifNone  []string
	objects map[string]string
}

func (d *createFoldDAV) snapshot() ([]string, []string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.puts...), append([]string(nil), d.ifNone...), len(d.objects)
}

func (d *createFoldDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		// No object exists yet, so the client-UID reconciliation lookup is empty.
		writeRedactionXML(w, `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"></multistatus>`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, foldWireCalendar):
		// The client-UID reconcile path probes the canonical href first.
		d.mu.Lock()
		body, ok := d.objects[r.URL.Path]
		d.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("ETag", `"created-1"`)
		_, _ = io.WriteString(w, body)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, foldWireCalendar):
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.puts = append(d.puts, string(body))
		d.ifNone = append(d.ifNone, r.Header.Get("If-None-Match"))
		if d.objects == nil {
			d.objects = map[string]string{}
		}
		d.objects[r.URL.Path] = string(body)
		d.mu.Unlock()
		w.Header().Set("ETag", `"created-1"`)
		w.WriteHeader(http.StatusCreated)
	default:
		d.t.Errorf("unexpected DAV request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func newCreateFoldProtocol(t *testing.T, dav *createFoldDAV) *client.Client {
	t.Helper()
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
	redactor := security.NewRedactor("synthetic-create-fold-secret")
	s := server.NewMCPServer("create-fold", "test", server.WithToolCapabilities(false),
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
	init.Params.ClientInfo = mcp.Implementation{Name: "create-fold", Version: "test"}
	if _, err := c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c
}

func callCreateFold(t *testing.T, c *client.Client, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "create_event"
	req.Params.Arguments = args
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("create_event dispatch: %v", err)
	}
	return res
}

// assertStoredCreateInstants decodes a PUT body and requires that each named
// property re-reads as the expected instant under the first-fold policy.
func assertStoredCreateInstants(t *testing.T, body string, want map[string]time.Time) {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(body)).Decode()
	if err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	var event *ical.Component
	for _, child := range cal.Children {
		if child.Name == ical.CompEvent {
			event = child
			break
		}
	}
	if event == nil {
		t.Fatalf("PUT has no VEVENT:\n%s", body)
	}
	for name, instant := range want {
		prop := event.Props.Get(name)
		if prop == nil {
			t.Fatalf("PUT lost %s:\n%s", name, body)
		}
		tzid := prop.Params.Get(ical.PropTimezoneID)
		if tzid != "Europe/Paris" {
			t.Fatalf("%s TZID = %q, want Europe/Paris", name, tzid)
		}
		wall, err := time.ParseInLocation("20060102T150405", prop.Value, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		loc, err := time.LoadLocation(tzid)
		if err != nil {
			t.Fatal(err)
		}
		reRead, _ := icloud.ResolveWallClock(wall, loc)
		if !reRead.Equal(instant) {
			t.Fatalf("%s %q re-reads as %s, want %s", name, prop.Value,
				reRead.UTC().Format(time.RFC3339), instant.UTC().Format(time.RFC3339))
		}
	}
}
