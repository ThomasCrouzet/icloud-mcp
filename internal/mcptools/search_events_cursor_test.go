package mcptools

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const cursorDenseCalendar = "/dense/calendars/main/"

type cursorDAV struct {
	mu      sync.Mutex
	reports int
	paths   []string
	body    string
}

func cursorEscape(value string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(value)); err != nil {
		panic(err)
	}
	return b.String()
}

func cursorDenseReport(count int) string {
	var b strings.Builder
	b.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
	for i := 0; i < count; i++ {
		uid := fmt.Sprintf("dense-%04d", i)
		calendar := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Issue 95 cursor//EN\r\n" +
			"BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20260701T000000Z\r\n" +
			"DTSTART:20260701T090000Z\r\nDTEND:20260701T100000Z\r\n" +
			"SUMMARY:Dense tie\r\nSTATUS:CONFIRMED\r\nTRANSP:OPAQUE\r\n" +
			"END:VEVENT\r\nEND:VCALENDAR\r\n"
		b.WriteString(`<D:response><D:href>` + cursorDenseCalendar + uid + `.ics</D:href>`)
		b.WriteString(`<D:propstat><D:prop><D:getetag>"dense-v1"</D:getetag><C:calendar-data>`)
		b.WriteString(cursorEscape(calendar))
		b.WriteString(`</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	b.WriteString(`</D:multistatus>`)
	return b.String()
}

func (d *cursorDAV) serveHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if r.Method == "REPORT" {
		d.reports++
		d.paths = append(d.paths, r.URL.Path)
	}
	d.mu.Unlock()

	var response string
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		response = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop>` +
			`<D:current-user-principal><D:href>/dense/principal/</D:href></D:current-user-principal>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	case r.Method == "PROPFIND" && r.URL.Path == "/dense/principal/":
		response = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response>` +
			`<D:href>/dense/principal/</D:href><D:propstat><D:prop><C:calendar-home-set>` +
			`<D:href>/dense/calendars/</D:href></C:calendar-home-set>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	case r.Method == "PROPFIND" && r.URL.Path == "/dense/calendars/":
		response = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response>` +
			`<D:href>` + cursorDenseCalendar + `</D:href><D:propstat><D:prop>` +
			`<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>` +
			`<D:displayname>Dense</D:displayname>` +
			`<C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	case r.Method == "REPORT" && r.URL.Path == cursorDenseCalendar:
		response = d.body
	default:
		http.Error(w, "unexpected synthetic DAV request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, response)
}

func (d *cursorDAV) reportCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reports
}

func newCursorProtocolMCP(t *testing.T, svc icloud.Service, scopes string) *client.Client {
	t.Helper()
	red := security.NewRedactor("synthetic-issue95-cursor-secret")
	s := server.NewMCPServer("issue95-cursor", "test",
		server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(),
		server.WithStrictInputSchemaDefault(),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)))
	RegisterUnified(s, Deps{
		Service:         svc,
		Audit:           security.NewAuditLogger(io.Discard),
		Redactor:        red,
		DefaultLocation: time.UTC,
		ResourceScopes:  mustScopes(t, scopes),
	}, NewCapabilityPlan(true, false, false, false, false))
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "issue95-cursor", Version: "test"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatal(err)
	}
	return c
}

func newCursorDAVMCP(t *testing.T, count int) (*client.Client, *cursorDAV) {
	t.Helper()
	dav := &cursorDAV{body: cursorDenseReport(count)}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(dav.serveHTTP))
	t.Cleanup(tlsServer.Close)
	base, err := url.Parse(tlsServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := tlsServer.Client()
	httpClient.Timeout = 30 * time.Second
	calendar := icloud.NewClient(httpClient, tlsServer.URL, func(host string) bool { return host == base.Hostname() })
	return newCursorProtocolMCP(t, calendar, "calendar.read="+cursorDenseCalendar), dav
}

type cursorCall struct {
	payload    searchEventsResponse
	text       string
	token      string
	hasCursor  bool
	byteLength int
}

func cursorCallTool(t *testing.T, c *client.Client, args map[string]any) cursorCall {
	t.Helper()
	call := mcp.CallToolRequest{}
	call.Params.Name = "search_events"
	call.Params.Arguments = args
	res, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("search_events returned an error: %s", text)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	token, _ := raw["nextCursor"].(string)
	_, hasCursor := raw["nextCursor"]
	return cursorCall{payload: payload, text: text, token: token, hasCursor: hasCursor, byteLength: len(text)}
}

func cursorBaseArgs(calendar string, limit, offset int, cursor string) map[string]any {
	args := map[string]any{
		"start":    "2026-07-01T00:00:00Z",
		"end":      "2026-07-02T00:00:00Z",
		"calendar": calendar,
		"limit":    float64(limit),
		"offset":   float64(offset),
	}
	if cursor != "" {
		args["cursor"] = cursor
	}
	return args
}

func cursorUIDSet(events []searchEventDTO) map[string]bool {
	out := make(map[string]bool, len(events))
	for _, event := range events {
		out[event.UID] = true
	}
	return out
}

func decodeCursorError(t *testing.T, res *mcp.CallToolResult) toolErrorPayload {
	t.Helper()
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatalf("decode tool error: %v", err)
	}
	return payload
}

func cursorValidationReason(t *testing.T, payload toolErrorPayload) string {
	t.Helper()
	if payload.Code != string(icloud.CodeValidation) {
		t.Fatalf("error code = %q, want validation: %+v", payload.Code, payload)
	}
	return payload.Details["cursor"]
}

func newTestCursorCodec(now time.Time) *searchCursorCodec {
	return &searchCursorCodec{now: func() time.Time { return now }}
}

func denseMockEvents(calendar string, count int) []icloud.Event {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	events := make([]icloud.Event, count)
	for i := range events {
		events[i] = icloud.Event{
			UID:       fmt.Sprintf("dense-%04d", i),
			Title:     "Dense tie",
			Calendar:  calendar,
			StartTime: start,
			EndTime:   start.Add(time.Hour),
		}
	}
	return events
}

func TestSearchEventsCursorDenseProgression(t *testing.T) {
	for _, total := range []int{450, 2500} {
		t.Run(fmt.Sprintf("%d", total), func(t *testing.T) {
			c, dav := newCursorDAVMCP(t, total)
			args := cursorBaseArgs(cursorDenseCalendar, 400, 0, "start")
			seen := map[string]bool{}
			pages := 0
			var token string
			for {
				call := cursorCallTool(t, c, args)
				if call.payload.Total != total {
					t.Fatalf("page %d total = %d, want %d", pages+1, call.payload.Total, total)
				}
				if call.payload.Count == 0 {
					t.Fatalf("page %d returned no events before completion", pages+1)
				}
				if call.byteLength > maxCalendarResultBytes {
					t.Fatalf("page %d bytes = %d, limit %d", pages+1, call.byteLength, maxCalendarResultBytes)
				}
				for uid := range cursorUIDSet(call.payload.Events) {
					if seen[uid] {
						t.Fatalf("duplicate occurrence %q on page %d", uid, pages+1)
					}
					seen[uid] = true
				}
				pages++
				token = call.token
				if token == "" {
					break
				}
				args = cursorBaseArgs(cursorDenseCalendar, 400, 0, token)
				if pages > 25 {
					t.Fatal("cursor progression exceeded the practical page bound")
				}
			}
			if len(seen) != total {
				t.Fatalf("unique occurrences = %d, want %d", len(seen), total)
			}
			wantPages := (total + 399) / 400
			if pages != wantPages {
				t.Fatalf("pages = %d, want %d", pages, wantPages)
			}
			// The first page performs the scan. Later pages reuse the bounded
			// process-local snapshot, so no backend REPORT repeats.
			if dav.reportCount() != 1 {
				t.Fatalf("REPORT count = %d, want 1", dav.reportCount())
			}
		})
	}
}

func TestSearchEventsCursor2501HardStop(t *testing.T) {
	c, _ := newCursorDAVMCP(t, 2501)
	call := mcp.CallToolRequest{}
	call.Params.Name = "search_events"
	call.Params.Arguments = cursorBaseArgs(cursorDenseCalendar, 400, 0, "start")
	res, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	text := resultText(t, res)
	if !res.IsError {
		t.Fatalf("2501 events returned a page: %s", text)
	}
	payload := decodeCursorError(t, res)
	if payload.Code != string(icloud.CodePayloadTooLarge) {
		t.Fatalf("2501 error code = %q, want payload_too_large: %s", payload.Code, text)
	}
	if strings.Contains(text, "nextCursor") {
		t.Fatalf("2501 refusal exposed a continuation: %s", text)
	}
}

func TestSearchEventsLegacyOffsetWitnessAndNoCursor(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/dense/"}},
		Events:    denseMockEvents("/cal/dense/", 450),
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/dense/")
	first := cursorCallTool(t, c, cursorBaseArgs("/cal/dense/", 400, 0, ""))
	if first.payload.Count != 400 || first.payload.Total != 450 || !first.payload.Truncated {
		t.Fatalf("legacy first page = count %d total %d truncated %v", first.payload.Count, first.payload.Total, first.payload.Truncated)
	}
	if first.hasCursor {
		t.Fatalf("legacy first page exposed nextCursor: %s", first.text)
	}
	second := cursorCallTool(t, c, cursorBaseArgs("/cal/dense/", 400, 400, ""))
	if second.payload.Count != 0 || second.payload.Total != 450 {
		t.Fatalf("legacy offset witness changed: count %d total %d", second.payload.Count, second.payload.Total)
	}
	if second.hasCursor {
		t.Fatalf("legacy offset witness exposed nextCursor: %s", second.text)
	}
}

type cursorFailService struct {
	*icloud.MockService
	failPath string
	err      error
}

func (s *cursorFailService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time, opts *icloud.SearchOptions) (icloud.SearchResult, error) {
	if calendarPath == s.failPath {
		return icloud.SearchResult{}, s.err
	}
	return s.MockService.SearchEvents(ctx, calendarPath, start, end, opts)
}

func TestSearchEventsCursorPartialFailureSuppressesContinuation(t *testing.T) {
	svc := &cursorFailService{
		MockService: &icloud.MockService{
			Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
			EventsByPath: map[string][]icloud.Event{
				"/cal/a/": denseMockEvents("/cal/a/", 2),
			},
		},
		failPath: "/cal/b/",
		err:      errors.New("synthetic calendar failure"),
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/,/cal/b/")
	args := map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendars": "/cal/a/,/cal/b/", "limit": float64(1), "offset": float64(0), "cursor": "start",
	}
	call := cursorCallTool(t, c, args)
	if !call.payload.PartialFailure || !call.payload.Truncated {
		t.Fatalf("partial page flags = %+v", call.payload)
	}
	if call.hasCursor {
		t.Fatalf("partial page exposed nextCursor: %s", call.text)
	}
	for _, warning := range call.payload.Warnings {
		if strings.Contains(warning, "/cal/b/") {
			t.Fatalf("warning exposed a raw calendar path: %q", warning)
		}
	}
}

func TestSearchEventsCursorExpansionSuppressesContinuation(t *testing.T) {
	svc := &icloud.MockService{
		Calendars:       []icloud.Calendar{{Path: "/cal/a/"}},
		Events:          denseMockEvents("/cal/a/", 5),
		SearchTruncated: true,
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	call := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 2, 0, "start"))
	if !call.payload.TruncatedByExpansion || !call.payload.Truncated {
		t.Fatalf("expansion flags = %+v", call.payload)
	}
	if call.hasCursor {
		t.Fatalf("expansion page exposed nextCursor: %s", call.text)
	}
}

func TestSearchEventsCursorAggregateCap(t *testing.T) {
	events := denseMockEvents("/cal/a/", icloud.MaxMultiSearchMaterialized/2+1)
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		Events:    events,
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/,/cal/b/")
	call := mcp.CallToolRequest{}
	call.Params.Name = "search_events"
	call.Params.Arguments = map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendars": "/cal/a/,/cal/b/", "limit": float64(400), "offset": float64(0), "cursor": "start",
	}
	res, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if !res.IsError {
		t.Fatal("aggregate overflow returned a page")
	}
	payload := decodeCursorError(t, res)
	if payload.Code != string(icloud.CodePayloadTooLarge) {
		t.Fatalf("aggregate error code = %q, want payload_too_large", payload.Code)
	}
	if strings.Contains(resultText(t, res), "nextCursor") {
		t.Fatal("aggregate overflow exposed a continuation")
	}
}

func TestSearchEventsCursorByteBudgetIncludesToken(t *testing.T) {
	events := make([]icloud.Event, 4)
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	for i := range events {
		events[i] = icloud.Event{
			UID:       fmt.Sprintf("large-%d", i),
			Title:     "Large notes",
			StartTime: start.Add(time.Duration(i) * time.Hour),
			EndTime:   start.Add(time.Duration(i)*time.Hour + time.Minute),
			Notes:     strings.Repeat("x", 90*1024),
		}
	}
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: events}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	first := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 400, 0, "start"))
	if first.byteLength > maxCalendarResultBytes {
		t.Fatalf("first page bytes = %d, limit %d", first.byteLength, maxCalendarResultBytes)
	}
	if first.payload.Count == 0 || first.payload.Count >= len(events) {
		t.Fatalf("first page count = %d, want a byte-limited subset", first.payload.Count)
	}
	if first.token == "" {
		t.Fatalf("first byte-limited page lost the continuation: %s", first.text)
	}
	if len(first.token) > maxSearchCursorTokenLen {
		t.Fatalf("token length = %d, limit %d", len(first.token), maxSearchCursorTokenLen)
	}
	second := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 400, 0, first.token))
	if second.byteLength > maxCalendarResultBytes {
		t.Fatalf("second page bytes = %d, limit %d", second.byteLength, maxCalendarResultBytes)
	}
	seen := cursorUIDSet(first.payload.Events)
	for uid := range cursorUIDSet(second.payload.Events) {
		if seen[uid] {
			t.Fatalf("byte-budget continuation duplicated %q", uid)
		}
		seen[uid] = true
	}
	if len(seen) != len(events) {
		t.Fatalf("byte-budget continuation reached %d events, want %d", len(seen), len(events))
	}
}

func TestSearchEventsCursorZeroRowByteLimit(t *testing.T) {
	event := icloud.Event{
		UID:       "huge",
		Title:     "Huge notes",
		StartTime: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		Notes:     strings.Repeat("x", 300*1024),
	}
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: []icloud.Event{event}}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	call := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 400, 0, "start"))
	if call.payload.Count != 0 || !call.payload.Truncated || call.hasCursor {
		t.Fatalf("zero-row byte limit = %+v", call.payload)
	}
	found := false
	for _, warning := range call.payload.Warnings {
		if strings.Contains(warning, "compact") {
			found = true
		}
	}
	if !found {
		t.Fatalf("zero-row byte limit lacks narrowing guidance: %+v", call.payload.Warnings)
	}
}

func TestSearchEventsCursorLegacyDuplicatesPreserved(t *testing.T) {
	duplicate := icloud.Event{
		UID:       "same",
		Title:     "Same",
		StartTime: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: []icloud.Event{duplicate, duplicate}}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	legacy := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 10, 0, ""))
	if legacy.payload.Total != 2 || legacy.payload.Count != 2 {
		t.Fatalf("legacy duplicates changed: %+v", legacy.payload)
	}
	if legacy.hasCursor {
		t.Fatal("legacy duplicate page exposed nextCursor")
	}
	cursor := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 10, 0, "start"))
	if cursor.payload.Total != 1 || cursor.payload.Count != 1 {
		t.Fatalf("cursor duplicates were not collapsed: %+v", cursor.payload)
	}
}

func TestSearchEventsCursorTiesAndOverrides(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		EventsByPath: map[string][]icloud.Event{
			"/cal/a/": {{UID: "same", Title: "Same", StartTime: start, EndTime: start.Add(time.Hour)}},
			"/cal/b/": {{UID: "same", Title: "Same", StartTime: start, EndTime: start.Add(time.Hour)}},
		},
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/,/cal/b/")
	first := cursorCallTool(t, c, map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendars": "/cal/a/,/cal/b/", "limit": float64(1), "offset": float64(0), "cursor": "start",
	})
	if first.payload.Count != 1 || first.payload.Events[0].Calendar != "/cal/a/" || first.token == "" {
		t.Fatalf("cross-calendar tie first page = %+v", first.payload)
	}
	second := cursorCallTool(t, c, map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendars": "/cal/a/,/cal/b/", "limit": float64(1), "offset": float64(0), "cursor": first.token,
	})
	if second.payload.Count != 1 || second.payload.Events[0].Calendar != "/cal/b/" || second.token != "" {
		t.Fatalf("cross-calendar tie second page = %+v", second.payload)
	}

	overrideSvc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}},
		Events: []icloud.Event{
			{UID: "series", Title: "Series", StartTime: start, EndTime: start.Add(time.Hour), RecurrenceID: start, IsOverride: true},
			{UID: "series", Title: "Series", StartTime: start, EndTime: start.Add(time.Hour), RecurrenceID: start.Add(time.Hour), IsOverride: true},
		},
	}
	overrideClient := newCursorProtocolMCP(t, overrideSvc, "calendar.read=/cal/a/")
	page := cursorCallTool(t, overrideClient, cursorBaseArgs("/cal/a/", 1, 0, "start"))
	if page.payload.Total != 2 || page.payload.Count != 1 || page.token == "" {
		t.Fatalf("override first page = %+v", page.payload)
	}
	next := cursorCallTool(t, overrideClient, cursorBaseArgs("/cal/a/", 1, 0, page.token))
	if next.payload.Count != 1 || next.token != "" {
		t.Fatalf("override second page = %+v", next.payload)
	}
	if next.payload.Events[0].RecurrenceID == page.payload.Events[0].RecurrenceID {
		t.Fatalf("override identities collapsed: %+v %+v", page.payload.Events[0], next.payload.Events[0])
	}
}

func TestSearchEventsCursorDuplicateAndReorderedSelection(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		EventsByPath: map[string][]icloud.Event{
			"/cal/a/": denseMockEvents("/cal/a/", 3),
			"/cal/b/": denseMockEvents("/cal/b/", 3),
		},
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/,/cal/b/")
	first := cursorCallTool(t, c, map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/a/", "calendars": "/cal/a/,/cal/b/,/cal/a/",
		"limit": float64(2), "offset": float64(0), "cursor": "start",
	})
	if first.payload.Total != 6 || first.token == "" {
		t.Fatalf("duplicate selection page = %+v", first.payload)
	}
	reordered := cursorCallTool(t, c, map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendars": "/cal/b/,/cal/a/",
		"limit":     float64(2), "offset": float64(0), "cursor": first.token,
	})
	if reordered.payload.Count != 2 || reordered.token == "" {
		t.Fatalf("reordered selection page = %+v", reordered.payload)
	}
}

func TestSearchEventsCursorLimitCanChange(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	first := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 2, 0, "start"))
	if first.payload.Count != 2 || first.token == "" {
		t.Fatalf("first page = %+v", first.payload)
	}
	repeat := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 1, 0, first.token))
	second := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 1, 0, first.token))
	if repeat.payload.Count != 1 || second.payload.Count != 1 {
		t.Fatalf("repeat/second page = %+v %+v", repeat.payload, second.payload)
	}
	if repeat.payload.Events[0].UID != second.payload.Events[0].UID {
		t.Fatalf("reusable cursor changed its page: %+v %+v", repeat.payload, second.payload)
	}
	third := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 10, 0, second.token))
	if third.payload.Count != 2 || third.token != "" {
		t.Fatalf("third page = %+v", third.payload)
	}
}

func TestSearchEventsCursorUnavailableKeyFailsClosed(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	deps := testDeps(svc)
	deps.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	handler := searchEventsHandlerWithCodec(deps, &searchCursorCodec{err: errors.New("synthetic key failure")})
	res, err := handler(context.Background(), newReq(cursorBaseArgs("/cal/a/", 2, 0, "start")))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("cursor mode ignored key failure: %s", resultText(t, res))
	}
	payload := decodeCursorError(t, res)
	if payload.Code != string(icloud.CodeInternal) {
		t.Fatalf("key failure code = %q, want internal_error", payload.Code)
	}
	if svc.SearchCallCount != 0 {
		t.Fatalf("key failure reached backend: %d", svc.SearchCallCount)
	}

	legacy, err := handler(context.Background(), newReq(cursorBaseArgs("/cal/a/", 2, 0, "")))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.IsError {
		t.Fatalf("key failure weakened legacy access: %s", resultText(t, legacy))
	}
}

func TestSearchEventsCursorValidationBeforeBackend(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	codec := newTestCursorCodec(now)
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		EventsByPath: map[string][]icloud.Event{
			"/cal/a/": denseMockEvents("/cal/a/", 5),
			"/cal/b/": denseMockEvents("/cal/b/", 5),
		},
	}
	deps := testDeps(svc)
	deps.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/,/cal/b/")
	handler := searchEventsHandlerWithCodec(deps, codec)

	base := cursorBaseArgs("/cal/a/", 1, 0, "start")
	good, err := handler(context.Background(), newReq(base))
	if err != nil || good.IsError {
		t.Fatalf("token setup failed: err=%v result=%s", err, resultText(t, good))
	}
	var issued searchEventsResponse
	decodeResult(t, good, &issued)
	var issuedRaw map[string]any
	if err := json.Unmarshal([]byte(resultText(t, good)), &issuedRaw); err != nil {
		t.Fatal(err)
	}
	token, _ := issuedRaw["nextCursor"].(string)
	if token == "" {
		t.Fatal("token setup returned no nextCursor")
	}
	searchesAfterToken := svc.SearchCallCount

	for _, tc := range []struct {
		name   string
		args   map[string]any
		reason string
	}{
		{"empty", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "offset": float64(0), "cursor": ""}, searchCursorReasonMalformed},
		{"whitespace", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "offset": float64(0), "cursor": " "}, searchCursorReasonMalformed},
		{"wrong-type", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "offset": float64(0), "cursor": 5}, searchCursorReasonMalformed},
		{"too-long", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "offset": float64(0), "cursor": strings.Repeat("a", maxSearchCursorTokenLen+1)}, searchCursorReasonMalformed},
		{"missing-selection", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "cursor": "start", "offset": float64(0)}, searchCursorReasonSelectionRequired},
		{"empty-selection", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendars": " , ", "cursor": "start", "offset": float64(0)}, searchCursorReasonSelectionRequired},
		{"offset", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "cursor": "start", "offset": float64(1)}, searchCursorReasonOffsetNotAllowed},
		{"cross-query", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "cursor": token, "offset": float64(0), "query": "different"}, searchCursorReasonQueryMismatch},
		{"cross-calendar", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/b/", "cursor": token, "offset": float64(0)}, searchCursorReasonQueryMismatch},
		{"cross-range", map[string]any{"start": "2026-07-01T01:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "cursor": token, "offset": float64(0)}, searchCursorReasonQueryMismatch},
		{"forged", map[string]any{"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z", "calendar": "/cal/a/", "cursor": token + "A", "offset": float64(0)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := svc.SearchCallCount
			res, callErr := handler(context.Background(), newReq(tc.args))
			if callErr != nil {
				t.Fatalf("unexpected protocol error: %v", callErr)
			}
			if !res.IsError {
				t.Fatalf("accepted invalid cursor: %s", resultText(t, res))
			}
			payload := decodeCursorError(t, res)
			reason := cursorValidationReason(t, payload)
			if tc.reason != "" && reason != tc.reason {
				t.Fatalf("cursor reason = %q, want %q", reason, tc.reason)
			}
			if tc.reason == "" && reason != searchCursorReasonSignature && reason != searchCursorReasonMalformed {
				t.Fatalf("forged cursor reason = %q", reason)
			}
			if svc.SearchCallCount != before {
				t.Fatalf("invalid cursor reached Calendar search: before=%d after=%d", before, svc.SearchCallCount)
			}
		})
	}

	if svc.SearchCallCount != searchesAfterToken {
		t.Fatalf("validation table amplified backend searches: %d -> %d", searchesAfterToken, svc.SearchCallCount)
	}

	codec.now = func() time.Time { return now.Add(searchCursorTTL + time.Second) }
	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/a/", "cursor": token, "offset": float64(0),
	}))
	if err != nil {
		t.Fatal(err)
	}
	expired := cursorValidationReason(t, decodeCursorError(t, res))
	if expired != searchCursorReasonExpired {
		t.Fatalf("expired reason = %q", expired)
	}
}

func TestSearchEventsCursorCrossRegistrationAndScope(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	svcA := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	svcB := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	depsA := testDeps(svcA)
	depsA.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	handlerA := searchEventsHandler(depsA)
	tokenRes, err := handlerA(context.Background(), newReq(cursorBaseArgs("/cal/a/", 1, 0, "start")))
	if err != nil || tokenRes.IsError {
		t.Fatalf("token setup: err=%v result=%s", err, resultText(t, tokenRes))
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(resultText(t, tokenRes)), &raw); err != nil {
		t.Fatal(err)
	}
	token, _ := raw["nextCursor"].(string)
	if token == "" {
		t.Fatal("no token")
	}

	depsB := testDeps(svcB)
	depsB.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	handlerB := searchEventsHandler(depsB)
	res, err := handlerB(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/a/", "cursor": token, "offset": float64(0),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if reason := cursorValidationReason(t, decodeCursorError(t, res)); reason != searchCursorReasonSignature {
		t.Fatalf("cross-registration reason = %q", reason)
	}
	if svcB.SearchCallCount != 0 {
		t.Fatalf("cross-registration reached Calendar search: %d", svcB.SearchCallCount)
	}

	codec := newTestCursorCodec(now)
	scopeA := testDeps(svcA)
	scopeA.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/,/cal/b/")
	wide := searchEventsHandlerWithCodec(scopeA, codec)
	wideRes, err := wide(context.Background(), newReq(cursorBaseArgs("/cal/a/", 1, 0, "start")))
	if err != nil || wideRes.IsError {
		t.Fatalf("wide token setup: err=%v result=%s", err, resultText(t, wideRes))
	}
	if err := json.Unmarshal([]byte(resultText(t, wideRes)), &raw); err != nil {
		t.Fatal(err)
	}
	wideToken, _ := raw["nextCursor"].(string)
	if wideToken == "" {
		t.Fatal("no wide token")
	}
	scopeB := testDeps(svcB)
	scopeB.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	narrow := searchEventsHandlerWithCodec(scopeB, codec)
	narrowRes, err := narrow(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/a/", "cursor": wideToken, "offset": float64(0),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if reason := cursorValidationReason(t, decodeCursorError(t, narrowRes)); reason != searchCursorReasonQueryMismatch {
		t.Fatalf("cross-scope reason = %q", reason)
	}
	if svcB.SearchCallCount != 0 {
		t.Fatalf("cross-scope reached Calendar search: %d", svcB.SearchCallCount)
	}
}

func TestSearchEventsCursorStaleAnchor(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	deps := testDeps(svc)
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	codec := newSearchCursorCodec()
	codec.now = func() time.Time { return now }
	codec.ttl = time.Second
	handler := searchEventsHandlerWithCodec(deps, codec)

	first, err := handler(context.Background(), newReq(cursorBaseArgs("/cal/a/", 2, 0, "start")))
	if err != nil || first.IsError {
		t.Fatalf("first page: err=%v result=%s", err, resultText(t, first))
	}
	var firstPayload searchEventsResponse
	decodeResult(t, first, &firstPayload)
	if firstPayload.NextCursor == "" {
		t.Fatal("no token")
	}
	svc.Events = append(append([]icloud.Event(nil), svc.Events[:1]...), svc.Events[2:]...)
	before := svc.SearchCallCount

	// Within the snapshot lifetime the anchor remains, so the page is
	// consistent and no backend call repeats.
	second, err := handler(context.Background(), newReq(cursorBaseArgs("/cal/a/", 2, 0, firstPayload.NextCursor)))
	if err != nil || second.IsError {
		t.Fatalf("cached page: err=%v result=%s", err, resultText(t, second))
	}
	if svc.SearchCallCount != before {
		t.Fatalf("cached page searched %d times", svc.SearchCallCount-before)
	}

	// After expiry the original generation is gone. The token must fail as
	// stale instead of rebasing onto a newer scan.
	now = now.Add(2 * time.Second)
	res, err := handler(context.Background(), newReq(cursorBaseArgs("/cal/a/", 2, 0, firstPayload.NextCursor)))
	if err != nil {
		t.Fatal(err)
	}
	payload := decodeCursorError(t, res)
	if reason := cursorValidationReason(t, payload); reason != searchCursorReasonStale {
		t.Fatalf("stale reason = %q", reason)
	}
	if svc.SearchCallCount != before {
		t.Fatalf("stale check rescanned: %d searches", svc.SearchCallCount-before)
	}
}

func TestSearchEventsCursorChangingSource(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	events := make([]icloud.Event, 5)
	for i := range events {
		events[i] = icloud.Event{
			UID: fmt.Sprintf("e-%d", i), Title: "Event", StartTime: start.Add(time.Duration(i) * time.Hour),
			EndTime: start.Add(time.Duration(i)*time.Hour + time.Minute),
		}
	}
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: append([]icloud.Event(nil), events...)}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	first := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 2, 0, "start"))
	if first.token == "" {
		t.Fatal("no token")
	}

	inserted := icloud.Event{UID: "inserted", Title: "Inserted", StartTime: start.Add(30 * time.Minute), EndTime: start.Add(45 * time.Minute)}
	svc.Events = append([]icloud.Event{events[0], inserted}, events[1:]...)
	page := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 10, 0, first.token))
	if cursorUIDSet(page.payload.Events)["inserted"] {
		t.Fatal("row inserted before the anchor appeared after the anchor")
	}
	if page.payload.Count != 3 {
		t.Fatalf("insert-before-anchor page count = %d, want 3", page.payload.Count)
	}

	svc.Events = append([]icloud.Event(nil), events...)
	moved := events[0]
	moved.StartTime = start.Add(3*time.Hour + 30*time.Minute)
	moved.EndTime = moved.StartTime.Add(time.Minute)
	svc.Events[0] = moved
	page = cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 10, 0, first.token))
	if cursorUIDSet(page.payload.Events)["e-0"] {
		t.Fatal("a source edit appeared inside a live cursor snapshot")
	}
	if page.payload.Count != 3 {
		t.Fatalf("snapshot page count = %d, want 3", page.payload.Count)
	}
}

func TestSearchEventsCursorCancellation(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	deps := testDeps(svc)
	deps.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	handler := searchEventsHandler(deps)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := handler(ctx, newReq(cursorBaseArgs("/cal/a/", 2, 0, "start")))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("canceled cursor call returned a page: %s", resultText(t, res))
	}
	if svc.SearchCallCount != 0 {
		t.Fatalf("canceled cursor call searched %d times", svc.SearchCallCount)
	}
}

func TestSearchEventsCursorSchema(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: denseMockEvents("/cal/a/", 5)}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")
	tools, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "search_events" {
			continue
		}
		input, _ := json.Marshal(tool.InputSchema)
		output, _ := json.Marshal(tool.OutputSchema)
		var inputSchema, outputSchema map[string]any
		if err := json.Unmarshal(input, &inputSchema); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(output, &outputSchema); err != nil {
			t.Fatal(err)
		}
		props, _ := inputSchema["properties"].(map[string]any)
		cursorProp, _ := props["cursor"].(map[string]any)
		if cursorProp == nil || cursorProp["maxLength"] != float64(maxSearchCursorTokenLen) {
			t.Fatalf("cursor input schema = %#v", cursorProp)
		}
		outProps, _ := outputSchema["properties"].(map[string]any)
		if outProps["nextCursor"] == nil {
			t.Fatalf("nextCursor output schema absent: %s", output)
		}
		return
	}
	t.Fatal("search_events not registered")
}

// TestSearchScanSnapshotBoundAndExpiry proves the cache is bounded and does
// not outlive the cursor lifetime.
func TestSearchScanSnapshotBoundAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	codec := newSearchCursorCodec()
	codec.now = func() time.Time { return now }
	var firstKey searchCursorDigest
	firstKey[0] = 1
	firstGeneration := ""
	for i := range maxSearchScanSnapshots + 2 {
		var key searchCursorDigest
		key[0] = byte(i + 1)
		generation := codec.storeScan(key, searchScanSnapshot{rows: []searchCursorRow{{}}})
		if i == 0 {
			firstGeneration = generation
		}
		now = now.Add(time.Second)
	}
	codec.mu.Lock()
	got := len(codec.scans)
	codec.mu.Unlock()
	if got != maxSearchScanSnapshots {
		t.Fatalf("snapshots = %d, want %d", got, maxSearchScanSnapshots)
	}
	if _, ok := codec.loadScan(firstKey, firstGeneration); ok {
		t.Fatal("oldest snapshot was not evicted")
	}
	now = now.Add(searchCursorTTL)
	if _, ok := codec.loadScan(firstKey, firstGeneration); ok {
		t.Fatal("expired snapshot was returned")
	}
}

func TestSearchScanGenerationsStayIndependent(t *testing.T) {
	codec := newSearchCursorCodec()
	var query searchCursorDigest
	firstRows := []searchCursorRow{{event: icloud.Event{UID: "a"}}, {event: icloud.Event{UID: "b"}}}
	secondRows := []searchCursorRow{{event: icloud.Event{UID: "a"}}, {event: icloud.Event{UID: "x"}}, {event: icloud.Event{UID: "b"}}}
	first := codec.storeScan(query, searchScanSnapshot{rows: firstRows})
	second := codec.storeScan(query, searchScanSnapshot{rows: secondRows})
	if first == second || first == "" || second == "" {
		t.Fatalf("generations = %q / %q", first, second)
	}
	loaded, ok := codec.loadScan(query, first)
	if !ok || len(loaded.rows) != 2 || loaded.rows[1].event.UID != "b" {
		t.Fatalf("first generation = %#v, %v", loaded.rows, ok)
	}
	loaded, ok = codec.loadScan(query, second)
	if !ok || len(loaded.rows) != 3 {
		t.Fatalf("second generation = %#v, %v", loaded.rows, ok)
	}
	if _, ok := codec.loadScan(query, "00000000000000000000000000000000"); ok {
		t.Fatal("unknown generation was loaded")
	}
}

func TestSearchScanCacheHonorsByteBudget(t *testing.T) {
	codec := newSearchCursorCodec()
	var query searchCursorDigest
	big := strings.Repeat("n", 8<<20)
	for i := 0; i < 4; i++ {
		generation := codec.storeScan(query, searchScanSnapshot{rows: []searchCursorRow{{
			event: icloud.Event{UID: "u", Notes: big},
		}}})
		if generation == "" {
			t.Fatalf("snapshot %d was refused", i)
		}
	}
	codec.mu.Lock()
	total := 0
	for _, snapshot := range codec.scans {
		total += snapshot.bytes
	}
	count := len(codec.scans)
	codec.mu.Unlock()
	if total > maxSearchScanBytes {
		t.Fatalf("cache bytes = %d, want at most %d", total, maxSearchScanBytes)
	}
	if count == 0 || count >= 4 {
		t.Fatalf("cache entries = %d, want bounded eviction below 4", count)
	}

	oversized := codec.storeScan(query, searchScanSnapshot{rows: []searchCursorRow{{
		event: icloud.Event{UID: "huge", Notes: strings.Repeat("n", maxSearchScanBytes+1)},
	}}})
	if oversized != "" {
		t.Fatal("an oversized snapshot was retained")
	}
}
