package icloud

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Deterministic offline Calendar benchmark fixtures. The scenario names and
// input sizes are fixed so repeated runs remain comparable. No Apple
// credential or network destination is used.
const (
	calendarFixtureSeed = "calendar-index-v1"
	calendarFixtureZone = "Europe/Paris"
)

func calendarFixtureLocation(tb testing.TB) *time.Location {
	tb.Helper()
	location, err := time.LoadLocation(calendarFixtureZone)
	if err != nil {
		tb.Fatalf("load %s: %v", calendarFixtureZone, err)
	}
	return location
}

func calendarFixtureDate(location *time.Location, year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, location)
}

type recurrenceBenchmarkCase struct {
	name           string
	master         Event
	rangeStart     time.Time
	rangeEnd       time.Time
	maxOccurrences int
	wantCount      int
	wantTruncated  bool
	check          func(testing.TB, []Event)
}

func recurrenceBenchmarkCases(tb testing.TB) []recurrenceBenchmarkCase {
	tb.Helper()
	location := calendarFixtureLocation(tb)
	master := func(uid, rrule string, start time.Time) Event {
		return Event{UID: uid, Title: "benchmark recurring event", Recurrence: rrule,
			StartTime: start, EndTime: start.Add(time.Hour)}
	}
	return []recurrenceBenchmarkCase{
		{
			name:           "DSTSpringForward",
			master:         master("bench-dst-spring", "FREQ=DAILY;COUNT=40", calendarFixtureDate(location, 2026, time.March, 20, 9)),
			rangeStart:     calendarFixtureDate(location, 2026, time.March, 20, 0),
			rangeEnd:       calendarFixtureDate(location, 2026, time.April, 30, 0),
			maxOccurrences: 2000,
			wantCount:      40,
			check: func(tb testing.TB, events []Event) {
				tb.Helper()
				for _, event := range events {
					local := event.StartTime.In(location)
					if local.Hour() != 9 || local.Minute() != 0 {
						tb.Fatalf("DST occurrence moved off local 09:00: %s", local)
					}
				}
				before, after := events[0].StartTime.In(location), events[len(events)-1].StartTime.In(location)
				if _, offset := before.Zone(); offset != 1*60*60 {
					tb.Fatalf("first offset = %d, want +01:00", offset)
				}
				if _, offset := after.Zone(); offset != 2*60*60 {
					tb.Fatalf("last offset = %d, want +02:00", offset)
				}
			},
		},
		{
			name:           "DSTFallBack",
			master:         master("bench-dst-fall", "FREQ=DAILY;COUNT=40", calendarFixtureDate(location, 2026, time.October, 18, 9)),
			rangeStart:     calendarFixtureDate(location, 2026, time.October, 18, 0),
			rangeEnd:       calendarFixtureDate(location, 2026, time.November, 30, 0),
			maxOccurrences: 2000,
			wantCount:      40,
			check: func(tb testing.TB, events []Event) {
				tb.Helper()
				for _, event := range events {
					local := event.StartTime.In(location)
					if local.Hour() != 9 || local.Minute() != 0 {
						tb.Fatalf("DST occurrence moved off local 09:00: %s", local)
					}
				}
				before, after := events[0].StartTime.In(location), events[len(events)-1].StartTime.In(location)
				if _, offset := before.Zone(); offset != 2*60*60 {
					tb.Fatalf("first offset = %d, want +02:00", offset)
				}
				if _, offset := after.Zone(); offset != 1*60*60 {
					tb.Fatalf("last offset = %d, want +01:00", offset)
				}
			},
		},
		{
			name:           "SeriesCap2000",
			master:         master("bench-series-cap", "FREQ=HOURLY;INTERVAL=4;COUNT=2001", calendarFixtureDate(time.UTC, 2026, time.January, 1, 0)),
			rangeStart:     calendarFixtureDate(time.UTC, 2026, time.January, 1, 0),
			rangeEnd:       calendarFixtureDate(time.UTC, 2027, time.January, 1, 0),
			maxOccurrences: 2000,
			wantCount:      2000,
			wantTruncated:  true,
		},
		{
			name:           "Count2000",
			master:         master("bench-count-2000", "FREQ=HOURLY;COUNT=2000", calendarFixtureDate(time.UTC, 2026, time.January, 1, 0)),
			rangeStart:     calendarFixtureDate(time.UTC, 2026, time.January, 1, 0),
			rangeEnd:       calendarFixtureDate(time.UTC, 2026, time.April, 1, 0),
			maxOccurrences: 2000,
			wantCount:      2000,
			wantTruncated:  false,
		},
	}
}

func runRecurrenceBenchmark(tb testing.TB, test recurrenceBenchmarkCase) {
	tb.Helper()
	events, truncated, err := ExpandOccurrencesContext(context.Background(), test.master, nil, test.rangeStart, test.rangeEnd, test.maxOccurrences)
	if err != nil {
		tb.Fatalf("%s: %v", test.name, err)
	}
	if len(events) != test.wantCount || truncated != test.wantTruncated {
		tb.Fatalf("%s: count=%d truncated=%v, want count=%d truncated=%v", test.name, len(events), truncated, test.wantCount, test.wantTruncated)
	}
	if test.check != nil {
		test.check(tb, events)
	}
}

func TestCalendarBenchmarkScenarios(t *testing.T) {
	t.Logf("fixture_seed=%s zone=%s", calendarFixtureSeed, calendarFixtureZone)
	for _, test := range recurrenceBenchmarkCases(t) {
		runRecurrenceBenchmark(t, test)
	}
	runImportedUIDBenchmark(t, false)
	runImportedUIDBenchmark(t, true)
	doer := &importedUIDBenchmarkDoer{}
	client := newImportedUIDBenchmarkClient(doer)
	if err := client.Discover(context.Background()); err != nil {
		t.Fatalf("warm discovery: %v", err)
	}
	assertImportedUIDLookup(t, client)
	base := doer.RPCs()
	assertImportedUIDLookup(t, client)
	if got := doer.RPCs() - base; got != 1 {
		t.Fatalf("warm location cache RPCs = %d, want 1", got)
	}
}

func BenchmarkRecurrenceExpansion(b *testing.B) {
	for _, test := range recurrenceBenchmarkCases(b) {
		b.Run(test.name, func(b *testing.B) {
			runRecurrenceBenchmark(b, test)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				runRecurrenceBenchmark(b, test)
			}
			b.StopTimer()
			b.ReportMetric(float64(test.wantCount), "fixture-occurrences")
			b.ReportMetric(float64(test.maxOccurrences), "occurrence-cap")
			if test.wantTruncated {
				b.ReportMetric(1, "truncated")
			} else {
				b.ReportMetric(0, "truncated")
			}
		})
	}
}

const (
	importedUIDBenchmarkCalendar = "/calendars/bench/"
	importedUIDBenchmarkUID      = "bench-imported-uid-0001"
	importedUIDBenchmarkObject   = "server-storage-name.ics"
)

type importedUIDBenchmarkDoer struct {
	mu   sync.Mutex
	rpcs int
}

func (d *importedUIDBenchmarkDoer) count() {
	d.mu.Lock()
	d.rpcs++
	d.mu.Unlock()
}

func (d *importedUIDBenchmarkDoer) RPCs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rpcs
}

func importedUIDBenchmarkCalendarData(uid string) string {
	return strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//icloud-mcp benchmark//EN",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTART:20260701T090000Z",
		"DTEND:20260701T100000Z",
		"SUMMARY:Imported fixture event",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
}

func importedUIDBenchmarkResponse(req *http.Request, status int, contentType, body, etag string) *http.Response {
	header := make(http.Header)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if etag != "" {
		header.Set("ETag", etag)
	}
	return &http.Response{StatusCode: status, Header: header,
		Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func (d *importedUIDBenchmarkDoer) Do(req *http.Request) (*http.Response, error) {
	d.count()
	switch {
	case req.Method == "PROPFIND" && req.URL.Path == "/":
		return importedUIDBenchmarkResponse(req, http.StatusMultiStatus, "application/xml",
			`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop>`+
				`<D:current-user-principal><D:href>/bench-principal/</D:href></D:current-user-principal>`+
				`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`, ""), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/bench-principal/":
		return importedUIDBenchmarkResponse(req, http.StatusMultiStatus, "application/xml",
			`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response>`+
				`<D:href>/bench-principal/</D:href><D:propstat><D:prop><C:calendar-home-set>`+
				`<D:href>/calendars/</D:href></C:calendar-home-set></D:prop>`+
				`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`, ""), nil
	case req.Method == http.MethodGet && req.URL.Path == importedUIDBenchmarkCalendar+importedUIDBenchmarkUID+".ics":
		return importedUIDBenchmarkResponse(req, http.StatusNotFound, "text/plain", "", ""), nil
	case req.Method == "REPORT" && req.URL.Path == importedUIDBenchmarkCalendar:
		calendar := importedUIDBenchmarkCalendarData(importedUIDBenchmarkUID)
		body := `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` +
			`<D:response><D:href>` + importedUIDBenchmarkCalendar + importedUIDBenchmarkObject + `</D:href>` +
			`<D:propstat><D:prop><D:getetag>&quot;bench-etag&quot;</D:getetag>` +
			`<C:calendar-data>` + xmlBenchEscape(calendar) + `</C:calendar-data>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
		return importedUIDBenchmarkResponse(req, http.StatusMultiStatus, "application/xml", body, ""), nil
	case req.Method == http.MethodGet && req.URL.Path == importedUIDBenchmarkCalendar+importedUIDBenchmarkObject:
		return importedUIDBenchmarkResponse(req, http.StatusOK, "text/calendar",
			importedUIDBenchmarkCalendarData(importedUIDBenchmarkUID), `"bench-etag"`), nil
	default:
		return importedUIDBenchmarkResponse(req, http.StatusBadRequest, "text/plain", "unexpected benchmark request", ""), nil
	}
}

func xmlBenchEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}

func newImportedUIDBenchmarkClient(doer httpDoer) *Client {
	return NewClient(doer, "https://caldav.icloud.com", func(host string) bool { return host == "caldav.icloud.com" })
}

func assertImportedUIDLookup(tb testing.TB, client *Client) {
	tb.Helper()
	detail, err := client.GetEvent(context.Background(), importedUIDBenchmarkCalendar, importedUIDBenchmarkUID)
	if err != nil {
		tb.Fatalf("imported UID lookup: %v", err)
	}
	if detail == nil || detail.UID != importedUIDBenchmarkUID || detail.Title != "Imported fixture event" {
		tb.Fatalf("imported UID result = %+v", detail)
	}
	if detail.Path != "" {
		tb.Fatalf("internal path leaked: %q", detail.Path)
	}
}

func runImportedUIDBenchmark(tb testing.TB, warm bool) {
	tb.Helper()
	doer := &importedUIDBenchmarkDoer{}
	client := newImportedUIDBenchmarkClient(doer)
	if warm {
		if err := client.Discover(context.Background()); err != nil {
			tb.Fatalf("warm discovery: %v", err)
		}
	}
	base := doer.RPCs()
	assertImportedUIDLookup(tb, client)
	wantRPC := 5
	if warm {
		wantRPC = 3
	}
	if got := doer.RPCs() - base; got != wantRPC {
		tb.Fatalf("RPCs = %d, want %d", got, wantRPC)
	}
}

func BenchmarkCalendarImportedUID(b *testing.B) {
	b.Run("ColdDiscovery", func(b *testing.B) {
		runImportedUIDBenchmark(b, false)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			runImportedUIDBenchmark(b, false)
		}
		b.StopTimer()
		b.ReportMetric(5, "rpc/op")
		b.ReportMetric(1, "fixture-events")
		b.ReportMetric(1, "fallback-report")
	})
	b.Run("WarmDiscovery", func(b *testing.B) {
		doer := &importedUIDBenchmarkDoer{}
		client := newImportedUIDBenchmarkClient(doer)
		if err := client.Discover(context.Background()); err != nil {
			b.Fatalf("warm discovery: %v", err)
		}
		baseRPC := doer.RPCs()
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			// Measure warm discovery with a cold UID location cache.
			client.locations.evict(importedUIDBenchmarkCalendar, importedUIDBenchmarkUID)
			assertImportedUIDLookup(b, client)
		}
		b.StopTimer()
		if b.N > 0 {
			b.ReportMetric(float64(doer.RPCs()-baseRPC)/float64(b.N), "rpc/op")
		}
		b.ReportMetric(1, "fixture-events")
		b.ReportMetric(1, "fallback-report")
		b.ReportMetric(0, "location-cache")
	})
	b.Run("WarmLocationCache", func(b *testing.B) {
		doer := &importedUIDBenchmarkDoer{}
		client := newImportedUIDBenchmarkClient(doer)
		if err := client.Discover(context.Background()); err != nil {
			b.Fatalf("warm discovery: %v", err)
		}
		assertImportedUIDLookup(b, client)
		baseRPC := doer.RPCs()
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			assertImportedUIDLookup(b, client)
		}
		b.StopTimer()
		if b.N > 0 {
			b.ReportMetric(float64(doer.RPCs()-baseRPC)/float64(b.N), "rpc/op")
		}
		b.ReportMetric(1, "fixture-events")
		b.ReportMetric(1, "fallback-report")
		b.ReportMetric(1, "location-cache")
	})
}
