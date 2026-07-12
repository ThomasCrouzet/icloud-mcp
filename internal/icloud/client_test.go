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

// --- Serveur CalDAV mocké (httptest) -----------------------------------
//
// Sert à la fois les requêtes de la découverte maison (PROPFIND fait à la
// main par icloud.Client) et les requêtes du client go-webdav/caldav
// (REPORT/PUT/DELETE). Un seul serveur TLS joue le rôle du serveur
// principal ET du "shard" (son propre host), ce qui suffit à exercer la
// logique de résolution d'URL absolue de la découverte (§5 du blueprint).

const (
	testPrincipalPath = "/121234567/principal/"
	testHomeSetPath   = "/121234567/calendars/"
	testHomeCalendar  = "/121234567/calendars/home/"
)

type mockObject struct {
	path string
	ics  string

	// getIcs, si non vide, est le corps renvoyé pour un GET sur path, sinon
	// GET renvoie ics (comportement par défaut : GET et REPORT servent le
	// même contenu). Utilisé pour simuler la différence réelle iCloud entre
	// un REPORT filtré (VEVENT nu, sans VERSION/PRODID/VTIMEZONE) et un GET
	// complet (VCALENDAR entier), voir FIX-B.
	getIcs string
}

type mockCalDAV struct {
	t  *testing.T
	mu sync.Mutex

	authFail bool

	// principalHrefFunc construit le href renvoyé pour current-user-principal
	// (étape 1 de la découverte), par défaut relatif (testPrincipalPath),
	// surchargeable pour simuler un principal hors allowlist (FIX-2).
	principalHrefFunc func(baseURL string) string

	// homeSetHrefFunc construit le href renvoyé pour calendar-home-set,
	// reçoit l'URL de base du serveur de test pour construire un href
	// absolu si besoin.
	homeSetHrefFunc func(baseURL string) string

	calendarsBody string // corps XML renvoyé pour PROPFIND Depth 1 (list_calendars)

	objects map[string]mockObject // uid -> objet (peut contenir plusieurs VEVENT)

	principalCount int
	homeSetCount   int
	calendarsCount int

	lastReportBody []byte
	puts           []mockPut
	deletes        []string
	gets           []string

	srv *httptest.Server
}

type mockPut struct {
	path string
	body string
}

func newMockCalDAV(t *testing.T) *mockCalDAV {
	t.Helper()
	m := &mockCalDAV{
		t:       t,
		objects: make(map[string]mockObject),
	}
	m.principalHrefFunc = func(string) string { return testPrincipalPath }
	m.homeSetHrefFunc = func(baseURL string) string { return baseURL + testHomeSetPath }
	m.srv = httptest.NewTLSServer(m)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockCalDAV) URL() string { return m.srv.URL }

// client construit un icloud.Client pointant vers ce serveur, avec une
// allowlist permissive (l'allowlist de production est testée séparément
// dans internal/security).
func (m *mockCalDAV) client() *Client {
	authHTTP := basicAuthDoer{inner: m.srv.Client(), user: "user@example.com", pass: "app-password"}
	return NewClient(authHTTP, m.srv.URL, func(string) bool { return true })
}

// basicAuthDoer pose un header Authorization Basic, équivalent minimal de
// webdav.HTTPClientWithBasicAuth pour ne pas dépendre du package webdav ici.
type basicAuthDoer struct {
	inner *http.Client
	user  string
	pass  string
}

func (d basicAuthDoer) Do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(d.user, d.pass)
	return d.inner.Do(req)
}

// spyDoer enregistre chaque host contacté (Do) et bloque toute requête vers
// un host donné SANS jamais faire de vrai réseau, utilisé pour prouver
// qu'un host hors allowlist n'est JAMAIS contacté (FIX-2), y compris pour
// l'étape 1 (principal) de la découverte, sans dépendre d'un accès réseau
// réel (lent et non déterministe) vers un host arbitraire type
// evil.example.com.
type spyDoer struct {
	inner          httpDoer
	blockedHost    string
	contactedHosts []string
}

func (s *spyDoer) Do(req *http.Request) (*http.Response, error) {
	s.contactedHosts = append(s.contactedHosts, req.URL.Hostname())
	if req.URL.Hostname() == s.blockedHost {
		return nil, fmt.Errorf("spyDoer : host bloqué contacté (%s), ne devrait jamais arriver si l'allowlist est appliquée avant dispatch", s.blockedHost)
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
		m.puts = append(m.puts, mockPut{path: r.URL.Path, body: string(body)})
		w.Header().Set("ETag", `"etag-1"`)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		m.deletes = append(m.deletes, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet:
		m.gets = append(m.gets, r.URL.Path)
		m.handleGet(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleGet sert un GET direct sur le path d'un objet (utilisé par
// UpdateEvent depuis FIX-B : GetCalendarObject avant PUT, pour récupérer le
// VCALENDAR complet plutôt que les données filtrées d'un REPORT).
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
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// reportProbe décode juste assez du corps d'une requête REPORT
// calendar-query pour distinguer une recherche par UID (findEventByUID)
// d'une recherche par plage temporelle (SearchEvents), et pour extraire les
// bornes time-range afin de les vérifier dans les tests.
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
		m.t.Fatalf("REPORT body non décodable : %v\n%s", err, body)
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

	// Recherche générale (time-range) : renvoyer tous les objets connus.
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
	var esc strings.Builder
	_ = xml.EscapeText(&esc, []byte(ics))
	return fmt.Sprintf(`<response><href>%s</href><propstat><prop><C:calendar-data>%s</C:calendar-data></prop><status>HTTP/1.1 200 OK</status></propstat></response>`, path, esc.String())
}

// defaultCalendarsBody fournit 1 calendrier normal + inbox + outbox +
// VTODO-only, cf. blueprint §7.2.
func defaultCalendarsBody() string {
	return `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:IC="http://apple.com/ns/ical/">
  <response>
    <href>` + testHomeSetPath + `</href>
    <propstat>
      <prop><resourcetype><collection/></resourcetype><displayname>racine</displayname></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <response>
    <href>` + testHomeCalendar + `</href>
    <propstat>
      <prop>
        <resourcetype><collection/><C:calendar/></resourcetype>
        <displayname>Maison</displayname>
        <C:calendar-description>Calendrier de la maison</C:calendar-description>
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

// --- Fixtures ICS ---------------------------------------------------------

const icsSimpleEvent = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-simple-1\r\n" +
	"SUMMARY:Reunion equipe\r\n" +
	"LOCATION:Salle A\r\n" +
	"DESCRIPTION:Point hebdo\r\n" +
	"DTSTART:20260706T090000Z\r\n" +
	"DTEND:20260706T100000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const icsRecurringWithExdate = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-recur-1\r\n" +
	"SUMMARY:Cours de sport\r\n" +
	"DTSTART:20260706T180000Z\r\n" +
	"DTEND:20260706T190000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=5\r\n" +
	"EXDATE:20260713T180000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const icsOverride = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-override-1\r\n" +
	"SUMMARY:Suivi projet\r\n" +
	"DTSTART:20260706T140000Z\r\n" +
	"DTEND:20260706T150000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-override-1\r\n" +
	"RECURRENCE-ID:20260713T140000Z\r\n" +
	"SUMMARY:Suivi projet (deplace)\r\n" +
	"DTSTART:20260713T160000Z\r\n" +
	"DTEND:20260713T170000Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

const icsAllDay = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-allday-1\r\n" +
	"SUMMARY:Anniversaire\r\n" +
	"DTSTART;VALUE=DATE:20260710\r\n" +
	"DTEND;VALUE=DATE:20260711\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsNoDtendWithDuration, FIX-6 : DTEND absent, DURATION présente
// (alternative valide RFC 5545 §3.6.1). EndTime doit être dérivée de
// StartTime+DURATION.
const icsNoDtendWithDuration = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-duration-1\r\n" +
	"SUMMARY:Reunion sans DTEND\r\n" +
	"DTSTART:20260706T090000Z\r\n" +
	"DURATION:PT1H\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsAllDayNoDtend, FIX-6 : événement all-day (DTSTART;VALUE=DATE) sans
// DTEND ni DURATION. EndTime doit être dérivée à StartTime+24h.
const icsAllDayNoDtend = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-allday-nodtend-1\r\n" +
	"SUMMARY:Anniversaire sans DTEND\r\n" +
	"DTSTART;VALUE=DATE:20260710\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsFilteredReportOnly reproduit le comportement réel d'iCloud sur un
// REPORT calendar-query : le calendar-data retourné ne contient QUE le
// VEVENT demandé, sans VERSION/PRODID (niveau VCALENDAR) ni VTIMEZONE, même
// si l'objet stocké côté serveur les a. Un go-ical.Encode direct de ces
// données échoue ("want exactly one PRODID property, got 0").
const icsFilteredReportOnly = "BEGIN:VCALENDAR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-tz-1\r\n" +
	"SUMMARY:Reunion NY\r\n" +
	"DTSTART;TZID=America/New_York:20261102T100000\r\n" +
	"DTEND;TZID=America/New_York:20261102T110000\r\n" +
	"DTSTAMP:20261001T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// icsFullGetVersion est ce qu'un GET direct sur le même objet renvoie chez
// iCloud : le VCALENDAR complet, avec VERSION/PRODID et le VTIMEZONE requis
// par le DTSTART/DTEND en TZID=America/New_York.
const icsFullGetVersion = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//FR\r\n" +
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
	"SUMMARY:Reunion NY\r\n" +
	"DTSTART;TZID=America/New_York:20261102T100000\r\n" +
	"DTEND;TZID=America/New_York:20261102T110000\r\n" +
	"DTSTAMP:20261001T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// --- Tests : découverte ----------------------------------------------------

func TestClient_Discover_HappyPath(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	if err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover() erreur : %v", err)
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
			t.Fatalf("Discover() goroutine %d erreur : %v", i, err)
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
		t.Fatal("attendu : erreur d'authentification")
	}
	if !strings.Contains(err.Error(), "authentification") {
		t.Errorf("erreur attendue mentionnant 'authentification', obtenu : %v", err)
	}
}

func TestClient_Discover_ShardOutsideAllowlist(t *testing.T) {
	m := newMockCalDAV(t)
	m.homeSetHrefFunc = func(string) string { return "https://evil.example.com/121234567/calendars/" }

	authHTTP := basicAuthDoer{inner: m.srv.Client(), user: "u@x.com", pass: "app-password"}
	// Allowlist de production réelle : evil.example.com doit être rejeté.
	c := NewClient(authHTTP, m.URL(), func(host string) bool {
		return host == "caldav.icloud.com" || strings.HasSuffix(host, "-caldav.icloud.com")
	})

	err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("attendu : erreur home-set hors allowlist")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("erreur attendue mentionnant 'allowlist', obtenu : %v", err)
	}
}

// TestClient_Discover_PrincipalOutsideAllowlist, FIX-2. Le principal
// (étape 1 de la découverte, current-user-principal) doit être revalidé
// (https + allowHost) au même titre que le home-set (étape 3, shard),
// aujourd'hui seul le home-set est explicitement revalidé ; le principal ne
// l'est pas et n'est rattrapé qu'en production par l'AllowlistTransport en
// aval (incohérence). spyDoer prouve, SANS accès réseau réel, qu'un
// principal hors allowlist n'est JAMAIS contacté : la validation doit
// intervenir avant tout dispatch réseau, pas seulement être rattrapée plus
// bas dans la pile.
func TestClient_Discover_PrincipalOutsideAllowlist(t *testing.T) {
	m := newMockCalDAV(t)
	m.principalHrefFunc = func(string) string { return "https://evil.example.com/121234567/principal/" }

	spy := &spyDoer{
		inner:       basicAuthDoer{inner: m.srv.Client(), user: "u@x.com", pass: "app-password"},
		blockedHost: "evil.example.com",
	}
	// Allowlist de production réelle : evil.example.com doit être rejeté.
	c := NewClient(spy, m.URL(), func(host string) bool {
		return host == "caldav.icloud.com" || strings.HasSuffix(host, "-caldav.icloud.com")
	})

	err := c.Discover(context.Background())
	if err == nil {
		t.Fatal("attendu : erreur principal hors allowlist")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("erreur attendue mentionnant 'allowlist', obtenu : %v", err)
	}
	for _, h := range spy.contactedHosts {
		if h == "evil.example.com" {
			t.Errorf("evil.example.com n'aurait jamais dû être contacté : contactedHosts=%v", spy.contactedHosts)
		}
	}
}

func TestClient_Discover_RelativeHomeSetHref(t *testing.T) {
	m := newMockCalDAV(t)
	m.homeSetHrefFunc = func(string) string { return testHomeSetPath } // relatif
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	if err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover() erreur : %v", err)
	}
	if c.shardBase != m.URL() {
		t.Errorf("shardBase = %q, want %q (fallback baseURL pour href relatif)", c.shardBase, m.URL())
	}
}

// TestClient_Discover_RejectsOversizedPropfindResponse, FIX-3. Une réponse
// PROPFIND anormalement volumineuse (serveur bogué ou hostile) doit être
// rejetée plutôt que chargée intégralement en mémoire via io.ReadAll non
// borné.
func TestClient_Discover_RejectsOversizedPropfindResponse(t *testing.T) {
	huge := make([]byte, (8<<20)+1) // 8 Mio + 1 octet, au-delà de la borne défensive.
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
		t.Fatal("attendu : erreur réponse PROPFIND trop volumineuse")
	}
	if !strings.Contains(err.Error(), "volumineuse") {
		t.Errorf("erreur attendue mentionnant 'volumineuse', obtenu : %v", err)
	}
}

// --- Tests : ListCalendars --------------------------------------------------

func TestClient_ListCalendars_FiltersTechnicalCalendars(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	c := m.client()

	cals, err := c.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars() erreur : %v", err)
	}
	if len(cals) != 1 {
		t.Fatalf("ListCalendars() = %d calendriers, want 1 (inbox/outbox/VTODO-only filtrés) : %+v", len(cals), cals)
	}
	got := cals[0]
	if got.Path != testHomeCalendar {
		t.Errorf("Path = %q, want %q", got.Path, testHomeCalendar)
	}
	if got.Name != "Maison" {
		t.Errorf("Name = %q, want %q", got.Name, "Maison")
	}
	if got.Description != "Calendrier de la maison" {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Color != "#FF2968FF" {
		t.Errorf("Color = %q, want #FF2968FF", got.Color)
	}
}

// --- Tests : SearchEvents ----------------------------------------------------

func TestClient_SearchEvents_TimeRangeTransmitted(t *testing.T) {
	m := newMockCalDAV(t)
	m.calendarsBody = defaultCalendarsBody()
	m.objects["uid-simple-1"] = mockObject{path: testHomeCalendar + "uid-simple-1.ics", ics: icsSimpleEvent}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	events, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() erreur : %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d événements, want 1 : %+v", len(events), events)
	}
	if events[0].Title != "Reunion equipe" {
		t.Errorf("Title = %q", events[0].Title)
	}

	m.mu.Lock()
	body := string(m.lastReportBody)
	m.mu.Unlock()
	if !strings.Contains(body, `start="20260701T000000Z"`) {
		t.Errorf("corps REPORT attendu contenant start=\"20260701T000000Z\", obtenu : %s", body)
	}
	if !strings.Contains(body, `end="20260708T000000Z"`) {
		t.Errorf("corps REPORT attendu contenant end=\"20260708T000000Z\", obtenu : %s", body)
	}
}

func TestClient_SearchEvents_AllDay(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-allday-1"] = mockObject{path: testHomeCalendar + "uid-allday-1.ics", ics: icsAllDay}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	events, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() erreur : %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d événements, want 1", len(events))
	}
	if !events[0].AllDay {
		t.Errorf("AllDay = false, want true pour uid-allday-1")
	}
}

// TestClient_SearchEvents_NoDtendDerivedFromDuration, FIX-6. Un VEVENT sans
// DTEND mais avec DURATION doit avoir EndTime = StartTime + DURATION (et
// donc apparaître dans une recherche qui chevauche ce créneau).
func TestClient_SearchEvents_NoDtendDerivedFromDuration(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-duration-1"] = mockObject{path: testHomeCalendar + "uid-duration-1.ics", ics: icsNoDtendWithDuration}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	events, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() erreur : %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d événements, want 1 (DTEND dérivé de DURATION) : %+v", len(events), events)
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

// TestClient_SearchEvents_AllDayNoDtendDefaultsTo24h, FIX-6. Un VEVENT
// all-day sans DTEND ni DURATION doit avoir EndTime = StartTime + 24h, et
// donc apparaître dans une recherche qui chevauche cette journée.
func TestClient_SearchEvents_AllDayNoDtendDefaultsTo24h(t *testing.T) {
	m := newMockCalDAV(t)
	m.objects["uid-allday-nodtend-1"] = mockObject{path: testHomeCalendar + "uid-allday-nodtend-1.ics", ics: icsAllDayNoDtend}
	c := m.client()

	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	events, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() erreur : %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("SearchEvents() = %d événements, want 1 (DTEND dérivé à StartTime+24h) : %+v", len(events), events)
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
	events, err := c.SearchEvents(context.Background(), testHomeCalendar, start, end)
	if err != nil {
		t.Fatalf("SearchEvents() erreur : %v", err)
	}

	// uid-recur-1 : 5 occurrences hebdo, 1 exclue (EXDATE) → 4.
	// uid-override-1 : 4 occurrences hebdo, la 2e remplacée par l'override.
	if len(events) != 8 {
		t.Fatalf("SearchEvents() = %d événements, want 8 : %+v", len(events), events)
	}

	var overrideFound bool
	for _, e := range events {
		if e.UID == "uid-override-1" && e.Title == "Suivi projet (deplace)" {
			overrideFound = true
			if e.StartTime.Hour() != 16 {
				t.Errorf("override StartTime hour = %d, want 16 (déplacé)", e.StartTime.Hour())
			}
		}
		if e.UID == "uid-recur-1" && e.StartTime.Day() == 13 {
			t.Errorf("occurrence du 13 juillet ne devrait pas apparaître (EXDATE)")
		}
	}
	if !overrideFound {
		t.Errorf("override RECURRENCE-ID non trouvé dans les résultats : %+v", events)
	}
}

// --- Tests : CreateEvent ----------------------------------------------------

func TestClient_CreateEvent(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()

	uid, err := c.CreateEvent(context.Background(), testHomeCalendar, &NewEvent{
		Title:              "Nouvel événement",
		Location:           "Bureau",
		Notes:              "Notes de test",
		StartTime:          time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC),
		EndTime:            time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC),
		AlarmMinutesBefore: 15,
	})
	if err != nil {
		t.Fatalf("CreateEvent() erreur : %v", err)
	}

	matched, merr := regexpUIDFormat(uid)
	if merr != nil || !matched {
		t.Errorf("UID généré %q ne correspond pas au format attendu ^[0-9a-f]{32}@icloud-mcp$", uid)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("attendu 1 PUT, obtenu %d", len(m.puts))
	}
	put := m.puts[0]
	if !strings.HasPrefix(put.path, testHomeCalendar) || !strings.HasSuffix(put.path, uid+".ics") {
		t.Errorf("path PUT = %q, want préfixe %q et suffixe %q", put.path, testHomeCalendar, uid+".ics")
	}
	for _, want := range []string{"SUMMARY:Nouvel", "DTSTART", "DTEND", "UID:" + uid, "TRIGGER:-PT15M"} {
		if !strings.Contains(put.body, want) {
			t.Errorf("corps PUT attendu contenant %q, obtenu :\n%s", want, put.body)
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

// --- Tests : UpdateEvent ----------------------------------------------------

func TestClient_UpdateEvent_ChangeAndClearFields(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "some-other-filename.ics" // le nom de fichier != UID, volontairement
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	c := m.client()

	newTitle := "Titre modifié"
	emptyLocation := ""
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{
		Title:    &newTitle,
		Location: &emptyLocation, // effacement
		// Notes: nil → inchangé
	})
	if err != nil {
		t.Fatalf("UpdateEvent() erreur : %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("attendu 1 PUT, obtenu %d", len(m.puts))
	}
	put := m.puts[0]
	if put.path != objPath {
		t.Errorf("path PUT = %q, want %q (le vrai path, pas <uid>.ics)", put.path, objPath)
	}
	if !strings.Contains(put.body, "SUMMARY:Titre modifié") {
		t.Errorf("corps PUT devrait contenir le nouveau titre : %s", put.body)
	}
	if strings.Contains(put.body, "LOCATION:") {
		t.Errorf("LOCATION devrait avoir été effacé : %s", put.body)
	}
	if !strings.Contains(put.body, "DESCRIPTION:Point hebdo") {
		t.Errorf("DESCRIPTION (notes) inchangée attendue : %s", put.body)
	}
	if !strings.Contains(put.body, "SEQUENCE:1") {
		t.Errorf("SEQUENCE attendu incrémenté à 1 : %s", put.body)
	}
}

func TestClient_UpdateEvent_UIDNotFound(t *testing.T) {
	m := newMockCalDAV(t)
	c := m.client()

	newTitle := "x"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-inconnu", &EventUpdate{Title: &newTitle})
	if err == nil {
		t.Fatal("attendu : erreur événement introuvable")
	}
	if !strings.Contains(err.Error(), "introuvable") {
		t.Errorf("erreur attendue mentionnant 'introuvable', obtenu : %v", err)
	}
}

func TestClient_UpdateEvent_PreservesAllDayFormat(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-allday-1.ics"
	m.objects["uid-allday-1"] = mockObject{path: objPath, ics: icsAllDay}
	c := m.client()

	// icsAllDay va du 10 au 11 juillet (DTEND exclusif) ; on avance le
	// début au 9 juillet pour rester avant la fin existante.
	newStart := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-allday-1", &EventUpdate{StartTime: &newStart})
	if err != nil {
		t.Fatalf("UpdateEvent() erreur : %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	put := m.puts[0]
	if !strings.Contains(put.body, "DTSTART;VALUE=DATE:20260709") && !strings.Contains(put.body, "DTSTART:20260709") {
		t.Errorf("DTSTART devrait rester au format date pure (8 caractères) : %s", put.body)
	}
	if strings.Contains(put.body, "DTSTART") && strings.Contains(put.body, "T000000") {
		t.Errorf("DTSTART ne devrait pas avoir été converti en datetime : %s", put.body)
	}
}

// TestClient_UpdateEvent_UsesGETNotFilteredREPORTData, FIX-B. Le REPORT
// (findEventByUID) renvoie un calendar-data FILTRÉ (VEVENT nu, sans
// VERSION/PRODID/VTIMEZONE), comme le fait le vrai iCloud sous filtre
// calendar-query. UpdateEvent DOIT relire l'objet complet via GET
// (GetCalendarObject) avant de le modifier et de le PUT, sinon
// go-ical.Encode échoue (VERSION/PRODID absents) ou le VTIMEZONE de
// l'événement est perdu au round-trip.
func TestClient_UpdateEvent_UsesGETNotFilteredREPORTData(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-tz-1.ics"
	m.objects["uid-tz-1"] = mockObject{
		path:   objPath,
		ics:    icsFilteredReportOnly, // réponse REPORT : filtrée
		getIcs: icsFullGetVersion,     // réponse GET : complète
	}
	c := m.client()

	newTitle := "Reunion NY (modifiee)"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-tz-1", &EventUpdate{Title: &newTitle})
	if err != nil {
		t.Fatalf("UpdateEvent() erreur : %v (devrait relire l'objet complet via GET) : %v", err, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.gets) != 1 {
		t.Fatalf("attendu 1 GET (GetCalendarObject), obtenu %d : %v", len(m.gets), m.gets)
	}
	if len(m.puts) != 1 {
		t.Fatalf("attendu 1 PUT, obtenu %d", len(m.puts))
	}
	put := m.puts[0]
	if put.path != objPath {
		t.Errorf("path PUT = %q, want %q", put.path, objPath)
	}
	for _, want := range []string{"VERSION:2.0", "PRODID:", "BEGIN:VTIMEZONE", "TZID:America/New_York", "SUMMARY:" + newTitle} {
		if !strings.Contains(put.body, want) {
			t.Errorf("corps PUT attendu contenant %q (preuve d'une lecture via GET complet, pas via REPORT filtré), obtenu :\n%s", want, put.body)
		}
	}
}

// --- Tests : DeleteEvent ----------------------------------------------------

func TestClient_DeleteEvent_ReturnsTitleAndDeletesRealPath(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "fichier-import-ancien.ics" // != uid-simple-1.ics
	m.objects["uid-simple-1"] = mockObject{path: objPath, ics: icsSimpleEvent}
	c := m.client()

	title, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-simple-1")
	if err != nil {
		t.Fatalf("DeleteEvent() erreur : %v", err)
	}
	if title != "Reunion equipe" {
		t.Errorf("title = %q, want %q", title, "Reunion equipe")
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

	_, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-inconnu")
	if err == nil {
		t.Fatal("attendu : erreur événement introuvable")
	}
}
