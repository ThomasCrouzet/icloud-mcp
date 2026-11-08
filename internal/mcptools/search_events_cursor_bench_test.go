package mcptools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// BenchmarkSearchEventsCursorPagination measures the backend work for ten
// cursor pages. rpc/page is the amplification metric: the snapshot design
// scans once, so later pages add no REPORT.
func BenchmarkSearchEventsCursorPagination(b *testing.B) {
	events := denseMockEvents("/cal/a/", 4000)
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}}, Events: events}
	deps := testDeps(svc)
	codec := newSearchCursorCodec()
	handler := searchEventsHandlerWithCodec(deps, codec)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		before := svc.SearchCallCount
		pages := 0
		token := "start"
		for {
			res, err := handler(context.Background(), newReq(cursorBaseArgs("/cal/a/", 400, 0, token)))
			if err != nil || res.IsError {
				b.Fatalf("page %d: err=%v result=%v", pages+1, err, res)
			}
			var payload searchEventsResponse
			if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &payload); err != nil {
				b.Fatal(err)
			}
			pages++
			token = payload.NextCursor
			if token == "" {
				break
			}
			if pages > 20 {
				b.Fatal("cursor benchmark exceeded its page bound")
			}
		}
		b.StopTimer()
		if pages != 10 {
			b.Fatalf("pages = %d, want 10", pages)
		}
		b.ReportMetric(float64(svc.SearchCallCount-before)/float64(pages), "rpc/page")
		b.ReportMetric(float64(pages), "pages")
		b.StartTimer()
	}
}
