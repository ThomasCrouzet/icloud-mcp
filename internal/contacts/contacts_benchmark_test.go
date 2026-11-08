package contacts

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

// Deterministic offline Contacts benchmark fixtures. The index-based generator
// has no randomness or clock dependency. The seed name identifies the recipe.
const (
	contactsBenchmarkSeed  = "contacts-index-v1"
	contactsBenchmarkCount = 2000
)

func contactsBenchmarkReport() string {
	cards := make(map[string]string, contactsBenchmarkCount)
	for i := range contactsBenchmarkCount {
		uid := fmt.Sprintf("u-%04d", i)
		name := fmt.Sprintf("Contact %04d", i)
		cards["/home/book/"+uid+".vcf"] = v3Card(uid, name,
			fmt.Sprintf("EMAIL:contact-%04d@example.test", i),
			fmt.Sprintf("TEL:+3310000%04d", i),
			"ORG:Benchmark Org",
		)
	}
	return multiCardResponseXML(cards)
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
