package mcptools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const recurrenceProtocolHome = "/recurrence/calendars/"
const recurrenceProtocolETag = `"recurrence-fixture"`

// These scenarios use JSON-RPC dispatch, the real Calendar client, and a local TLS DAV server.
// They do not start the production executable or use live credentials.
func TestProtocolRecurrenceCombinedDST(t *testing.T) {
	for _, name := range []string{"spring", "fall"} {
		t.Run(name, func(t *testing.T) {
			var fixture struct {
				Timezone  string              `json:"timezone"`
				Calendars map[string][]string `json:"calendars"`
				Calls     []struct {
					Name      string          `json:"name"`
					Tool      string          `json:"tool"`
					Arguments map[string]any  `json:"arguments"`
					Result    json.RawMessage `json:"result"`
				} `json:"calls"`
			}
			if err := json.Unmarshal(readRecurrenceProtocolFixture(t, name+".json"), &fixture); err != nil {
				t.Fatal(err)
			}
			if len(fixture.Calls) != 4 {
				t.Fatalf("fixture has %d calls, want search and three availability checks", len(fixture.Calls))
			}
			protocol := newRecurrenceProtocol(t, fixture.Timezone, fixture.Calendars)
			for _, call := range fixture.Calls {
				t.Run(call.Name, func(t *testing.T) {
					result := protocol.call(t, call.Tool, call.Arguments)
					assertRecurrenceProtocolResult(t, result, false, call.Result)
				})
			}
		})
	}
}

func TestProtocolRecurrenceOccurrenceLimit(t *testing.T) {
	for _, count := range []int{2000, 2001} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			path := recurrenceProtocolHome + "minutes/"
			empty := recurrenceProtocolHome + "empty/"
			protocol := newRecurrenceProtocol(t, "UTC", map[string][]string{
				path: {fmt.Sprintf("minutes-%d.ics", count)}, empty: {},
			})
			args := map[string]any{
				"calendar": path, "start": "2026-01-01T00:00:00Z", "end": "2026-01-02T12:00:00Z", "limit": 400,
			}
			// The expansion cap is 2000. The public search cap is 400.
			want := recurrenceProtocolMinutePage("minutes", "Minutes", "2026-01-01T00:00:00Z", 400, 2000)
			if count == 2001 {
				want["truncatedByExpansion"] = true
			}
			assertRecurrenceProtocolResult(t, protocol.call(t, "search_events", args), false, want)

			freeArgs := map[string]any{
				"calendars": empty + "," + path, "start": args["start"], "end": args["end"], "duration_minutes": 60,
			}
			result := protocol.call(t, "find_free_slots", freeArgs)
			if count == 2001 {
				assertRecurrenceProtocolError(t, result, "partial_failure", "recurrence expansion was truncated; availability cannot be determined")
			} else {
				assertRecurrenceProtocolResult(t, result, false, json.RawMessage(`{"count":2,"slots":[
					{"start":"2026-01-02T09:20:00+00:00","end":"2026-01-02T10:20:00+00:00"},
					{"start":"2026-01-02T10:20:00+00:00","end":"2026-01-02T11:20:00+00:00"}
				]}`))
			}
		})
	}
}

func TestProtocolRecurrenceRejectedBudgets(t *testing.T) {
	for _, test := range []struct {
		name     string
		fixtures []string
		start    string
		end      string
		message  string
	}{
		{
			name: "iterator", fixtures: []string{"iterator-limit.ics"},
			start: "2026-03-11T10:39:00Z", end: "2026-03-11T10:42:00Z",
			message: "Calendar recurrence rule exceeded its expansion-work limit",
		},
		{
			name: "work_estimate", fixtures: []string{"estimate-limit.ics"},
			start: "2026-03-11T10:39:00Z", end: "2026-03-11T10:42:00Z",
			message: "Calendar recurrence rule requires excessive expansion work",
		},
		{
			name: "event_materialization", fixtures: []string{"minutes-2000.ics", "work-a.ics"},
			start: "2026-01-01T00:00:00Z", end: "2026-01-02T00:00:00Z",
			message: "Calendar search exceeded its event limit",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := recurrenceProtocolHome + "rejected/"
			empty := recurrenceProtocolHome + "empty/"
			protocol := newRecurrenceProtocol(t, "UTC", map[string][]string{path: test.fixtures, empty: {}})
			search := map[string]any{"calendar": path, "start": test.start, "end": test.end}
			assertRecurrenceProtocolError(t, protocol.call(t, "search_events", search), "payload_too_large", test.message)
			free := map[string]any{
				"calendars": empty + "," + path, "start": test.start, "end": test.end, "duration_minutes": 1,
			}
			assertRecurrenceProtocolError(t, protocol.call(t, "find_free_slots", free), "payload_too_large", test.message)
		})
	}
}

func TestProtocolRecurrenceAggregateWorkBudget(t *testing.T) {
	collections := map[string][]string{
		recurrenceProtocolHome + "aggregate/": {"work-a.ics", "work-b.ics", "work-c.ics"},
	}
	for _, uid := range []string{"work-a", "work-b", "work-c"} {
		collections[recurrenceProtocolHome+uid+"/"] = []string{uid + ".ics"}
	}
	protocol := newRecurrenceProtocol(t, "UTC", collections)
	start, end := "2026-03-04T11:58:00Z", "2026-03-04T12:02:00Z"
	// Each series needs 90001 iterator calls, but returns only two occurrences.
	// Three series exceed the shared 250000-step budget before the event cap.
	for _, uid := range []string{"work-a", "work-b", "work-c"} {
		args := map[string]any{"calendar": recurrenceProtocolHome + uid + "/", "start": start, "end": end, "limit": 400}
		want := recurrenceProtocolMinutePage(uid, "Work", start, 2, 2)
		assertRecurrenceProtocolResult(t, protocol.call(t, "search_events", args), false, want)
		args["duration_minutes"] = 1
		args["limit"] = 50
		assertRecurrenceProtocolResult(t, protocol.call(t, "find_free_slots", args), false, json.RawMessage(`{"count":2,"slots":[
			{"start":"2026-03-04T12:00:00+00:00","end":"2026-03-04T12:01:00+00:00"},
			{"start":"2026-03-04T12:01:00+00:00","end":"2026-03-04T12:02:00+00:00"}
		]}`))
	}
	args := map[string]any{"calendar": recurrenceProtocolHome + "aggregate/", "start": start, "end": end}
	const message = "Calendar recurrence rule requires excessive expansion work"
	assertRecurrenceProtocolError(t, protocol.call(t, "search_events", args), "payload_too_large", message)
	free := map[string]any{
		"calendars": recurrenceProtocolHome + "work-a/," + recurrenceProtocolHome + "aggregate/",
		"start":     start, "end": end, "duration_minutes": 1,
	}
	assertRecurrenceProtocolError(t, protocol.call(t, "find_free_slots", free), "payload_too_large", message)
	// A rejected request must not consume the next request's recurrence budget.
	args["calendar"] = recurrenceProtocolHome + "work-a/"
	args["limit"] = 400
	assertRecurrenceProtocolResult(t, protocol.call(t, "search_events", args), false,
		recurrenceProtocolMinutePage("work-a", "Work", start, 2, 2))
}

type recurrenceProtocol struct {
	transport *mcptransport.InProcessTransport
	nextID    int64
	dav       *recurrenceProtocolDAV
}

type recurrenceProtocolDAV struct {
	reports  map[string]string
	mu       sync.Mutex
	requests []recurrenceProtocolDAVRequest
}

type recurrenceProtocolDAVRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Depth  string `json:"depth"`
	Body   string `json:"body"`
}

func readRecurrenceProtocolFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "recurrence", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logRecurrenceProtocolJSON(t, "calendar_fixture", map[string]string{
		"path": "internal/mcptools/" + filepath.ToSlash(path), "sha256": fmt.Sprintf("%x", sha256.Sum256(data)),
	})
	return data
}

func newRecurrenceProtocol(t *testing.T, timezone string, collections map[string][]string) *recurrenceProtocol {
	t.Helper()
	dav := &recurrenceProtocolDAV{reports: make(map[string]string)}
	paths := make([]string, 0, len(collections))
	for path := range collections {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		var body strings.Builder
		body.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
		for _, name := range collections[path] {
			data := readRecurrenceProtocolFixture(t, name)
			// Source fixtures use LF. DAV calendar data uses CRLF.
			calendar := strings.ReplaceAll(string(data), "\n", "\r\n")
			body.WriteString(`<D:response><D:href>` + path + name + `</D:href><D:propstat><D:prop><D:getetag>`)
			body.WriteString(recurrenceProtocolETag)
			body.WriteString(`</D:getetag><C:calendar-data>`)
			if err := xml.EscapeText(&body, []byte(calendar)); err != nil {
				t.Fatal(err)
			}
			body.WriteString(`</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		}
		body.WriteString(`</D:multistatus>`)
		dav.reports[path] = body.String()
		logRecurrenceProtocolJSON(t, "calendar_report", map[string]any{
			"path": path, "fixtures": collections[path], "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(body.String()))),
		})
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(dav.serveHTTP))
	t.Cleanup(tlsServer.Close)
	base, err := url.Parse(tlsServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := tlsServer.Client()
	httpClient.Timeout = 30 * time.Second
	calendar := icloud.NewClient(httpClient, tlsServer.URL, func(host string) bool { return host == base.Hostname() })
	redactor := security.NewRedactor("synthetic-calendar-secret")
	mcpServer := server.NewMCPServer("recurrence-protocol", "test",
		server.WithToolCapabilities(false), server.WithToolHandlerMiddleware(RecoverRedactMiddleware(redactor)))
	Register(mcpServer, Deps{Service: calendar, Redactor: redactor, DefaultLocation: location}, true)
	transport := mcptransport.NewInProcessTransport(mcpServer)
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transport.Close(); err != nil {
			t.Error(err)
		}
	})
	protocol := &recurrenceProtocol{transport: transport, dav: dav}
	protocol.request(t, "initialize", map[string]any{
		"protocolVersion": mcp.LATEST_PROTOCOL_VERSION, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "recurrence-protocol", "version": "test"},
	})
	notification := mcp.JSONRPCNotification{JSONRPC: "2.0", Notification: mcp.Notification{Method: "notifications/initialized"}}
	logRecurrenceProtocolJSON(t, "mcp_request", notification)
	if err := transport.SendNotification(context.Background(), notification); err != nil {
		t.Fatal(err)
	}
	return protocol
}

func (d *recurrenceProtocolDAV) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "request read failed", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.requests = append(d.requests, recurrenceProtocolDAVRequest{Method: r.Method, Path: r.URL.Path, Depth: r.Header.Get("Depth"), Body: string(body)})
	d.mu.Unlock()
	var response string
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		response = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop>` +
			`<D:current-user-principal><D:href>/recurrence/principal/</D:href></D:current-user-principal>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	case r.Method == "PROPFIND" && r.URL.Path == "/recurrence/principal/":
		response = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response>` +
			`<D:href>/recurrence/principal/</D:href><D:propstat><D:prop><C:calendar-home-set><D:href>` + recurrenceProtocolHome +
			`</D:href></C:calendar-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	case r.Method == "REPORT":
		response = d.reports[r.URL.Path]
	}
	if response == "" {
		http.Error(w, "unexpected DAV request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, response)
}

func (p *recurrenceProtocol) request(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	p.nextID++
	request := mcptransport.JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(p.nextID), Method: method, Params: params}
	logRecurrenceProtocolJSON(t, "mcp_request", request)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	response, err := p.transport.SendRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	logRecurrenceProtocolJSON(t, "mcp_result", response)
	if response.JSONRPC != "2.0" || response.ID != request.ID || response.Error != nil {
		t.Fatalf("unexpected JSON-RPC response: %+v", response)
	}
	return response.Result
}

func (p *recurrenceProtocol) call(t *testing.T, tool string, args map[string]any) json.RawMessage {
	t.Helper()
	result := p.request(t, "tools/call", map[string]any{"name": tool, "arguments": args})
	p.dav.mu.Lock()
	requests := p.dav.requests
	p.dav.requests = nil
	p.dav.mu.Unlock()
	var paths []string
	for _, request := range requests {
		logRecurrenceProtocolJSON(t, "calendar_dav_request", request)
		if request.Method == "PROPFIND" {
			if request.Depth != "0" || request.Path != "/" && request.Path != "/recurrence/principal/" {
				t.Fatalf("unexpected discovery request: %+v", request)
			}
			continue
		}
		if request.Method != "REPORT" || request.Depth != "1" {
			t.Fatalf("unexpected Calendar request: %+v", request)
		}
		var query struct {
			Range struct {
				Start string `xml:"start,attr"`
				End   string `xml:"end,attr"`
			} `xml:"filter>comp-filter>comp-filter>time-range"`
		}
		if err := xml.Unmarshal([]byte(request.Body), &query); err != nil {
			t.Fatal(err)
		}
		for key, actual := range map[string]string{"start": query.Range.Start, "end": query.Range.End} {
			value, ok := args[key].(string)
			if !ok {
				t.Fatalf("missing %s argument", key)
			}
			instant, err := time.Parse(time.RFC3339, value)
			if err != nil {
				t.Fatal(err)
			}
			if want := instant.UTC().Format("20060102T150405Z"); actual != want {
				t.Fatalf("REPORT %s = %q, want %q", key, actual, want)
			}
		}
		paths = append(paths, request.Path)
	}
	selection, _ := args["calendar"].(string)
	if multiple, ok := args["calendars"].(string); ok {
		selection = multiple
	}
	if want := strings.Split(selection, ","); !reflect.DeepEqual(paths, want) {
		t.Fatalf("REPORT paths = %v, want %v", paths, want)
	}
	return result
}

func logRecurrenceProtocolJSON(t *testing.T, label string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s=%s", label, encoded)
}

func assertRecurrenceProtocolResult(t *testing.T, result json.RawMessage, isError bool, want any) {
	t.Helper()
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.IsError != isError || len(envelope.Content) != 1 || envelope.Content[0].Type != "text" {
		t.Fatalf("unexpected tool result: %s", result)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var actualJSON, expectedJSON any
	if err := json.Unmarshal([]byte(envelope.Content[0].Text), &actualJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &expectedJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualJSON, expectedJSON) {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, expected, "", "  "); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("tool payload differs\ngot: %s\nwant: %s", envelope.Content[0].Text, formatted.String())
	}
}

func assertRecurrenceProtocolError(t *testing.T, result json.RawMessage, code, message string) {
	t.Helper()
	// Exact equality also proves that no partial events or free slots are present.
	assertRecurrenceProtocolResult(t, result, true, map[string]any{
		"code": code, "message": "searching events: " + code + ": " + message,
	})
}

func recurrenceProtocolMinutePage(uid, title, first string, count, total int) map[string]any {
	start, err := time.Parse(time.RFC3339, first)
	if err != nil {
		panic(err)
	}
	rows := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		instant := start.Add(time.Duration(i) * time.Minute)
		formatted := instant.Format("2006-01-02T15:04:05-07:00")
		rows = append(rows, map[string]any{
			"uid": uid, "title": title, "start": formatted, "end": instant.Add(time.Minute).Format("2006-01-02T15:04:05-07:00"),
			"recurrenceId": formatted, "etag": recurrenceProtocolETag,
		})
	}
	return map[string]any{"count": count, "total": total, "offset": 0, "limit": 400, "truncated": total > 400, "events": rows}
}
