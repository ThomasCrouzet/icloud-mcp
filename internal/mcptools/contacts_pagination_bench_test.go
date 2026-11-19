package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// BenchmarkContactsCursorWalk measures the handler and codec cost of one cursor
// walk over a fixed 600-row, three-book result set. It uses the in-process fake
// service so the numbers exclude network pacing. The metrics show that one walk
// performs one service scan for six pages, so a regression that reintroduces a
// per-page rescan is visible in "scan/op".
func BenchmarkContactsCursorWalk(b *testing.B) {
	const limit = 100
	for _, name := range []string{"SixPages"} {
		b.Run(name, func(b *testing.B) {
			service := newPaginationFakeService()
			deps := ContactsDeps{
				Service:        service,
				Redactor:       security.NewRedactor("unused"),
				ContactsCursor: newContactsCursorCodec(),
			}
			handler := searchContactsHandler(deps)
			b.ReportAllocs()
			b.ResetTimer()
			pages := 0
			for range b.N {
				cursor := "start"
				for {
					res, err := handler(context.Background(), newReq(map[string]any{"cursor": cursor, "limit": limit}))
					if err != nil {
						b.Fatalf("page: %v", err)
					}
					if res.IsError {
						b.Fatalf("page failed: %s", benchResultText(b, res))
					}
					var page contacts.SearchResult
					if decodeErr := json.Unmarshal([]byte(benchResultText(b, res)), &page); decodeErr != nil {
						b.Fatalf("decode page: %v", decodeErr)
					}
					pages++
					if page.NextCursor == "" {
						break
					}
					cursor = page.NextCursor
				}
			}
			b.StopTimer()
			if b.N > 0 {
				b.ReportMetric(float64(pages)/float64(b.N), "pages/op")
				b.ReportMetric(float64(service.searchCount())/float64(b.N), "scan/op")
				b.ReportMetric(paginationFixtureBooks*paginationFixturePerBook, "rows")
			}
		})
	}
}

func benchResultText(tb testing.TB, res *mcp.CallToolResult) string {
	tb.Helper()
	var text strings.Builder
	for _, content := range res.Content {
		if value, ok := mcp.AsTextContent(content); ok {
			text.WriteString(value.Text)
		}
	}
	return text.String()
}
