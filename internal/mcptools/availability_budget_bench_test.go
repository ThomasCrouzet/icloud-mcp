package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

type availabilityBenchmarkHTTP struct {
	report string
}

func (d availabilityBenchmarkHTTP) Do(req *http.Request) (*http.Response, error) {
	var body string
	switch {
	case req.Method == "REPORT":
		body = d.report
	case req.URL.Path == "/":
		body = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:current-user-principal><D:href>/recurrence/principal/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	default:
		body = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response><D:href>/recurrence/principal/</D:href><D:propstat><D:prop><C:calendar-home-set><D:href>/recurrence/calendars/</D:href></C:calendar-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	}
	return &http.Response{StatusCode: http.StatusMultiStatus, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

type availabilityBenchmarkListedService struct {
	icloud.Service
	calendars []icloud.Calendar
}

func (s availabilityBenchmarkListedService) ListCalendars(context.Context) ([]icloud.Calendar, error) {
	return s.calendars, nil
}

func BenchmarkAvailabilityBudget(b *testing.B) {
	for _, name := range []string{"HighCardinality", "Overflow", "ManyEmpty", "DenseRecurrence", "LargeBuffers"} {
		b.Run(name, func(b *testing.B) {
			counts := []int{2500, 2500, 2500, 2500}
			if name == "Overflow" {
				counts = append(counts, 1)
			}
			if name == "ManyEmpty" {
				counts = make([]int, 1024)
			}
			svc, _ := availabilityBudgetFixture(counts)
			var service icloud.Service = svc
			start, end := mustParseTime("2026-07-01T09:00:00Z"), mustParseTime("2026-07-01T14:00:00Z")
			args := map[string]any{"duration_minutes": 60}
			wantCount := 4
			switch name {
			case "ManyEmpty":
				wantCount = 5
			case "LargeBuffers":
				args["buffer_before_minutes"], args["buffer_after_minutes"] = 1440, 1440
				wantCount = 0
			case "DenseRecurrence":
				data, err := os.ReadFile("testdata/recurrence/minutes-2000.ics")
				if err != nil {
					b.Fatal(err)
				}
				calendar := icloud.NewClient(availabilityBenchmarkHTTP{report: availabilityBudgetReport(strings.ReplaceAll(string(data), "\n", "\r\n"))},
					"https://caldav.icloud.com", func(host string) bool { return host == "caldav.icloud.com" })
				if err := calendar.Discover(b.Context()); err != nil {
					b.Fatal(err)
				}
				calendars := make([]icloud.Calendar, 5)
				for i := range calendars {
					calendars[i].Path = fmt.Sprintf("%sbench-%d/", recurrenceProtocolHome, i)
				}
				service = availabilityBenchmarkListedService{Service: calendar, calendars: calendars}
				start, end = mustParseTime("2026-01-01T00:00:00Z"), mustParseTime("2026-01-02T12:00:00Z")
				wantCount = 2
			}
			handler := findFreeSlotsHandler(freeSlotsTestDeps(service))
			req := freeSlotsTestRequest(start, end, args)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, err := handler(b.Context(), req)
				if err != nil {
					b.Fatal(err)
				}
				if name == "Overflow" {
					if !result.IsError || !strings.Contains(result.Content[0].(mcp.TextContent).Text, "payload_too_large") {
						b.Fatal("overflow returned availability")
					}
					continue
				}
				var payload freeSlotsResponse
				if err := json.Unmarshal([]byte(result.Content[0].(mcp.TextContent).Text), &payload); err != nil {
					b.Fatal(err)
				}
				if result.IsError || payload.Count != wantCount {
					b.Fatalf("fixture result=%+v error=%v", payload, result.IsError)
				}
			}
			b.StopTimer()
			if svc.max.Load() > 4 || svc.active.Load() != 0 {
				b.Fatal("collector exceeded its concurrency or detached workers")
			}
			b.ReportMetric(float64(calendarReadConcurrency), "worker-limit")
			b.ReportMetric(float64(end.Sub(start)/time.Hour), "range-hours")
		})
	}
}
