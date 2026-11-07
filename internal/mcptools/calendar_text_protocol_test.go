package mcptools

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const calendarTextPath = "/text/calendars/home/"
const calendarTextETag = `"text-fixture"`

func calendarTextObject(properties, override string) string {
	return strings.ReplaceAll("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Text fixture//EN\n"+
		"BEGIN:VEVENT\nUID:text-event\nDTSTAMP:20261107T000000Z\n"+
		"DTSTART:20261107T100000Z\nDTEND:20261107T110000Z\n"+
		properties+"END:VEVENT\n"+override+"END:VCALENDAR\n", "\n", "\r\n")
}

type calendarTextProtocol struct {
	client  *client.Client
	deletes atomic.Int32
	audit   bytes.Buffer
}

// Exercise MCP dispatch and the real Calendar client against synthetic TLS DAV data.
func newCalendarTextProtocol(t *testing.T, calendar string) *calendarTextProtocol {
	t.Helper()
	f := &calendarTextProtocol{}
	t.Logf("calendar_fixture=%q", calendar)
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(calendar)); err != nil {
		t.Fatal(err)
	}
	resource := calendarTextPath + "text-event.ics"
	discovery := &redactionTestServer{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PROPFIND" && r.URL.Path == "/":
			discovery.ServeHTTP(w, r)
		case r.Method == "PROPFIND" && r.URL.Path == redactionPrincipalPath:
			writeRedactionXML(w, `<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>`+
				redactionPrincipalPath+`</href><propstat><prop><C:calendar-home-set><href>/text/calendars/</href>`+
				`</C:calendar-home-set></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
		case r.Method == "REPORT" && r.URL.Path == calendarTextPath:
			writeRedactionXML(w, `<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>`+
				resource+`</href><propstat><prop><getetag>`+calendarTextETag+`</getetag><C:calendar-data>`+
				escaped.String()+`</C:calendar-data></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
		case r.Method == http.MethodGet && r.URL.Path == resource:
			w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
			w.Header().Set("ETag", calendarTextETag)
			_, _ = io.WriteString(w, calendar)
		case r.Method == http.MethodDelete && r.URL.Path == resource:
			if r.Header.Get("If-Match") != calendarTextETag {
				t.Error("delete lost its strong If-Match")
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			f.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected DAV request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := srv.Client()
	httpClient.Timeout = 30 * time.Second
	service := icloud.NewClient(httpClient, srv.URL, func(host string) bool { return host == base.Hostname() })
	redactor := security.NewRedactor("synthetic-text-secret")
	s := server.NewMCPServer("calendar-text", "test", server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(redactor)))
	Register(s, Deps{Service: service, Redactor: redactor, DefaultLocation: time.UTC,
		Audit: security.NewAuditLogger(&f.audit)}, false)
	f.client, err = client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Close() })
	if err := f.client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := mcp.InitializeRequest{}
	request.Method = "initialize"
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "calendar-text-test", Version: "test"}
	if _, err := f.client.Initialize(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *calendarTextProtocol) call(t *testing.T, tool string, extra map[string]any, wantError bool, target any) {
	t.Helper()
	args := map[string]any{"calendar": calendarTextPath, "uid": "text-event"}
	if tool == "search_events" {
		args["start"], args["end"] = "2026-11-07T00:00:00Z", "2026-11-09T00:00:00Z"
	}
	for key, value := range extra {
		args[key] = value
	}
	request := mcp.CallToolRequest{}
	request.Method = "tools/call"
	request.Params.Name, request.Params.Arguments = tool, args
	response, err := f.client.CallTool(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"request": request, "response": response})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	if response.IsError != wantError {
		t.Fatalf("%s IsError=%v, want %v: %s", tool, response.IsError, wantError, resultText(t, response))
	}
	if err := json.Unmarshal([]byte(resultText(t, response)), target); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolCalendarTextReadsSearchAndDelete(t *testing.T) {
	const title = "Review, caf\u00e9; path \\n"
	const location = "100 Main St., Springfield; floor \\2"
	const notes = "Bring, notes; then\nnext\nlast \\n \u6771\u4eac"
	properties := "SUMMARY:Review\\, caf\u00e9\\; path \\\\n\n" +
		"LOCATION:100 Main St.\\, Springfield\\; floor \\\\2\n" +
		"DESCRIPTION:Bring\\, notes\\; then\\nnext\\Nlast \\\\n \u6771\u4eac\nRRULE:FREQ=DAILY;COUNT=2\n"
	const overrideTitle = "Changed, title; \\n"
	override := "BEGIN:VEVENT\nUID:text-event\nRECURRENCE-ID:20261108T100000Z\n" +
		"DTSTART:20261108T100000Z\nDTEND:20261108T110000Z\n" +
		"SUMMARY:Changed\\, title\\; \\\\n\nLOCATION:Override\\, room\nDESCRIPTION:Override\\; notes\\nend\nEND:VEVENT\n"
	f := newCalendarTextProtocol(t, calendarTextObject(properties, override))
	t.Run("get", func(t *testing.T) {
		var got getEventDTO
		f.call(t, "get_event", nil, false, &got)
		if got.Title != title || got.Location != location || got.Notes != notes || got.ETag != calendarTextETag {
			t.Errorf("decoded event mismatch: %+v", got)
		}
		if len(got.Overrides) != 1 || got.Overrides[0].Title != overrideTitle {
			t.Errorf("decoded override mismatch: %+v", got.Overrides)
		}
	})
	t.Run("search", func(t *testing.T) {
		var got searchEventsResponse
		f.call(t, "search_events", nil, false, &got)
		if got.Count != 2 {
			t.Fatalf("count=%d, want master and override", got.Count)
		}
		if got.Events[0].Title != title || got.Events[0].Location != location || got.Events[0].Notes != notes {
			t.Errorf("decoded master mismatch: %+v", got.Events[0])
		}
		if got.Events[1].Title != overrideTitle || got.Events[1].Location != "Override, room" || got.Events[1].Notes != "Override; notes\nend" {
			t.Errorf("decoded occurrence mismatch: %+v", got.Events[1])
		}
	})
	for _, query := range []string{"review, CAF\u00c9;", "Main St., Springfield", "notes; then\nnext", "Changed, title;"} {
		t.Run("query_"+query, func(t *testing.T) {
			var got searchEventsResponse
			f.call(t, "search_events", map[string]any{"query": query}, false, &got)
			if got.Count != 1 || got.Total != 1 {
				t.Errorf("query %q matched %d events, want 1", query, got.Count)
			}
		})
	}
	t.Run("unexpanded", func(t *testing.T) {
		var got searchEventsResponse
		f.call(t, "search_events", map[string]any{"expand_recurrence": false, "query": "Main St., Springfield"}, false, &got)
		if got.Count != 1 || got.Events[0].Title != title || got.Events[0].Notes != notes {
			t.Errorf("unexpanded search mismatch: %+v", got)
		}
	})
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("delete_dry_run_%v", dryRun), func(t *testing.T) {
			var got deleteEventResponse
			f.call(t, "delete_event", map[string]any{"dry_run": dryRun}, false, &got)
			if !got.Success || got.DeletedTitle != title || got.DryRun != dryRun || got.Scope != "series" || !got.WouldMutate {
				t.Errorf("delete response mismatch: %+v", got)
			}
			wantDeletes := int32(1)
			if dryRun {
				wantDeletes = 0
			}
			if f.deletes.Load() != wantDeletes {
				t.Errorf("DELETE count=%d, want %d", f.deletes.Load(), wantDeletes)
			}
		})
	}
	if strings.Contains(f.audit.String(), "Review") || strings.Contains(f.audit.String(), "text-event") {
		t.Errorf("audit exposed event content: %s", f.audit.String())
	}
}

func TestProtocolCalendarTextEmptyAndMissing(t *testing.T) {
	for _, properties := range []string{"", "SUMMARY:\nLOCATION:\nDESCRIPTION:\n"} {
		t.Run(fmt.Sprintf("properties_%d", len(properties)), func(t *testing.T) {
			f := newCalendarTextProtocol(t, calendarTextObject(properties, ""))
			var got getEventDTO
			f.call(t, "get_event", nil, false, &got)
			if got.Title != "" || got.Location != "" || got.Notes != "" {
				t.Errorf("empty fields mismatch: %+v", got)
			}
			var page searchEventsResponse
			f.call(t, "search_events", nil, false, &page)
			if page.Count != 1 || page.Events[0].Title != "" || page.Events[0].Location != "" || page.Events[0].Notes != "" {
				t.Errorf("empty search mismatch: %+v", page)
			}
			var deleted deleteEventResponse
			f.call(t, "delete_event", nil, false, &deleted)
			if !deleted.Success || deleted.DeletedTitle != "" || f.deletes.Load() != 1 {
				t.Errorf("empty delete mismatch: %+v", deleted)
			}
		})
	}
}

func TestProtocolCalendarTextMalformed(t *testing.T) {
	for _, test := range []struct {
		name, property string
	}{
		{"title_escape", "SUMMARY:private-text\\q\n"},
		{"location_trailing_backslash", "LOCATION:private-text\\\n"},
		{"notes_value_type", "DESCRIPTION;VALUE=URI:private-text\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCalendarTextProtocol(t, calendarTextObject(test.property, ""))
			tools := []string{"get_event", "search_events"}
			if test.name == "title_escape" {
				tools = append(tools, "delete_event")
			}
			for _, tool := range tools {
				t.Run(tool, func(t *testing.T) {
					var got toolErrorPayload
					f.call(t, tool, nil, true, &got)
					if got.Code != "protocol_error" || strings.Contains(got.Message, "private-text") || strings.Contains(got.Message, "\\q") {
						t.Errorf("unsafe malformed TEXT response: %+v", got)
					}
				})
			}
			if f.deletes.Load() != 0 {
				t.Error("malformed title dispatched a DELETE")
			}
		})
	}
}
