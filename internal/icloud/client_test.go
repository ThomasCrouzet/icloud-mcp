package icloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Mocked CalDAV server (httptest) -----------------------------------
//
// Serves both the hand-rolled discovery requests (PROPFIND issued manually
// by icloud.Client) and the go-webdav/caldav client requests
// (REPORT/PUT/DELETE). A single TLS server plays the role of the main
// server AND of the "shard" (its own host), which is enough to exercise
// the discovery's absolute URL resolution logic.

const (
	testPrincipalPath = "/121234567/principal/"
	testHomeSetPath   = "/121234567/calendars/"
	testHomeCalendar  = "/121234567/calendars/home/"
)

type mockObject struct {
	path string
	ics  string

	// getIcs, if non-empty, is the body returned for a GET on path;
	// otherwise GET returns ics (default behavior: GET and REPORT serve the
	// same content). Used to simulate the real iCloud difference between a
	// filtered REPORT (bare VEVENT, without VERSION/PRODID/VTIMEZONE) and a
	// full GET (whole VCALENDAR).
	getIcs string
}

type mockCalDAV struct {
	t  *testing.T
	mu sync.Mutex

	authFail bool

	// principalHrefFunc builds the href returned for current-user-principal
	// (discovery step 1), relative by default (testPrincipalPath),
	// overridable to simulate a principal outside the allowlist.
	principalHrefFunc func(baseURL string) string

	// homeSetHrefFunc builds the href returned for calendar-home-set; it
	// receives the test server base URL to build an absolute href when
	// needed.
	homeSetHrefFunc func(baseURL string) string

	calendarsBody string // XML body returned for PROPFIND Depth 1 (list_calendars)

	objects map[string]mockObject // uid -> object (may contain several VEVENTs)

	principalCount int
	homeSetCount   int
	calendarsCount int

	lastReportBody []byte
	puts           []mockPut
	deletes        []string
	deleteIfMatch  []string
	gets           []string

	// etags maps an object path to its current ETag (quotes included, as
	// served in the ETag header). When a path has an entry, the mock
	// returns it on GET (so icloud.Client can do a conditional PUT) and
	// ENFORCES If-Match on PUT: a mismatch yields 412 Precondition Failed,
	// a match accepts the PUT and bumps the etag. Tests that do not populate
	// this map keep the legacy unconditional behavior (no ETag on GET, no
	// If-Match check), so existing fixtures are unchanged.
	etags map[string]string

	srv *httptest.Server
}

type mockPut struct {
	path    string
	body    string
	ifMatch string
}

func newMockCalDAV(t *testing.T) *mockCalDAV {
	t.Helper()
	m := &mockCalDAV{
		t:       t,
		objects: make(map[string]mockObject),
		etags:   make(map[string]string),
	}
	m.principalHrefFunc = func(string) string { return testPrincipalPath }
	m.homeSetHrefFunc = func(baseURL string) string { return baseURL + testHomeSetPath }
	m.srv = httptest.NewTLSServer(m)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockCalDAV) URL() string { return m.srv.URL }

// client builds an icloud.Client pointing at this server, with a permissive
// allowlist (the production allowlist is tested separately in
// internal/security).
func (m *mockCalDAV) client() *Client {
	authHTTP := basicAuthDoer{inner: m.srv.Client(), user: "user@example.com", pass: "app-password"}
	return NewClient(authHTTP, m.srv.URL, func(string) bool { return true })
}

// nextEtag synthesizes a deterministic new ETag value for a path after the nth
// PUT, so the mock's conditional-PUT logic can bump the ETag on each
// successful write and reject a stale If-Match.
func nextEtag(path string, n int) string {
	return fmt.Sprintf("v%d-%s", n, path)
}

// basicAuthDoer sets a Basic Authorization header, a minimal equivalent of
// webdav.HTTPClientWithBasicAuth so this file does not depend on the webdav
// package.
type basicAuthDoer struct {
	inner *http.Client
	user  string
	pass  string
}

func (d basicAuthDoer) Do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(d.user, d.pass)
	return d.inner.Do(req)
}

// spyDoer records each contacted host (Do) and blocks any request to a
// given host WITHOUT ever touching the real network; used to prove that a
// host outside the allowlist is NEVER contacted, including for discovery
// step 1 (principal), without depending on real network access (slow and
// nondeterministic) to an arbitrary host such as evil.example.com.
type spyDoer struct {
	inner          httpDoer
	blockedHost    string
	contactedHosts []string
}

func (s *spyDoer) Do(req *http.Request) (*http.Response, error) {
	s.contactedHosts = append(s.contactedHosts, req.URL.Hostname())
	if req.URL.Hostname() == s.blockedHost {
		return nil, fmt.Errorf("spyDoer: blocked host contacted (%s), should never happen if the allowlist is enforced before dispatch", s.blockedHost)
	}
	return s.inner.Do(req)
}

func (m *mockCalDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.authFail {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == "PROPFIND" && r.URL.Path == "/":
		m.principalCount++
		writeMultistatus(w, principalResponseXML(m.principalHrefFunc(m.srv.URL)))
	case r.Method == "PROPFIND" && r.URL.Path == testPrincipalPath:
		m.homeSetCount++
		writeMultistatus(w, homeSetResponseXML(m.homeSetHrefFunc(m.srv.URL)))
	case r.Method == "PROPFIND" && r.URL.Path == testHomeSetPath:
		m.calendarsCount++
		writeMultistatus(w, m.calendarsBody)
	case r.Method == "REPORT":
		body, _ := io.ReadAll(r.Body)
		m.lastReportBody = body
		m.handleReport(w, body)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		ifMatch := r.Header.Get("If-Match")
		m.puts = append(m.puts, mockPut{path: r.URL.Path, body: string(body), ifMatch: ifMatch})
		// If the path has a tracked ETag, enforce If-Match (conditional
		// PUT). A missing/empty If-Match on a tracked path is also a
		// 412 per RFC 7232 (the resource is not "unmapped"), but iCloud's
		// real behavior is to accept an unconditional PUT; the mock
		// mirrors iCloud and only rejects an If-Match that does not match.
		if current, ok := m.etags[r.URL.Path]; ok && ifMatch != "" && ifMatch != current {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		// Bump the ETag on a successful PUT.
		m.etags[r.URL.Path] = `"` + nextEtag(r.URL.Path, len(m.puts)) + `"`
		w.Header().Set("ETag", m.etags[r.URL.Path])
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		ifMatch := r.Header.Get("If-Match")
		m.deletes = append(m.deletes, r.URL.Path)
		m.deleteIfMatch = append(m.deleteIfMatch, ifMatch)
		if current, ok := m.etags[r.URL.Path]; ok && ifMatch != "" && ifMatch != current {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		// Remove object on successful delete.
		for uid, obj := range m.objects {
			if obj.path == r.URL.Path {
				delete(m.objects, uid)
				break
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet:
		m.gets = append(m.gets, r.URL.Path)
		m.handleGet(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleGet serves a direct GET on an object's path (used by UpdateEvent:
// GetCalendarObject before PUT, to retrieve the full VCALENDAR rather than
// the filtered data of a REPORT).
func (m *mockCalDAV) handleGet(w http.ResponseWriter, r *http.Request) {
	for _, obj := range m.objects {
		if obj.path != r.URL.Path {
			continue
		}
		body := obj.ics
		if obj.getIcs != "" {
			body = obj.getIcs
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		// When the path has a tracked ETag, return it so the client can do
		// a conditional PUT (If-Match). Paths without a tracked ETag serve
		// no ETag header (legacy unconditional behavior).
		if etag, ok := m.etags[obj.path]; ok {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// reportProbe decodes just enough of a REPORT calendar-query request body
// to distinguish a UID lookup (findEventByUID) from a time-range search
// (SearchEvents), and to extract the time-range bounds so tests can verify
// them.
type reportProbe struct {
	Filter struct {
		CompFilter struct {
			CompFilter struct {
				TimeRange *struct {
					Start string `xml:"start,attr"`
					End   string `xml:"end,attr"`
				} `xml:"time-range"`
				PropFilter *struct {
					Name      string `xml:"name,attr"`
					TextMatch string `xml:"text-match"`
				} `xml:"prop-filter"`
			} `xml:"comp-filter"`
		} `xml:"comp-filter"`
	} `xml:"filter"`
}

func (m *mockCalDAV) handleReport(w http.ResponseWriter, body []byte) {
	var probe reportProbe
	if err := xml.Unmarshal(body, &probe); err != nil {
		m.t.Fatalf("undecodable REPORT body: %v\n%s", err, body)
	}

	if pf := probe.Filter.CompFilter.CompFilter.PropFilter; pf != nil && pf.Name == "UID" {
		obj, ok := m.objects[pf.TextMatch]
		if !ok {
			writeMultistatus(w, emptyMultistatusXML())
			return
		}
		writeMultistatus(w, reportResponseXML(obj.path, obj.ics))
		return
	}

	// General (time-range) search: return every known object.
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
	for _, obj := range m.objects {
		sb.WriteString(reportResponseFragment(obj.path, obj.ics))
	}
	sb.WriteString(`</multistatus>`)
	writeMultistatus(w, sb.String())
}

func writeMultistatus(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_, _ = io.WriteString(w, body)
}

func principalResponseXML(href string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:">
  <response>
    <href>/</href>
    <propstat>
      <prop>
        <current-user-principal><href>` + href + `</href></current-user-principal>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`
}

func homeSetResponseXML(href string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <response>
    <href>` + testPrincipalPath + `</href>
    <propstat>
      <prop>
        <C:calendar-home-set><href>` + href + `</href></C:calendar-home-set>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`
}

func emptyMultistatusXML() string {
	return `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:"></multistatus>`
}

func reportResponseXML(path, ics string) string {
	return `<?xml version="1.0" encoding="utf-8"?><multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
		reportResponseFragment(path, ics) + `</multistatus>`
}

func reportResponseFragment(path, ics string) string {
	return reportResponseFragmentWithETag(path, ics, `"report-etag-1"`)
}

func reportResponseFragmentWithETag(path, ics, etag string) string {
	var esc strings.Builder
	_ = xml.EscapeText(&esc, []byte(ics))
	etagProp := ""
	if etag != "" {
		etagProp = "<getetag>" + etag + "</getetag>"
	}
	return fmt.Sprintf(`<response><href>%s</href><propstat><prop>%s<C:calendar-data>%s</C:calendar-data></prop><status>HTTP/1.1 200 OK</status></propstat></response>`, path, etagProp, esc.String())
}

// defaultCalendarsBody provides 1 regular calendar + inbox + outbox +
// a VTODO-only collection.
func defaultCalendarsBody() string {
	return `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:IC="http://apple.com/ns/ical/">
  <response>
    <href>` + testHomeSetPath + `</href>
    <propstat>
      <prop><resourcetype><collection/></resourcetype><displayname>root</displayname></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <response>
    <href>` + testHomeCalendar + `</href>
    <propstat>
      <prop>
        <resourcetype><collection/><C:calendar/></resourcetype>
        <displayname>Home</displayname>
        <C:calendar-description>Household calendar</C:calendar-description>
        <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
        <IC:calendar-color>#FF2968FF</IC:calendar-color>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <response>
    <href>/121234567/calendars/inbox/</href>
    <propstat>
      <prop><resourcetype><collection/><C:schedule-inbox/></resourcetype><displayname>inbox</displayname></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <response>
    <href>/121234567/calendars/outbox/</href>
    <propstat>
      <prop><resourcetype><collection/><C:schedule-outbox/></resourcetype><displayname>outbox</displayname></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <response>
    <href>/121234567/calendars/reminders/</href>
    <propstat>
      <prop>
        <resourcetype><collection/><C:calendar/></resourcetype>
        <displayname>Reminders</displayname>
        <C:supported-calendar-component-set><C:comp name="VTODO"/></C:supported-calendar-component-set>
      </prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
</multistatus>`
}

// --- ICS fixtures ---------------------------------------------------------

const icsSimpleEvent = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-simple-1\r\n" +
	"SUMMARY:Team meeting\r\n" +
	"LOCATION:Room A\r\n" +
	"DESCRIPTION:Weekly sync\r\n" +
	"DTSTART:20260706T090000Z\r\n" +
	"DTEND:20260706T100000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const icsRecurringWithExdate = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-recur-1\r\n" +
	"SUMMARY:Gym class\r\n" +
	"DTSTART:20260706T180000Z\r\n" +
	"DTEND:20260706T190000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=5\r\n" +
	"EXDATE:20260713T180000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const icsOverride = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-override-1\r\n" +
	"SUMMARY:Project review\r\n" +
	"DTSTART:20260706T140000Z\r\n" +
	"DTEND:20260706T150000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-override-1\r\n" +
	"RECURRENCE-ID:20260713T140000Z\r\n" +
	"SUMMARY:Project review (moved)\r\n" +
	"DTSTART:20260713T160000Z\r\n" +
	"DTEND:20260713T170000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const icsAllDay = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-allday-1\r\n" +
	"SUMMARY:Birthday\r\n" +
	"DTSTART;VALUE=DATE:20260710\r\n" +
	"DTEND;VALUE=DATE:20260711\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsNoDtendWithDuration: DTEND absent, DURATION present (a valid
// alternative per RFC 5545 section 3.6.1). EndTime must be derived from
// StartTime+DURATION.
const icsNoDtendWithDuration = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-duration-1\r\n" +
	"SUMMARY:Meeting without DTEND\r\n" +
	"DTSTART:20260706T090000Z\r\n" +
	"DURATION:PT1H\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsAllDayNoDtend: all-day event (DTSTART;VALUE=DATE) with neither DTEND
// nor DURATION. EndTime must be derived as StartTime+24h.
const icsAllDayNoDtend = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-allday-nodtend-1\r\n" +
	"SUMMARY:Birthday without DTEND\r\n" +
	"DTSTART;VALUE=DATE:20260710\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsFilteredReportOnly reproduces iCloud's real behavior on a REPORT
// calendar-query: the returned calendar-data contains ONLY the requested
// VEVENT, without VERSION/PRODID (VCALENDAR level) or VTIMEZONE, even if
// the object stored server-side has them. A direct go-ical.Encode of this
// data fails ("want exactly one PRODID property, got 0").
const icsFilteredReportOnly = "BEGIN:VCALENDAR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-tz-1\r\n" +
	"SUMMARY:NY meeting\r\n" +
	"DTSTART;TZID=America/New_York:20261102T100000\r\n" +
	"DTEND;TZID=America/New_York:20261102T110000\r\n" +
	"DTSTAMP:20261001T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsFullGetVersion is what a direct GET on the same object returns from
// iCloud: the full VCALENDAR, with VERSION/PRODID and the VTIMEZONE
// required by the DTSTART/DTEND in TZID=America/New_York.
const icsFullGetVersion = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VTIMEZONE\r\n" +
	"TZID:America/New_York\r\n" +
	"BEGIN:STANDARD\r\n" +
	"DTSTART:19701101T020000\r\n" +
	"TZOFFSETFROM:-0400\r\n" +
	"TZOFFSETTO:-0500\r\n" +
	"END:STANDARD\r\n" +
	"BEGIN:DAYLIGHT\r\n" +
	"DTSTART:19700308T020000\r\n" +
	"TZOFFSETFROM:-0500\r\n" +
	"TZOFFSETTO:-0400\r\n" +
	"END:DAYLIGHT\r\n" +
	"END:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-tz-1\r\n" +
	"SUMMARY:NY meeting\r\n" +
	"DTSTART;TZID=America/New_York:20261102T100000\r\n" +
	"DTEND;TZID=America/New_York:20261102T110000\r\n" +
	"DTSTAMP:20261001T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// --- Tests: discovery ----------------------------------------------------

func TestClient_Discover_HappyPath(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	if err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if c.shardBase != m.URL() {
		t.Errorf("shardBase = %q, want %q", c.shardBase, m.URL())
	}
	if c.homeSetPath != testHomeSetPath {
		t.Errorf("homeSetPath = %q, want %q", c.homeSetPath, testHomeSetPath)
	}
}

func TestClient_Discover_CachedAcrossConcurrentCalls(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Discover(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Discover() goroutine %d error: %v", i, err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.principalCount != 1 {
		t.Errorf("principalCount = %d, want 1 (sync.Once)", m.principalCount)
	}
	if m.homeSetCount != 1 {
		t.Errorf("homeSetCount = %d, want 1 (sync.Once)", m.homeSetCount)
	}
}

func TestClient_Discover_AuthFailure(t *testing.T) {
	m := newMockCalDAV(t)
	m.authFail = true
	c := m.client()

	err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("expected: authentication error")
	}
	if !strings.Contains(err.Error(), "authentication") {
		t.Errorf("expected error mentioning 'authentication', got: %v", err)
	}
}

func TestClient_Discover_ShardOutsideAllowlist(t *testing.T) {
	m := newMockCalDAV(t)
	m.homeSetHrefFunc = func(string) string { return "https://evil.example.com/121234567/calendars/" }

	authHTTP := basicAuthDoer{inner: m.srv.Client(), user: "u@x.com", pass: "app-password"}
	// Real production allowlist: evil.example.com must be rejected.
	c := NewClient(authHTTP, m.URL(), func(host string) bool {
		return host == "caldav.icloud.com" || strings.HasSuffix(host, "-caldav.icloud.com")
	})

	err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("expected: home-set outside allowlist error")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("expected error mentioning 'allowlist', got: %v", err)
	}
}

// TestClient_Discover_PrincipalOutsideAllowlist. The principal (discovery
// step 1, current-user-principal) must be revalidated (https + allowHost)
// just like the home-set (step 3, shard); at some point only the home-set
// was explicitly revalidated, and a hostile principal was only caught in
// production by the downstream AllowlistTransport (an inconsistency).
// spyDoer proves, WITHOUT real network access, that a principal outside
// the allowlist is NEVER contacted: validation must happen before any
// network dispatch, not merely be caught lower in the stack.
func TestClient_Discover_PrincipalOutsideAllowlist(t *testing.T) {
	m := newMockCalDAV(t)
	m.principalHrefFunc = func(string) string { return "https://evil.example.com/121234567/principal/" }

	spy := &spyDoer{
		inner:       basicAuthDoer{inner: m.srv.Client(), user: "u@x.com", pass: "app-password"},
		blockedHost: "evil.example.com",
	}
	// Real production allowlist: evil.example.com must be rejected.
	c := NewClient(spy, m.URL(), func(host string) bool {
		return host == "caldav.icloud.com" || strings.HasSuffix(host, "-caldav.icloud.com")
	})

	err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("expected: principal outside allowlist error")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("expected error mentioning 'allowlist', got: %v", err)
	}
	for _, h := range spy.contactedHosts {
		if h == "evil.example.com" {
			t.Errorf("evil.example.com should never have been contacted: contactedHosts=%v", spy.contactedHosts)
		}
	}
}

func TestClient_Discover_RelativeHomeSetHref(t *testing.T) {
	m := newMockCalDAV(t)
	m.homeSetHrefFunc = func(string) string { return testHomeSetPath } // relative
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	if err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if c.shardBase != m.URL() {
		t.Errorf("shardBase = %q, want %q (baseURL fallback for relative href)", c.shardBase, m.URL())
	}
}

// TestClient_Discover_RejectsOversizedPropfindResponse. An abnormally large
// PROPFIND response (buggy or hostile server) must be rejected rather than
// fully loaded into memory through an unbounded io.ReadAll.
func TestClient_Discover_RejectsOversizedPropfindResponse(t *testing.T) {
	huge := make([]byte, (8<<20)+1) // 8 MiB + 1 byte, past the defensive bound.
	for i := range huge {
		huge[i] = ' '
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		_, _ = w.Write(huge)
	}))
	defer srv.Close()

	authHTTP := basicAuthDoer{inner: srv.Client(), user: "u@x.com", pass: "app-password"}
	c := NewClient(authHTTP, srv.URL, func(string) bool { return true })

	err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("expected: oversized PROPFIND response error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected error mentioning 'too large', got: %v", err)
	}
}

// --- Tests: ListCalendars --------------------------------------------------

func TestClient_ListCalendars_FiltersTechnicalCalendars(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	cals, err := c.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars() error: %v", err)
	}
	if len(cals) != 1 {
		t.Fatalf("ListCalendars() = %d calendars, want 1 (inbox/outbox/VTODO-only filtered): %+v", len(cals), cals)
	}
	got := cals[0]
	if got.Path != testHomeCalendar {
		t.Errorf("Path = %q, want %q", got.Path, testHomeCalendar)
	}
	if got.Name != "Home" {
		t.Errorf("Name = %q, want %q", got.Name, "Home")
	}
	if got.Description != "Household calendar" {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Color != "#FF2968FF" {
		t.Errorf("Color = %q, want #FF2968FF", got.Color)
	}
}

// --- Tests: SearchEvents ----------------------------------------------------

func TestClient_SearchEvents_TimeRangeTransmitted(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	m.objects["uid-simple-1"] = mockObject{path: testHomeCalendar + "uid-simple-1.ics", ics: icsSimpleEvent}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	res, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() error: %v", err)
	}
	events := res.Events
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d events, want 1: %+v", len(events), events)
	}
	if events[0].Title != "Team meeting" {
		t.Errorf("Title = %q", events[0].Title)
	}

	m.mu.Lock()
	body := string(m.lastReportBody)
	m.mu.Unlock()
	if !strings.Contains(body, `start="20260701T000000Z"`) {
		t.Errorf("expected REPORT body containing start=\"20260701T000000Z\", got: %s", body)
	}
	if !strings.Contains(body, `end="20260708T000000Z"`) {
		t.Errorf("expected REPORT body containing end=\"20260708T000000Z\", got: %s", body)
	}
}

func TestClient_SearchEvents_AllDay(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-allday-1"] = mockObject{path: testHomeCalendar + "uid-allday-1.ics", ics: icsAllDay}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	res, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() error: %v", err)
	}
	events := res.Events
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d events, want 1", len(events))
	}
	if !events[0].AllDay {
		t.Errorf("AllDay = false, want true for uid-allday-1")
	}
}

// TestClient_SearchEvents_NoDtendDerivedFromDuration. A VEVENT without
// DTEND but with DURATION must have EndTime = StartTime + DURATION (and
// therefore appear in a search overlapping that slot).
func TestClient_SearchEvents_NoDtendDerivedFromDuration(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-duration-1"] = mockObject{path: testHomeCalendar + "uid-duration-1.ics", ics: icsNoDtendWithDuration}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	res, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() error: %v", err)
	}
	events := res.Events
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d events, want 1 (DTEND derived from DURATION): %+v", len(events), events)
	}
	wantStart := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	wantEnd := wantStart.Add(time.Hour)
	if !events[0].StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", events[0].StartTime, wantStart)
	}
	if !events[0].EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v (StartTime + DURATION PT1H)", events[0].EndTime, wantEnd)
	}
}

// TestClient_SearchEvents_AllDayNoDtendDefaultsTo24h. An all-day VEVENT
// with neither DTEND nor DURATION must have EndTime = StartTime + 24h, and
// therefore appear in a search overlapping that day.
func TestClient_SearchEvents_AllDayNoDtendDefaultsTo24h(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-allday-nodtend-1"] = mockObject{path: testHomeCalendar + "uid-allday-nodtend-1.ics", ics: icsAllDayNoDtend}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	res, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() error: %v", err)
	}
	events := res.Events
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d events, want 1 (DTEND derived as StartTime+24h): %+v", len(events), events)
	}
	wantStart := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	wantEnd := wantStart.Add(24 * time.Hour)
	if !events[0].StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", events[0].StartTime, wantStart)
	}
	if !events[0].EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v (StartTime + 24h, all-day)", events[0].EndTime, wantEnd)
	}
	if !events[0].AllDay {
		t.Errorf("AllDay = false, want true")
	}
}

func TestClient_SearchEvents_RecurringWithExdateAndOverride(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-recur-1"] = mockObject{path: testHomeCalendar + "uid-recur-1.ics", ics: icsRecurringWithExdate}
	m.objects["uid-override-1"] = mockObject{path: testHomeCalendar + "uid-override-1.ics", ics: icsOverride}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	res, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() error: %v", err)
	}
	events := res.Events

	// uid-recur-1: 5 weekly occurrences, 1 excluded (EXDATE), so 4.
	// uid-override-1: 4 weekly occurrences, the 2nd replaced by the override.
	if len(events) != 8 {
		t.Fatalf("SearchEvents() = %d events, want 8: %+v", len(events), events)
	}

	var overrideFound bool
	for _, e := range events {
		if e.UID == "uid-override-1" && e.Title == "Project review (moved)" {
			overrideFound = true
			if e.StartTime.Hour() != 16 {
				t.Errorf("override StartTime hour = %d, want 16 (moved)", e.StartTime.Hour())
			}
		}
		if e.UID == "uid-recur-1" && e.StartTime.Day() == 13 {
			t.Errorf("the July 13 occurrence should not appear (EXDATE)")
		}
	}
	if !overrideFound {
		t.Errorf("RECURRENCE-ID override not found in results: %+v", events)
	}
}

// --- Tests: CreateEvent ----------------------------------------------------

func TestClient_CreateEvent(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()

	uid, err := c.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title:              "New event",
		Location:           "Office",
		Notes:              "Test notes",
		StartTime:          time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC),
		EndTime:            time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC),
		AlarmMinutesBefore: 15,
	})
	if err != nil {
		t.Fatalf("CreateEvent() error: %v", err)
	}

	matched, merr := regexpUIDFormat(uid)
	if merr != nil || !matched {
		t.Errorf("generated UID %q does not match the expected format ^[0-9a-f]{32}@icloud-mcp$", uid)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(m.puts))
	}
	put := m.puts[0]
	if !strings.HasPrefix(put.path, testHomeCalendar) || !strings.HasSuffix(put.path, uid+".ics") {
		t.Errorf("PUT path = %q, want prefix %q and suffix %q", put.path, testHomeCalendar, uid+".ics")
	}
	for _, want := range []string{"SUMMARY:New event", "DTSTART", "DTEND", "UID:" + uid, "TRIGGER:-PT15M"} {
		if !strings.Contains(put.body, want) {
			t.Errorf("expected PUT body containing %q, got:\n%s", want, put.body)
		}
	}
}

func regexpUIDFormat(uid string) (bool, error) {
	if !strings.HasSuffix(uid, "@icloud-mcp") {
		return false, nil
	}
	hexPart := strings.TrimSuffix(uid, "@icloud-mcp")
	if len(hexPart) != 32 {
		return false, nil
	}
	for _, r := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false, nil
		}
	}
	return true, nil
}

// --- Tests: UpdateEvent ----------------------------------------------------

func TestClient_UpdateEvent_ChangeAndClearFields(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "some-other-filename.ics" // file name deliberately != UID
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	c := m.client()

	newTitle := "Updated title"
	emptyLocation := ""
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{
		Title:    &newTitle,
		Location: &emptyLocation, // clearing
		// Notes: nil means unchanged
	})
	if err != nil {
		t.Fatalf("UpdateEvent() error: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(m.puts))
	}
	put := m.puts[0]
	if put.path != objPath {
		t.Errorf("PUT path = %q, want %q (the real path, not <uid>.ics)", put.path, objPath)
	}
	if !strings.Contains(put.body, "SUMMARY:Updated title") {
		t.Errorf("PUT body should contain the new title: %s", put.body)
	}
	if strings.Contains(put.body, "LOCATION:") {
		t.Errorf("LOCATION should have been cleared: %s", put.body)
	}
	if !strings.Contains(put.body, "DESCRIPTION:Weekly sync") {
		t.Errorf("expected unchanged DESCRIPTION (notes): %s", put.body)
	}
	if !strings.Contains(put.body, "SEQUENCE:1") {
		t.Errorf("expected SEQUENCE incremented to 1: %s", put.body)
	}
}

func TestClient_UpdateEvent_UIDNotFound(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()

	newTitle := "x"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "unknown-uid", &EventUpdate{Title: &newTitle})
	if err == nil {
		t.Fatal("expected: event not found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error mentioning 'not found', got: %v", err)
	}
}

func TestClient_UpdateEvent_PreservesAllDayFormat(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-allday-1.ics"
	m.objects["uid-allday-1"] = mockObject{path: objPath, ics: icsAllDay}
	c := m.client()

	// icsAllDay runs July 10 to 11 (exclusive DTEND); move the start to
	// July 9 to stay before the existing end.
	newStart := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-allday-1", &EventUpdate{StartTime: &newStart})
	if err != nil {
		t.Fatalf("UpdateEvent() error: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	put := m.puts[0]
	if !strings.Contains(put.body, "DTSTART;VALUE=DATE:20260709") && !strings.Contains(put.body, "DTSTART:20260709") {
		t.Errorf("DTSTART should stay in date-only format (8 characters): %s", put.body)
	}
	if strings.Contains(put.body, "DTSTART") && strings.Contains(put.body, "T000000") {
		t.Errorf("DTSTART should not have been converted to a datetime: %s", put.body)
	}
}

// TestClient_UpdateEvent_UsesGETNotFilteredREPORTData. The REPORT
// (findEventByUID) returns FILTERED calendar-data (bare VEVENT, without
// VERSION/PRODID/VTIMEZONE), just like the real iCloud under a
// calendar-query filter. UpdateEvent MUST re-read the full object via GET
// (GetCalendarObject) before modifying and PUTting it, otherwise
// go-ical.Encode fails (missing VERSION/PRODID) or the event's VTIMEZONE
// is lost in the round-trip.
func TestClient_UpdateEvent_UsesGETNotFilteredREPORTData(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-tz-1.ics"
	m.objects["uid-tz-1"] = mockObject{
		path:   objPath,
		ics:    icsFilteredReportOnly, // REPORT response: filtered
		getIcs: icsFullGetVersion,     // GET response: full
	}
	c := m.client()

	newTitle := "NY meeting (updated)"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-tz-1", &EventUpdate{Title: &newTitle})
	if err != nil {
		t.Fatalf("UpdateEvent() error: %v (should re-read the full object via GET): %v", err, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.gets) != 1 {
		t.Fatalf("expected 1 GET (GetCalendarObject), got %d: %v", len(m.gets), m.gets)
	}
	if len(m.puts) != 1 {
		t.Fatalf("expected 1 PUT, got %d", len(m.puts))
	}
	put := m.puts[0]
	if put.path != objPath {
		t.Errorf("PUT path = %q, want %q", put.path, objPath)
	}
	for _, want := range []string{"VERSION:2.0", "PRODID:", "BEGIN:VTIMEZONE", "TZID:America/New_York", "SUMMARY:" + newTitle} {
		if !strings.Contains(put.body, want) {
			t.Errorf("expected PUT body containing %q (proof of a full GET read, not filtered REPORT data), got:\n%s", want, put.body)
		}
	}
}

// --- Tests: DeleteEvent ----------------------------------------------------

func TestClient_DeleteEvent_ReturnsTitleAndDeletesRealPath(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "old-imported-file.ics" // != uid-simple-1.ics
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	c := m.client()

	res, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-simple-1", nil)
	if err != nil {
		t.Fatalf("DeleteEvent() error: %v", err)
	}
	if res.Title != "Team meeting" {
		t.Errorf("title = %q, want %q", res.Title, "Team meeting")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deletes) != 1 || m.deletes[0] != objPath {
		t.Errorf("deletes = %v, want [%q]", m.deletes, objPath)
	}
}

func TestClient_DeleteEvent_UIDNotFound(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()

	_, err := c.DeleteEvent(context.Background(), testHomeCalendar, "unknown-uid", nil)
	if err == nil {
		t.Fatal("expected: event not found error")
	}
}
