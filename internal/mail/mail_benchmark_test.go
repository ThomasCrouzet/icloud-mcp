package mail

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"golang.org/x/time/rate"
)

// Deterministic offline Mail benchmark fixtures. The sparse UID base is near
// the uint32 maximum so the fixture exercises large UID values without a dense
// mailbox or user data.
const (
	mailBenchmarkSeed     = "mail-pattern-v1"
	mailBenchmarkUIDBase  = uint32(4_000_000_000)
	mailBenchmarkUIDCount = 30
)

func newMailBenchmarkService(tb testing.TB, imap *fakeIMAP) *Client {
	tb.Helper()
	policy, err := ParseRecipientPolicy("*")
	if err != nil {
		tb.Fatal(err)
	}
	factory := func(context.Context, net.Conn, string, string) (imapSession, error) { return imap, nil }
	service, err := newService(Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
		nil, false, false, factory, nil)
	if err != nil {
		tb.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	// The throughput fixture disables the local read limiter so a large N
	// measures per-operation work instead of the 60-per-minute product gate.
	// Existing service tests assert that the 11th burst read is rate limited.
	service.readLimiter = rate.NewLimiter(rate.Inf, 1)
	return service
}

func mailBenchmarkSearchFixture(tb testing.TB) (*fakeIMAP, *Client) {
	tb.Helper()
	uids := make([]uint32, 0, mailBenchmarkUIDCount)
	metadata := make([]imapadapter.Message, 0, mailBenchmarkUIDCount)
	for i := range mailBenchmarkUIDCount {
		uid := mailBenchmarkUIDBase - uint32(i)
		uids = append(uids, uid)
		metadata = append(metadata, imapadapter.Message{
			UID:          uid,
			Flags:        []string{"\\Seen"},
			Envelope:     imapadapter.Envelope{Subject: fmt.Sprintf("sparse fixture %02d", i)},
			InternalDate: time.Date(2026, 7, 1, 0, i, 0, 0, time.UTC),
			Size:         int64(1000 + i),
		})
	}
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 7, UIDNext: mailBenchmarkUIDBase + 1},
		searchFn: func(request imapadapter.SearchRequest) ([]uint32, error) {
			if request.UIDMax > mailBenchmarkUIDBase || request.UIDMin > request.UIDMax {
				return nil, &imapadapter.Error{Kind: imapadapter.ErrorProtocol}
			}
			return uids, nil
		},
		metadata: metadata,
	}
	return imap, newMailBenchmarkService(tb, imap)
}

func assertMailBenchmarkSearch(tb testing.TB, imap *fakeIMAP, service *Client) {
	tb.Helper()
	imap.mu.Lock()
	searchStart := len(imap.searches)
	imap.mu.Unlock()
	result, err := service.SearchMessages(context.Background(), SearchInput{
		Mailbox:     "INBOX",
		BeforeUID:   mailBenchmarkUIDBase + 1,
		UIDValidity: 7,
		Limit:       20,
	})
	if err != nil {
		tb.Fatalf("mail search: %v", err)
	}
	if len(result.Messages) != 20 || !result.Truncated {
		tb.Fatalf("mail search result = %+v", result)
	}
	if result.Messages[0].UID != mailBenchmarkUIDBase {
		tb.Fatalf("highest UID = %d, want %d", result.Messages[0].UID, mailBenchmarkUIDBase)
	}
	imap.mu.Lock()
	searches := append([]imapadapter.SearchRequest(nil), imap.searches[searchStart:]...)
	imap.mu.Unlock()
	if len(searches) != 1 || searches[0].UIDMax > mailBenchmarkUIDBase || searches[0].UIDMax <= 3_999_990_000 {
		tb.Fatalf("search windows = %+v", searches)
	}
}

func mailBenchmarkGetFixture(tb testing.TB, bodyBytes int) (*fakeIMAP, *Client) {
	tb.Helper()
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 9, UIDNext: mailBenchmarkUIDBase + 1},
		metadata: []imapadapter.Message{{
			UID:      mailBenchmarkUIDBase,
			Envelope: imapadapter.Envelope{Subject: "body fixture"},
			Parts: []imapadapter.BodyPart{{
				Path: []int{1}, ContentType: "text/plain", InlinePlain: true,
			}},
		}},
		body: imapadapter.BodyData{
			MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"),
			Body:       []byte(strings.Repeat(string(mailBenchmarkSeed[0]), bodyBytes)),
		},
	}
	return imap, newMailBenchmarkService(tb, imap)
}

func assertMailBenchmarkGet(tb testing.TB, service *Client, bodyBytes int, wantOmitted bool) {
	tb.Helper()
	message, err := service.GetMessage(context.Background(), GetMessageInput{
		Mailbox: "INBOX", UIDValidity: 9, UID: mailBenchmarkUIDBase, MaxBodyBytes: MaxBodyBytes,
	})
	if err != nil {
		tb.Fatalf("mail get: %v", err)
	}
	if message.BodyOmitted != wantOmitted {
		tb.Fatalf("body omitted = %v, want %v (%+v)", message.BodyOmitted, wantOmitted, message.Warnings)
	}
	if wantOmitted {
		if len(message.Warnings) == 0 || message.Warnings[0].Code != "body_too_large" || message.Body != "" {
			tb.Fatalf("oversize body result = %+v", message)
		}
		return
	}
	if len(message.Body) != bodyBytes || message.BodySource != "plain_text" {
		tb.Fatalf("body bytes = %d source = %q, want %d plain_text", len(message.Body), message.BodySource, bodyBytes)
	}
}

func mailBenchmarkCommandCount(imap *fakeIMAP, from int) int {
	imap.mu.Lock()
	defer imap.mu.Unlock()
	count := 0
	for _, command := range imap.log[from:] {
		if command != "close" {
			count++
		}
	}
	return count
}

func mailBenchmarkLogLength(imap *fakeIMAP) int {
	imap.mu.Lock()
	defer imap.mu.Unlock()
	return len(imap.log)
}

func TestMailBenchmarkScenarios(t *testing.T) {
	t.Logf("fixture_seed=%s sparse_uid_base=%d", mailBenchmarkSeed, mailBenchmarkUIDBase)
	searchIMAP, searchService := mailBenchmarkSearchFixture(t)
	searchStart := mailBenchmarkLogLength(searchIMAP)
	for iteration := 0; iteration < 2; iteration++ {
		assertMailBenchmarkSearch(t, searchIMAP, searchService)
		if got := mailBenchmarkCommandCount(searchIMAP, searchStart); got != 3*(iteration+1) {
			t.Fatalf("search commands after %d iterations = %d, want %d", iteration+1, got, 3*(iteration+1))
		}
	}

	for _, test := range []struct {
		name        string
		bodyBytes   int
		wantOmitted bool
	}{
		{name: "NearLimitBody", bodyBytes: MaxBodyBytes},
		{name: "OverLimitBodyRefusal", bodyBytes: MaxBodyBytes + 1, wantOmitted: true},
	} {
		imap, service := mailBenchmarkGetFixture(t, test.bodyBytes)
		start := mailBenchmarkLogLength(imap)
		assertMailBenchmarkGet(t, service, test.bodyBytes, test.wantOmitted)
		if got := mailBenchmarkCommandCount(imap, start); got != 3 {
			t.Fatalf("%s commands = %d, want 3", test.name, got)
		}
	}
}

func BenchmarkMailSearchSparseUID(b *testing.B) {
	imap, service := mailBenchmarkSearchFixture(b)
	base := mailBenchmarkLogLength(imap)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		assertMailBenchmarkSearch(b, imap, service)
	}
	b.StopTimer()
	if b.N > 0 {
		b.ReportMetric(float64(mailBenchmarkCommandCount(imap, base))/float64(b.N), "service-call/op")
	}
	b.ReportMetric(mailBenchmarkUIDCount, "fixture-candidates")
	b.ReportMetric(20, "result-limit")
	b.ReportMetric(float64(mailBenchmarkUIDBase), "sparse-uid-base")
}

func BenchmarkMailGetNearLimitBody(b *testing.B) {
	for _, test := range []struct {
		name        string
		bodyBytes   int
		wantOmitted bool
	}{
		{name: "NearLimitBody", bodyBytes: MaxBodyBytes},
		{name: "OverLimitBodyRefusal", bodyBytes: MaxBodyBytes + 1, wantOmitted: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			imap, service := mailBenchmarkGetFixture(b, test.bodyBytes)
			assertMailBenchmarkGet(b, service, test.bodyBytes, test.wantOmitted)
			start := mailBenchmarkLogLength(imap)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				assertMailBenchmarkGet(b, service, test.bodyBytes, test.wantOmitted)
			}
			b.StopTimer()
			if b.N > 0 {
				b.ReportMetric(float64(mailBenchmarkCommandCount(imap, start))/float64(b.N), "service-call/op")
			}
			b.ReportMetric(float64(test.bodyBytes), "payload-bytes")
			if test.wantOmitted {
				b.ReportMetric(1, "refused")
			} else {
				b.ReportMetric(0, "refused")
			}
		})
	}
}
