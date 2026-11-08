package contacts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// Deterministic offline Contacts benchmark fixtures. The index-based generator
// has no randomness or clock dependency. The seed name identifies the recipe.
const (
	contactsBenchmarkSeed  = "contacts-index-v1"
	contactsBenchmarkCount = 2000
)

type contactsBenchmarkCard struct {
	href string
	card string
}

func contactsBenchmarkReport() string {
	cards := make([]contactsBenchmarkCard, 0, contactsBenchmarkCount)
	for i := range contactsBenchmarkCount {
		uid := fmt.Sprintf("u-%04d", i)
		name := fmt.Sprintf("Contact %04d", i)
		cards = append(cards, contactsBenchmarkCard{
			href: "/home/book/" + uid + ".vcf",
			card: v3Card(uid, name,
				fmt.Sprintf("EMAIL:contact-%04d@example.test", i),
				fmt.Sprintf("TEL:+3310000%04d", i),
				"ORG:Benchmark Org",
			),
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

type contactsBenchmarkFixture struct {
	doer   HTTPDoer
	client *Client
	rpcs   atomic.Int64
}

func newContactsBenchmarkFixture(reportBody string) *contactsBenchmarkFixture {
	fixture := &contactsBenchmarkFixture{}
	base := discoveryDoer(reportBody)
	fixture.doer = doerFunc(func(req *http.Request) (*http.Response, error) {
		fixture.rpcs.Add(1)
		return base(req)
	})
	fixture.client = NewClient(fixture.doer, "https://contacts.icloud.com/", allowContactsHost)
	return fixture
}

func (f *contactsBenchmarkFixture) discover(tb testing.TB) {
	tb.Helper()
	if _, err := f.client.ListAddressBooks(context.Background()); err != nil {
		tb.Fatalf("contacts discovery: %v", err)
	}
}

func (f *contactsBenchmarkFixture) search(tb testing.TB) {
	tb.Helper()
	books, err := f.client.ListAddressBooks(context.Background())
	if err != nil {
		tb.Fatalf("contacts discovery: %v", err)
	}
	if len(books) != 1 {
		tb.Fatalf("address books = %d, want 1", len(books))
	}
	result, err := f.client.SearchContacts(context.Background(), SearchOptions{
		AddressBook: books[0].Identifier,
		Query:       "Contact 1999",
		Limit:       100,
	})
	if err != nil {
		tb.Fatalf("contacts search: %v", err)
	}
	if len(result.Contacts) != 1 || result.Contacts[0].UID != "u-1999" {
		tb.Fatalf("contacts result = %+v", result.Contacts)
	}
	if result.ScanLimitReached || result.Truncated {
		tb.Fatalf("contacts result reached a cap: %+v", result)
	}
}

func TestContactsBenchmarkReportDeterministic(t *testing.T) {
	first := contactsBenchmarkReport()
	firstHash := sha256.Sum256([]byte(first))
	for iteration := 0; iteration < 20; iteration++ {
		later := contactsBenchmarkReport()
		laterHash := sha256.Sum256([]byte(later))
		if later != first {
			t.Fatalf("contacts benchmark fixture bytes differ between runs: first=%d later=%d iteration=%d first_sha256=%x later_sha256=%x",
				len(first), len(later), iteration, firstHash, laterHash)
		}
	}
	t.Logf("fixture_seed=%s size=%d report_bytes=%d report_sha256=%x", contactsBenchmarkSeed, contactsBenchmarkCount, len(first), firstHash)
}

func TestContactsBenchmarkScenarios(t *testing.T) {
	t.Logf("fixture_seed=%s size=%d", contactsBenchmarkSeed, contactsBenchmarkCount)
	reportBody := contactsBenchmarkReport()
	cold := newContactsBenchmarkFixture(reportBody)
	cold.search(t)
	if got := cold.rpcs.Load(); got != 4 {
		t.Fatalf("cold RPCs = %d, want 4", got)
	}

	warm := newContactsBenchmarkFixture(reportBody)
	warm.discover(t)
	base := warm.rpcs.Load()
	warm.search(t)
	if got := warm.rpcs.Load() - base; got != 1 {
		t.Fatalf("warm RPCs = %d, want 1", got)
	}
}

func BenchmarkContactsParseSearch2000(b *testing.B) {
	reportBody := contactsBenchmarkReport()
	b.Run("ColdDiscovery", func(b *testing.B) {
		fixture := newContactsBenchmarkFixture(reportBody)
		fixture.search(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			fixture = newContactsBenchmarkFixture(reportBody)
			fixture.search(b)
		}
		b.StopTimer()
		b.ReportMetric(float64(fixture.rpcs.Load()), "rpc/op")
		b.ReportMetric(contactsBenchmarkCount, "fixture-contacts")
	})
	b.Run("WarmDiscovery", func(b *testing.B) {
		fixture := newContactsBenchmarkFixture(reportBody)
		fixture.discover(b)
		base := fixture.rpcs.Load()
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			fixture.search(b)
		}
		b.StopTimer()
		if b.N > 0 {
			b.ReportMetric(float64(fixture.rpcs.Load()-base)/float64(b.N), "rpc/op")
		}
		b.ReportMetric(contactsBenchmarkCount, "fixture-contacts")
	})
}
