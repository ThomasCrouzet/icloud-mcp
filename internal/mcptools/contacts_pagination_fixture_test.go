package mcptools

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Deterministic multi-book CardDAV fixture for cursor page measurement. Every
// value derives from an index, so REPORT bodies are byte-identical between
// runs. The seed name identifies the recipe.
const (
	paginationFixtureSeed    = "mcptools-contacts-pagination-v1"
	paginationFixtureBooks   = 3
	paginationFixturePerBook = 200
)

var paginationFixturePaths = []string{"/home/work/", "/home/family/", "/home/friends/"}

type paginationCounters struct {
	propfinds   atomic.Int64
	reports     atomic.Int64
	reportBytes atomic.Int64
}

func (c *paginationCounters) snapshot() (reports, bytes int64) {
	return c.reports.Load(), c.reportBytes.Load()
}

// paginationDisplayName keeps book 0 and book 1 names equal after
// normalization and adds letter case plus surrounding whitespace in book 2.
func paginationDisplayName(book, index int) string {
	if book == 2 {
		return "\t person " + fmt.Sprintf("%04d", index) + " "
	}
	return fmt.Sprintf("Person %04d", index)
}

func paginationXML(value string) string {
	var builder strings.Builder
	_ = xml.EscapeText(&builder, []byte(value))
	return builder.String()
}

// paginationCard reuses the same UID in every book, so the fixture proves that
// identical UID values in distinct books stay distinct summaries.
func paginationCard(book, index int) string {
	uid := fmt.Sprintf("u-%04d", index)
	name := paginationDisplayName(book, index)
	lines := []string{
		"BEGIN:VCARD", "VERSION:3.0", "UID:" + uid,
		"FN:" + name, "N:" + name + ";;;;",
		fmt.Sprintf("EMAIL:contact-%04d-%d@example.test", index, book),
		fmt.Sprintf("TEL:+3310000%04d", index),
		"ORG:Pagination Org",
		"END:VCARD",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func paginationHref(book, index int) string {
	return paginationFixturePaths[book] + fmt.Sprintf("u-%04d.vcf", index)
}

func paginationReportBody(book int) string {
	hrefs := make([]string, 0, paginationFixturePerBook)
	for index := range paginationFixturePerBook {
		hrefs = append(hrefs, paginationHref(book, index))
	}
	sort.Strings(hrefs)
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`)
	for _, href := range hrefs {
		var index int
		if _, err := fmt.Sscanf(href, paginationFixturePaths[book]+"u-%04d.vcf", &index); err != nil {
			panic("pagination fixture href parse failed")
		}
		body.WriteString(`<D:response><D:href>` + paginationXML(href) + `</D:href><D:propstat><D:prop><D:getetag>&quot;e&quot;</D:getetag><C:address-data>` +
			paginationXML(paginationCard(book, index)) + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	body.WriteString(`</D:multistatus>`)
	return body.String()
}

func paginationHomesXML() string {
	var rows strings.Builder
	for _, path := range paginationFixturePaths {
		rows.WriteString(`<D:response><D:href>` + paginationXML(path) + `</D:href><D:propstat><D:prop>` +
			`<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>` +
			`<D:displayname>Book ` + paginationXML(path) + `</D:displayname>` +
			`<C:supported-address-data><C:address-data-type content-type="text/vcard" version="3.0"/></C:supported-address-data>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` + rows.String() + `</D:multistatus>`
}

// paginationDoer serves the fixture and counts REPORT traffic exactly.
type paginationDoer struct {
	counters *paginationCounters
	bodies   []string

	mu        sync.Mutex
	failBooks map[string]bool
}

func newPaginationDoer(counters *paginationCounters) *paginationDoer {
	doer := &paginationDoer{counters: counters, bodies: make([]string, paginationFixtureBooks), failBooks: map[string]bool{}}
	for book := range paginationFixtureBooks {
		doer.bodies[book] = paginationReportBody(book)
	}
	return doer
}

func (d *paginationDoer) failBook(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failBooks[path] = true
}

func (d *paginationDoer) healBook(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.failBooks, path)
}

func (d *paginationDoer) renameCard(book, index int, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bodies[book] = strings.ReplaceAll(d.bodies[book],
		paginationXML(paginationDisplayName(book, index)), paginationXML(name))
}

func paginationResponse(req *http.Request, status int, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/xml")
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func (d *paginationDoer) Do(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case "PROPFIND":
		d.counters.propfinds.Add(1)
		switch req.URL.Path {
		case "/":
			return paginationResponse(req, http.StatusMultiStatus,
				`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`), nil
		case "/principal/":
			return paginationResponse(req, http.StatusMultiStatus,
				`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/principal/</D:href><D:propstat><D:prop><C:addressbook-home-set><D:href>/home/</D:href></C:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`), nil
		case "/home/":
			return paginationResponse(req, http.StatusMultiStatus, paginationHomesXML()), nil
		}
	case "REPORT":
		d.mu.Lock()
		failing := d.failBooks[req.URL.Path]
		d.mu.Unlock()
		if failing {
			return paginationResponse(req, http.StatusServiceUnavailable, ""), nil
		}
		for book, path := range paginationFixturePaths {
			if req.URL.Path != path {
				continue
			}
			d.counters.reports.Add(1)
			d.counters.reportBytes.Add(int64(len(d.bodies[book])))
			return paginationResponse(req, http.StatusMultiStatus, d.bodies[book]), nil
		}
	}
	return paginationResponse(req, http.StatusNotFound, ""), nil
}

// newPaginationClient builds a real Contacts client over the fixture, so tests
// can count REPORT requests and transferred bytes. The read limits stay at
// their production values, so a test must stay inside the burst it allows.
func newPaginationClient(counters *paginationCounters) (*contacts.Client, *paginationDoer) {
	doer := newPaginationDoer(counters)
	client := contacts.NewClient(doer, "https://contacts.icloud.com/",
		func(host string) bool { return host == "contacts.icloud.com" })
	return client, doer
}

// paginationFakeService returns one fixed result set and counts searches. It
// removes the read rate limit from cursor tests that exercise the codec alone,
// because each search costs several REPORT requests on the real client.
type paginationFakeService struct {
	rows []contacts.ContactSummary

	mu       sync.Mutex
	searches int
}

func newPaginationFakeService() *paginationFakeService {
	service := &paginationFakeService{}
	for index := range paginationFixturePerBook {
		// Equal normalized names, identical UIDs, distinct books. The rows are
		// generated in the canonical order: name, UID, then address book.
		for book := range paginationFixtureBooks {
			service.rows = append(service.rows, contacts.ContactSummary{
				AddressBook: fmt.Sprintf("book-%02d", book),
				UID:         fmt.Sprintf("u-%04d", index),
				DisplayName: paginationDisplayName(book, index),
			})
		}
	}
	sort.SliceStable(service.rows, func(i, j int) bool {
		left, right := service.rows[i], service.rows[j]
		name, other := contacts.NormalizeSortName(left.DisplayName), contacts.NormalizeSortName(right.DisplayName)
		if name != other {
			return name < other
		}
		if left.UID != right.UID {
			return left.UID < right.UID
		}
		return left.AddressBook < right.AddressBook
	})
	return service
}

func (s *paginationFakeService) searchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searches
}

func (s *paginationFakeService) ListAddressBooks(context.Context) ([]contacts.AddressBook, error) {
	return []contacts.AddressBook{{Identifier: "book-00"}}, nil
}

func (s *paginationFakeService) SearchContacts(_ context.Context, opts contacts.SearchOptions) (contacts.SearchResult, error) {
	s.mu.Lock()
	s.searches++
	s.mu.Unlock()
	rows := append([]contacts.ContactSummary(nil), s.rows...)
	if opts.AddressBook != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if row.AddressBook == opts.AddressBook {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	return contacts.SearchResult{Contacts: rows}, nil
}

func (s *paginationFakeService) GetContact(context.Context, string, string) (*contacts.Contact, error) {
	return nil, nil
}

func (s *paginationFakeService) CreateContact(context.Context, *contacts.CreateContactInput) (contacts.CreateResult, error) {
	return contacts.CreateResult{}, nil
}

func (s *paginationFakeService) UpdateContact(context.Context, *contacts.UpdateContactInput) (contacts.UpdateResult, error) {
	return contacts.UpdateResult{}, nil
}

func (s *paginationFakeService) DeleteContact(context.Context, *contacts.DeleteContactInput) (contacts.DeleteResult, error) {
	return contacts.DeleteResult{}, nil
}

func paginationDeps(t *testing.T, client contacts.Service, codec *contactsCursorCodec) ContactsDeps {
	t.Helper()
	if codec == nil {
		codec = newContactsCursorCodec()
	}
	return ContactsDeps{Service: client, Redactor: security.NewRedactor("unused"), ContactsCursor: codec}
}

func paginationRowKey(summary contacts.ContactSummary) string {
	return summary.AddressBook + "\x00" + summary.UID
}

// TestContactsPaginationOneWalkIsOneScan is the issue #215 regression test. It
// paginates one fixed multi-book result set and asserts that the whole walk
// costs one REPORT per book, not one REPORT per book per page. It uses only the
// tool handler and the counted fake, so it compiles against the pre-fix code.
func TestContactsPaginationOneWalkIsOneScan(t *testing.T) {
	const limit = 100
	counters := &paginationCounters{}
	client, _ := newPaginationClient(counters)
	books, err := client.ListAddressBooks(context.Background())
	if err != nil {
		t.Fatalf("ListAddressBooks() error = %v", err)
	}
	if len(books) != paginationFixtureBooks {
		t.Fatalf("address books = %d, want %d", len(books), paginationFixtureBooks)
	}
	counters.reports.Store(0)
	counters.reportBytes.Store(0)

	deps := paginationDeps(t, client, newContactsCursorCodec())
	rows, pages := walkPaginationPages(t, deps, nil, limit)
	total := paginationFixtureBooks * paginationFixturePerBook
	if len(rows) != total {
		t.Fatalf("rows = %d, want %d", len(rows), total)
	}
	if want := (total + limit - 1) / limit; pages != want {
		t.Fatalf("pages = %d, want %d", pages, want)
	}
	seen := make(map[string]int, len(rows))
	for _, summary := range rows {
		seen[paginationRowKey(summary)]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("row %q observed %d times across pages", key, count)
		}
	}
	reports, reportBytes := counters.snapshot()
	if want := int64(paginationFixtureBooks); reports != want {
		t.Fatalf("a %d-page walk over %d books issued %d REPORTs, want %d; per-page bytes %d",
			pages, paginationFixtureBooks, reports, want, reportBytes)
	}
	t.Logf("walk_seed=%s pages=%d rows=%d reports=%d report_bytes=%d",
		paginationFixtureSeed, pages, len(rows), reports, reportBytes)
}

// walkPaginationPages requests every page for one filter set and returns the
// emitted rows in order with the page count.
func walkPaginationPages(t *testing.T, deps ContactsDeps, args map[string]any, limit int) ([]contacts.ContactSummary, int) {
	t.Helper()
	handler := searchContactsHandler(deps)
	cursor := "start"
	var rows []contacts.ContactSummary
	pages := 0
	for {
		request := map[string]any{"cursor": cursor, "limit": limit}
		for key, value := range args {
			request[key] = value
		}
		res, err := handler(context.Background(), newReq(request))
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if res.IsError {
			t.Fatalf("page %d failed: %s", pages, resultText(t, res))
		}
		var page contacts.SearchResult
		decodeResult(t, res, &page)
		rows = append(rows, page.Contacts...)
		pages++
		if page.NextCursor == "" {
			return rows, pages
		}
		cursor = page.NextCursor
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
	}
}
