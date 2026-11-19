package contacts

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/time/rate"
)

// Deterministic offline pagination fixture. Every value derives from an index,
// so repeated runs produce byte-identical REPORT bodies. The seed name
// identifies the recipe.
//
// The fixture exercises three hazards at once:
//   - equal normalized display names in distinct books (book 0 and book 1),
//   - identical UID values in distinct books (all books reuse "u-%04d"),
//   - surrounding whitespace and letter case that normalization must fold.
const (
	contactsPaginationSeed         = "contacts-pagination-v1"
	contactsPaginationBookCount    = 3
	contactsPaginationCardsPerBook = 200
)

type contactsPaginationCounters struct {
	propfinds   atomic.Int64
	reports     atomic.Int64
	reportBytes atomic.Int64
}

func (c *contactsPaginationCounters) reportTotals() (int64, int64) {
	return c.reports.Load(), c.reportBytes.Load()
}

func (c *contactsPaginationCounters) resetReports() {
	c.reports.Store(0)
	c.reportBytes.Store(0)
}

func contactsPaginationBookPaths() []string {
	return []string{"/home/work/", "/home/family/", "/home/friends/"}
}

// contactsPaginationDisplayName returns the display name for one card. Book 2
// varies case and surrounding whitespace to prove the shared sort key folds
// both, so the ordering stays stable across books.
func contactsPaginationDisplayName(book, index int) string {
	name := fmt.Sprintf("Person %04d", index)
	if book == 2 {
		return " person " + fmt.Sprintf("%04d", index) + "\t"
	}
	return name
}

func contactsPaginationCard(book, index int) string {
	uid := fmt.Sprintf("u-%04d", index)
	return v3Card(uid, contactsPaginationDisplayName(book, index),
		fmt.Sprintf("EMAIL:contact-%04d-%d@example.test", index, book),
		fmt.Sprintf("TEL:+3310000%04d", index),
		"ORG:Pagination Org",
	)
}

func contactsPaginationReportBody(book int) string {
	cards := make([]contactsBenchmarkCard, 0, contactsPaginationCardsPerBook)
	for index := range contactsPaginationCardsPerBook {
		uid := fmt.Sprintf("u-%04d", index)
		cards = append(cards, contactsBenchmarkCard{
			href: contactsPaginationBookPaths()[book] + uid + ".vcf",
			card: contactsPaginationCard(book, index),
		})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].href < cards[j].href })
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`)
	for _, item := range cards {
		body.WriteString(`<D:response><D:href>` + xmlEscape(item.href) + `</D:href><D:propstat><D:prop><D:getetag>&quot;e&quot;</D:getetag><C:address-data>` + xmlEscape(item.card) + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	body.WriteString(`</D:multistatus>`)
	return body.String()
}

func contactsPaginationHomeXML() string {
	var values strings.Builder
	for _, path := range contactsPaginationBookPaths() {
		values.WriteString(`<D:response><D:href>` + xmlEscape(path) + `</D:href><D:propstat><D:prop>` +
			`<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>` +
			`<D:displayname>Book ` + xmlEscape(path) + `</D:displayname>` +
			`<C:supported-address-data><C:address-data-type content-type="text/vcard" version="3.0"/></C:supported-address-data>` +
			`<C:max-resource-size>1048576</C:max-resource-size>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
		values.String() + `</D:multistatus>`
}

// contactsPaginationDoer serves three address books and counts the REPORT
// traffic, so tests can measure requests and transferred bytes exactly.
func contactsPaginationDoer(counters *contactsPaginationCounters) doerFunc {
	bodies := make([]string, contactsPaginationBookCount)
	for book := range contactsPaginationBookCount {
		bodies[book] = contactsPaginationReportBody(book)
	}
	paths := contactsPaginationBookPaths()
	return func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case "PROPFIND":
			counters.propfinds.Add(1)
			switch req.URL.Path {
			case "/":
				return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
			case "/principal/":
				return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
			case "/home/":
				return httpResponse(req, http.StatusMultiStatus, contactsPaginationHomeXML(), nil), nil
			}
		case "REPORT":
			for book, path := range paths {
				if req.URL.Path == path {
					counters.reports.Add(1)
					counters.reportBytes.Add(int64(len(bodies[book])))
					return httpResponse(req, http.StatusMultiStatus, bodies[book], nil), nil
				}
			}
		}
		return httpResponse(req, http.StatusNotFound, "", nil), nil
	}
}

// contactsPaginationClient builds a client over the deterministic fixture. The
// rate limiter is disabled because these tests and benchmarks measure decode
// and transfer work, not network pacing. Production limits stay untouched.
func contactsPaginationClient(tb testing.TB, counters *contactsPaginationCounters) *Client {
	tb.Helper()
	allow := func(host string) bool { return host == "contacts.icloud.com" }
	client := NewClient(contactsPaginationDoer(counters), "https://contacts.icloud.com/", allow)
	client.readLimit = rate.NewLimiter(rate.Inf, 1)
	client.writeLimit = rate.NewLimiter(rate.Inf, 1)
	return client
}

func contactsPaginationBooks(tb testing.TB, client *Client) []AddressBook {
	tb.Helper()
	books, err := client.ListAddressBooks(context.Background())
	if err != nil {
		tb.Fatalf("ListAddressBooks() error = %v", err)
	}
	if len(books) != contactsPaginationBookCount {
		tb.Fatalf("address books = %d, want %d", len(books), contactsPaginationBookCount)
	}
	return books
}

func TestContactsPaginationFixtureIsDeterministic(t *testing.T) {
	for book := range contactsPaginationBookCount {
		first := contactsPaginationReportBody(book)
		for iteration := 0; iteration < 4; iteration++ {
			if later := contactsPaginationReportBody(book); later != first {
				t.Fatalf("book %d fixture bytes differ between runs", book)
			}
		}
	}
	counters := &contactsPaginationCounters{}
	client := contactsPaginationClient(t, counters)
	books := contactsPaginationBooks(t, client)
	result, err := client.SearchContacts(context.Background(), SearchOptions{PageAll: true})
	if err != nil {
		t.Fatalf("SearchContacts() error = %v", err)
	}
	if len(result.Contacts) != contactsPaginationBookCount*contactsPaginationCardsPerBook {
		t.Fatalf("matches = %d, want %d", len(result.Contacts), contactsPaginationBookCount*contactsPaginationCardsPerBook)
	}
	if result.ScanLimitReached || result.Truncated {
		t.Fatalf("fixture reached a cap: %+v", result)
	}
	reports, bytes := counters.reportTotals()
	if reports != contactsPaginationBookCount {
		t.Fatalf("one full scan issued %d REPORTs, want %d", reports, contactsPaginationBookCount)
	}
	t.Logf("fixture_seed=%s books=%d cards_per_book=%d reports=%d report_bytes=%d books=%v",
		contactsPaginationSeed, contactsPaginationBookCount, contactsPaginationCardsPerBook,
		reports, bytes, len(books))
}

// TestContactsPaginationRescanCostIsPerPage records the pre-fix cost model: a
// cursor page repeated one REPORT per book for every page. It is the reference
// measurement for the snapshot design.
func TestContactsPaginationRescanCostIsPerPage(t *testing.T) {
	const limit = 100
	counters := &contactsPaginationCounters{}
	client := contactsPaginationClient(t, counters)
	contactsPaginationBooks(t, client)
	counters.resetReports()

	total := contactsPaginationBookCount * contactsPaginationCardsPerBook
	pages := (total + limit - 1) / limit
	seen := make(map[string]int)
	for page := range pages {
		result, err := client.SearchContacts(context.Background(), SearchOptions{Limit: limit, PageAll: true})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		start := page * limit
		end := start + limit
		if end > len(result.Contacts) {
			end = len(result.Contacts)
		}
		for _, summary := range result.Contacts[start:end] {
			seen[summary.AddressBook+"\x00"+summary.UID]++
		}
	}
	reports, bytes := counters.reportTotals()
	if reports != int64(pages*contactsPaginationBookCount) {
		t.Fatalf("rescan pages issued %d REPORTs, want %d", reports, pages*contactsPaginationBookCount)
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("row %q observed %d times", key, count)
		}
	}
	t.Logf("rescan_pages pages=%d reports=%d report_bytes=%d", pages, reports, bytes)
}

// BenchmarkContactsPagination measures the decode and transfer work for a
// fixed multi-book result set. RescanPerPage repeats the full scan for every
// page, which is the behaviour issue #215 reports. SingleScan performs the
// work once, which is the floor a bounded snapshot design can reach.
func BenchmarkContactsPagination(b *testing.B) {
	const limit = 100
	total := contactsPaginationBookCount * contactsPaginationCardsPerBook
	pages := (total + limit - 1) / limit

	b.Run("RescanPerPage", func(b *testing.B) {
		counters := &contactsPaginationCounters{}
		client := contactsPaginationClient(b, counters)
		contactsPaginationBooks(b, client)
		b.ReportAllocs()
		b.ResetTimer()
		var reports, bytes int64
		for range b.N {
			counters.resetReports()
			for range pages {
				if _, err := client.SearchContacts(context.Background(), SearchOptions{Limit: limit, PageAll: true}); err != nil {
					b.Fatalf("search: %v", err)
				}
			}
			usedReports, usedBytes := counters.reportTotals()
			reports += usedReports
			bytes += usedBytes
		}
		b.StopTimer()
		if b.N > 0 {
			b.ReportMetric(float64(reports)/float64(b.N), "report/op")
			b.ReportMetric(float64(bytes)/float64(b.N), "report-bytes/op")
		}
		b.ReportMetric(float64(pages), "pages/op")
	})

	b.Run("SingleScan", func(b *testing.B) {
		counters := &contactsPaginationCounters{}
		client := contactsPaginationClient(b, counters)
		contactsPaginationBooks(b, client)
		b.ReportAllocs()
		b.ResetTimer()
		var reports, bytes int64
		for range b.N {
			counters.resetReports()
			if _, err := client.SearchContacts(context.Background(), SearchOptions{Limit: limit, PageAll: true}); err != nil {
				b.Fatalf("search: %v", err)
			}
			usedReports, usedBytes := counters.reportTotals()
			reports += usedReports
			bytes += usedBytes
		}
		b.StopTimer()
		if b.N > 0 {
			b.ReportMetric(float64(reports)/float64(b.N), "report/op")
			b.ReportMetric(float64(bytes)/float64(b.N), "report-bytes/op")
		}
		b.ReportMetric(1, "pages/op")
	})
}
