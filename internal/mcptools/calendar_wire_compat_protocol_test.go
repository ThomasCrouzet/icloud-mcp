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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// These protocol tests record the Calendar and Contacts wire behavior that the
// go-webdav and go-ical updates can change: malformed iCalendar parameters,
// collection hrefs with and without a trailing slash, and line breaks in
// values that a write encodes.

const (
	wireHomePath = "/wire/calendars/"
	wireUID      = "wire-event"
	wireETag     = `"wire-1"`
	wireNewETag  = `"wire-2"`
)

type wireCalendar struct {
	href   string // collection href in the home-set listing
	name   string
	object string // one event with UID wireUID
}

type wireProtocol struct {
	client    *client.Client
	calendars []wireCalendar
	puts      atomic.Int32
	deletes   atomic.Int32
	audit     bytes.Buffer

	mu        sync.Mutex
	requests  []string
	putBodies []string
}

func wireCollection(href string) string { return strings.TrimSuffix(href, "/") }

func wireResource(href string) string { return wireCollection(href) + "/" + wireUID + ".ics" }

func wireObject(properties string) string {
	return strings.ReplaceAll("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Wire fixture//EN\n"+
		"BEGIN:VEVENT\nUID:"+wireUID+"\nDTSTAMP:20261107T000000Z\n"+
		"DTSTART:20261107T100000Z\nDTEND:20261107T110000Z\nSUMMARY:Wire event\n"+
		properties+"END:VEVENT\nEND:VCALENDAR\n", "\n", "\r\n")
}

func wireXMLText(t *testing.T, value string) string {
	t.Helper()
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		t.Fatal(err)
	}
	return escaped.String()
}

// newWireProtocol serves discovery, one home-set listing, REPORT, GET, PUT,
// and DELETE over synthetic TLS. It matches each collection by its exact href
// form or by the other trailing-slash form and records the exact request path.
func newWireProtocol(t *testing.T, scopes string, calendars ...wireCalendar) *wireProtocol {
	t.Helper()
	f := &wireProtocol{calendars: calendars}
	discovery := &redactionTestServer{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.EscapedPath())
		f.mu.Unlock()
		switch {
		case r.Method == "PROPFIND" && r.URL.Path == "/":
			discovery.ServeHTTP(w, r)
			return
		case r.Method == "PROPFIND" && r.URL.Path == redactionPrincipalPath:
			writeRedactionXML(w, `<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>`+
				redactionPrincipalPath+`</href><propstat><prop><C:calendar-home-set><href>`+wireHomePath+`</href>`+
				`</C:calendar-home-set></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
			return
		case r.Method == "PROPFIND" && r.URL.Path == wireHomePath:
			var rows strings.Builder
			for _, calendar := range f.calendars {
				rows.WriteString(`<response><href>` + calendar.href + `</href><propstat><prop>` +
					`<resourcetype><collection/><C:calendar/></resourcetype><displayname>` + calendar.name + `</displayname>` +
					`<C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>` +
					`</prop><status>HTTP/1.1 200 OK</status></propstat></response>`)
			}
			writeRedactionXML(w, `<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`+rows.String()+`</multistatus>`)
			return
		}
		for _, calendar := range f.calendars {
			switch {
			case r.Method == "REPORT" && wireCollection(r.URL.Path) == wireCollection(calendar.href):
				writeRedactionXML(w, `<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>`+
					wireResource(calendar.href)+`</href><propstat><prop><getetag>`+wireETag+`</getetag><C:calendar-data>`+
					wireXMLText(t, calendar.object)+`</C:calendar-data></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
				return
			case r.Method == http.MethodGet && r.URL.Path == wireResource(calendar.href):
				w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
				w.Header().Set("ETag", wireETag)
				_, _ = io.WriteString(w, calendar.object)
				return
			case r.Method == http.MethodPut && r.URL.Path == wireResource(calendar.href):
				f.puts.Add(1)
				f.mu.Lock()
				f.putBodies = append(f.putBodies, string(body))
				f.mu.Unlock()
				w.Header().Set("ETag", wireNewETag)
				w.WriteHeader(http.StatusNoContent)
				return
			case r.Method == http.MethodDelete && r.URL.Path == wireResource(calendar.href):
				f.deletes.Add(1)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := srv.Client()
	httpClient.Timeout = 30 * time.Second
	service := icloud.NewClient(httpClient, srv.URL, func(host string) bool { return host == base.Hostname() })
	redactor := security.NewRedactor("synthetic-wire-secret")
	s := server.NewMCPServer("calendar-wire", "test", server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(redactor)))
	var resourceScopes config.ResourceScopes
	if scopes != "" {
		resourceScopes = mustScopes(t, scopes)
	}
	Register(s, Deps{Service: service, Redactor: redactor, DefaultLocation: time.UTC, ResourceScopes: resourceScopes,
		Audit: security.NewAuditLogger(&f.audit), Idempotency: newIdempotencyStore()}, false)
	f.client, err = client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Close() })
	if err := f.client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := mcp.InitializeRequest{}
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "calendar-wire-test", Version: "test"}
	if _, err := f.client.Initialize(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return f
}

// call sends one tool call and returns the decoded payload and the error flag.
func (f *wireProtocol) call(t *testing.T, tool string, args map[string]any, target any) bool {
	t.Helper()
	request := mcp.CallToolRequest{}
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
	if err := json.Unmarshal([]byte(resultText(t, response)), target); err != nil {
		t.Fatalf("%s returned a non-JSON payload: %s", tool, resultText(t, response))
	}
	return response.IsError
}

func (f *wireProtocol) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *wireProtocol) requested(method, path string) int {
	count := 0
	for _, entry := range f.requestLog() {
		if entry == method+" "+path {
			count++
		}
	}
	return count
}

func wireSearchArgs(calendar string) map[string]any {
	return map[string]any{"calendar": calendar, "start": "2026-11-07T00:00:00Z", "end": "2026-11-09T00:00:00Z"}
}

func wireFreeSlotArgs(calendar string) map[string]any {
	return map[string]any{"calendar": calendar, "start": "2026-11-07T09:00:00Z", "end": "2026-11-07T12:00:00Z",
		"timezone": "UTC", "duration_minutes": 60}
}

func assertWirePrivate(t *testing.T, value any, private ...string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range private {
		if strings.Contains(string(encoded), needle) {
			t.Errorf("public output contains %q: %s", needle, encoded)
		}
	}
}

// A quoted parameter value followed by other text, or a parameter list that
// reaches the end of the line without a colon, is malformed iCalendar. Reads
// fail closed with a stable protocol_error that repeats no remote text, and no
// mutation dispatches a write.
func TestProtocolMalformedICalendarParameters(t *testing.T) {
	const calendarPath = "/wire/calendars/home/"
	for _, test := range []struct {
		name, property string
	}{
		{"attendee quoted value with trailing text", "ATTENDEE;X-PRIVATE=\"private-param\"tail:mailto:private-guest@example.invalid\n"},
		{"custom property quoted value with trailing text", "X-WIRE;X-PRIVATE=\"private-param\"tail:private-value\n"},
		{"parameter list without colon", "X-WIRE;X-PRIVATE=private-param\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWireProtocol(t, "", wireCalendar{href: calendarPath, name: "Home", object: wireObject(test.property)})
			for _, call := range []struct {
				tool string
				args map[string]any
			}{
				{"get_event", map[string]any{"calendar": calendarPath, "uid": wireUID}},
				{"search_events", wireSearchArgs(calendarPath)},
				{"search_events", map[string]any{"start": "2026-11-07T00:00:00Z", "end": "2026-11-09T00:00:00Z"}},
				{"find_free_slots", wireFreeSlotArgs(calendarPath)},
				{"find_free_slots", map[string]any{"start": "2026-11-07T09:00:00Z", "end": "2026-11-07T12:00:00Z",
					"timezone": "UTC", "duration_minutes": 60}},
				{"update_event", map[string]any{"calendar": calendarPath, "uid": wireUID, "title": "Changed",
					"etag": wireETag, "acknowledge_scheduling": true}},
				{"delete_event", map[string]any{"calendar": calendarPath, "uid": wireUID, "etag": wireETag,
					"acknowledge_scheduling": true}},
			} {
				var got toolErrorPayload
				if !f.call(t, call.tool, call.args, &got) {
					t.Fatalf("%s accepted malformed iCalendar: %+v", call.tool, got)
				}
				if got.Code != "protocol_error" || !strings.HasSuffix(got.Message, "protocol_error: Calendar event data is malformed") {
					t.Errorf("%s error = %+v, want the stable malformed-data protocol_error", call.tool, got)
				}
				assertWirePrivate(t, got, "private", "tail", "X-PRIVATE")
			}
			if f.puts.Load() != 0 || f.deletes.Load() != 0 {
				t.Fatalf("malformed iCalendar dispatched PUT=%d DELETE=%d", f.puts.Load(), f.deletes.Load())
			}
			if strings.Contains(f.audit.String(), "private") || strings.Contains(f.audit.String(), wireUID) {
				t.Errorf("audit exposed event data: %s", f.audit.String())
			}
		})
	}
}

// A calendar collection href keeps its exact form, with or without a trailing
// slash, in list_calendars, in every request path, and in search provenance.
// Resource scope matching compares the exact form. It never adds or removes a
// trailing slash.
func TestProtocolCalendarCollectionHrefForms(t *testing.T) {
	const slash = "/wire/calendars/slash/"
	const bare = "/wire/calendars/bare"
	calendars := []wireCalendar{
		{href: slash, name: "Slash", object: wireObject("")},
		{href: bare, name: "Bare", object: wireObject("")},
	}
	t.Run("unscoped", func(t *testing.T) {
		f := newWireProtocol(t, "", calendars...)
		var listed calendarsResponse
		if f.call(t, "list_calendars", map[string]any{}, &listed) {
			t.Fatalf("list_calendars failed: %+v", listed)
		}
		if listed.Count != 2 || listed.Calendars[0].Path != slash || listed.Calendars[1].Path != bare {
			t.Fatalf("list_calendars paths = %+v, want the exact href forms", listed.Calendars)
		}
		for _, path := range []string{slash, bare} {
			var page searchEventsResponse
			if f.call(t, "search_events", wireSearchArgs(path), &page) {
				t.Fatalf("search_events %s failed: %+v", path, page)
			}
			if page.Count != 1 || page.Events[0].Calendar != path {
				t.Errorf("search_events %s = %+v, want one event with exact provenance", path, page)
			}
			if f.requested("REPORT", path) != 1 {
				t.Errorf("REPORT path for %s was not the exact href form: %v", path, f.requestLog())
			}
			var detail getEventDTO
			if f.call(t, "get_event", map[string]any{"calendar": path, "uid": wireUID}, &detail) {
				t.Fatalf("get_event %s failed: %+v", path, detail)
			}
			if detail.UID != wireUID || detail.ETag != wireETag || f.requested(http.MethodGet, wireResource(path)) != 1 {
				t.Errorf("get_event %s = %+v, requests %v", path, detail, f.requestLog())
			}
		}
		var all searchEventsResponse
		if f.call(t, "search_events", map[string]any{"start": "2026-11-07T00:00:00Z", "end": "2026-11-09T00:00:00Z"}, &all) {
			t.Fatalf("all-calendar search failed: %+v", all)
		}
		if all.Count != 2 || all.Events[0].Calendar == all.Events[1].Calendar {
			t.Errorf("all-calendar search = %+v, want one event from each href form", all)
		}
		var free freeSlotsResponse
		if f.call(t, "find_free_slots", map[string]any{"calendars": slash + "," + bare, "start": "2026-11-07T09:00:00Z",
			"end": "2026-11-07T12:00:00Z", "timezone": "UTC", "duration_minutes": 60}, &free) {
			t.Fatalf("find_free_slots failed: %+v", free)
		}
		if free.Count != 2 || free.Slots[0].End != "2026-11-07T10:00:00+00:00" || free.Slots[1].Start != "2026-11-07T11:00:00+00:00" {
			t.Errorf("find_free_slots = %+v, want the 10:00-11:00 busy interval", free)
		}
	})
	t.Run("scope uses exact forms", func(t *testing.T) {
		f := newWireProtocol(t, "calendar.read="+slash+","+bare, calendars...)
		var listed calendarsResponse
		if f.call(t, "list_calendars", map[string]any{}, &listed) || listed.Count != 2 {
			t.Fatalf("scoped list_calendars = %+v, want both exact forms", listed)
		}
		var page searchEventsResponse
		if f.call(t, "search_events", map[string]any{"start": "2026-11-07T00:00:00Z", "end": "2026-11-09T00:00:00Z"}, &page) ||
			page.Count != 2 {
			t.Fatalf("scoped all-calendar search = %+v, want two events", page)
		}
	})
	t.Run("scope does not normalize slashes", func(t *testing.T) {
		f := newWireProtocol(t, "calendar.read="+wireCollection(slash)+","+bare+"/", calendars...)
		var listed calendarsResponse
		if f.call(t, "list_calendars", map[string]any{}, &listed) || listed.Count != 0 {
			t.Fatalf("scoped list_calendars = %+v, want no calendar for the other slash forms", listed)
		}
		for _, path := range []string{slash, bare} {
			for _, tool := range []string{"search_events", "get_event", "find_free_slots"} {
				args := wireSearchArgs(path)
				switch tool {
				case "get_event":
					args = map[string]any{"calendar": path, "uid": wireUID}
				case "find_free_slots":
					args = wireFreeSlotArgs(path)
				}
				var denied toolErrorPayload
				if !f.call(t, tool, args, &denied) || denied.Code != "authorization" {
					t.Errorf("%s %s = %+v, want an authorization error", tool, path, denied)
				}
			}
		}
		for _, entry := range f.requestLog() {
			if strings.HasPrefix(entry, "REPORT ") || strings.HasPrefix(entry, "GET ") {
				t.Errorf("out-of-scope call reached the server: %s", entry)
			}
		}
	})
}

// A write must not dispatch a line break inside a content line. MCP input has
// no path to a parameter value: the only written parameter is TZID, and it
// comes from a loaded time zone. Text values with a CR keep their current
// pre-dispatch rejection.
func TestProtocolCalendarWriteLineBreakInput(t *testing.T) {
	const calendarPath = "/wire/calendars/home/"
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"title with CR", map[string]any{"title": "private\rX-INJECTED:1"}},
		{"location with CR LF", map[string]any{"location": "private\r\nX-INJECTED:1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWireProtocol(t, "", wireCalendar{href: calendarPath, name: "Home", object: wireObject("")})
			args := map[string]any{"calendar": calendarPath, "uid": wireUID, "etag": wireETag}
			for key, value := range test.args {
				args[key] = value
			}
			var got toolErrorPayload
			if !f.call(t, "update_event", args, &got) {
				t.Fatalf("update_event accepted a CR in a text value: %+v", got)
			}
			t.Logf("characterized_error code=%q message=%q", got.Code, got.Message)
			assertWirePrivate(t, got, "private", "X-INJECTED")
			if f.puts.Load() != 0 {
				t.Fatalf("a CR in a text value dispatched %d PUT requests", f.puts.Load())
			}
		})
	}
	t.Run("notes with LF stay writable", func(t *testing.T) {
		f := newWireProtocol(t, "", wireCalendar{href: calendarPath, name: "Home", object: wireObject("")})
		var got updateEventResponse
		if f.call(t, "update_event", map[string]any{"calendar": calendarPath, "uid": wireUID, "etag": wireETag,
			"notes": "first\nsecond"}, &got) || !got.Success {
			t.Fatalf("update_event with a LF in notes failed: %+v", got)
		}
		f.mu.Lock()
		body := f.putBodies[0]
		f.mu.Unlock()
		if !strings.Contains(body, "DESCRIPTION:first\\nsecond\r\n") || strings.Count(body, "\r") != strings.Count(body, "\r\n") {
			t.Errorf("PUT body did not escape the LF as TEXT: %q", body)
		}
	})
}

// Contacts discovery keeps address-book collection hrefs opaque. A book href
// with or without a trailing slash gives a stable identifier, scope matching by
// name, and a REPORT on the exact href form.
func TestProtocolContactsAddressBookHrefForms(t *testing.T) {
	books := map[string]string{"/home/slash/": "Slash", "/home/bare": "Bare"}
	doer := &wireContactsDoer{books: books}
	service := contacts.NewClient(doer, "https://contacts.icloud.com/",
		func(host string) bool { return host == "contacts.icloud.com" })
	deps := contactsTestDeps(service, nil)
	list, err := listAddressBooksHandler(deps)(t.Context(), newReq(map[string]any{}))
	if err != nil || list.IsError {
		t.Fatalf("list_address_books: %v %s", err, resultText(t, list))
	}
	var listed contactsAddressBooksResponse
	decodeResult(t, list, &listed)
	if listed.Count != 2 {
		t.Fatalf("address books = %+v, want both href forms", listed)
	}
	identifiers := map[string]string{}
	for _, book := range listed.AddressBooks {
		identifiers[book.Name] = book.Identifier
	}
	assertWirePrivate(t, listed, "/home/")
	for name, path := range map[string]string{"Slash": "/home/slash/", "Bare": "/home/bare"} {
		search, err := searchContactsHandler(deps)(t.Context(), newReq(map[string]any{"address_book": identifiers[name]}))
		if err != nil || search.IsError {
			t.Fatalf("search_contacts %s: %v %s", name, err, resultText(t, search))
		}
		var result contacts.SearchResult
		decodeResult(t, search, &result)
		if len(result.Contacts) != 1 || result.Contacts[0].DisplayName != name+" Person" {
			t.Errorf("search_contacts %s = %+v", name, result.Contacts)
		}
		if doer.reportCount(path) != 1 {
			t.Errorf("REPORT for %s did not use the exact href form: %v", name, doer.reportPaths())
		}
	}
	scoped := contactsTestDeps(service, nil)
	scoped.ResourceScopes = mustScopes(t, "contacts.read=Bare")
	list, err = listAddressBooksHandler(scoped)(t.Context(), newReq(map[string]any{}))
	if err != nil || list.IsError {
		t.Fatalf("scoped list_address_books: %v %s", err, resultText(t, list))
	}
	decodeResult(t, list, &listed)
	if listed.Count != 1 || listed.AddressBooks[0].Identifier != identifiers["Bare"] {
		t.Errorf("scoped address books = %+v, want only Bare", listed)
	}
}

type wireContactsDoer struct {
	books map[string]string

	mu      sync.Mutex
	reports []string
}

func (d *wireContactsDoer) reportCount(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, entry := range d.reports {
		if entry == path {
			count++
		}
	}
	return count
}

func (d *wireContactsDoer) reportPaths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.reports...)
}

func (d *wireContactsDoer) Do(req *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	const multistatus = `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`
	switch {
	case req.Method == "PROPFIND" && req.URL.Path == "/":
		return paginationResponse(req, http.StatusMultiStatus, multistatus+`<D:response><D:href>/</D:href><D:propstat><D:prop>`+
			`<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal></D:prop>`+
			`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/principal/":
		return paginationResponse(req, http.StatusMultiStatus, multistatus+`<D:response><D:href>/principal/</D:href><D:propstat><D:prop>`+
			`<C:addressbook-home-set><D:href>/home/</D:href></C:addressbook-home-set></D:prop>`+
			`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/home/":
		var rows strings.Builder
		for _, href := range []string{"/home/slash/", "/home/bare"} {
			rows.WriteString(`<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` +
				`<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>` + d.books[href] + `</D:displayname>` +
				`<C:supported-address-data><C:address-data-type content-type="text/vcard" version="3.0"/></C:supported-address-data>` +
				`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		}
		return paginationResponse(req, http.StatusMultiStatus, multistatus+rows.String()+`</D:multistatus>`), nil
	case req.Method == "REPORT":
		name, ok := d.books[req.URL.Path]
		if !ok {
			return paginationResponse(req, http.StatusNotFound, ""), nil
		}
		d.mu.Lock()
		d.reports = append(d.reports, req.URL.Path)
		d.mu.Unlock()
		card := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:" + strings.ToLower(name) + "\r\nFN:" + name + " Person\r\nN:Person;" + name + ";;;\r\nEND:VCARD\r\n"
		href := strings.TrimSuffix(req.URL.Path, "/") + "/" + strings.ToLower(name) + ".vcf"
		return paginationResponse(req, http.StatusMultiStatus, multistatus+`<D:response><D:href>`+href+`</D:href><D:propstat><D:prop>`+
			`<D:getetag>&quot;e&quot;</D:getetag><C:address-data>`+paginationXML(card)+`</C:address-data></D:prop>`+
			`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`), nil
	}
	return paginationResponse(req, http.StatusNotFound, fmt.Sprintf("unexpected %s", req.Method)), nil
}
