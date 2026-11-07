package mcptools

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

type availabilityBudgetService struct {
	*icloud.MockService
	read   func(context.Context, string) (icloud.SearchResult, error)
	active atomic.Int32
	max    atomic.Int32
	mu     sync.Mutex
	paths  []string
}

func (s *availabilityBudgetService) SearchEvents(ctx context.Context, path string, _, _ time.Time, _ *icloud.SearchOptions) (icloud.SearchResult, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for old := s.max.Load(); active > old; old = s.max.Load() {
		if s.max.CompareAndSwap(old, active) {
			break
		}
	}
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	return s.read(ctx, path)
}

func (s *availabilityBudgetService) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func availabilityBudgetFixture(counts []int) (*availabilityBudgetService, []string) {
	paths := make([]string, len(counts))
	calendars := make([]icloud.Calendar, len(counts))
	byPath := make(map[string][]icloud.Event)
	start := mustParseTime("2026-07-01T09:00:00Z")
	for i, count := range counts {
		path := fmt.Sprintf("/cal/budget-%04d/", i)
		paths[i], calendars[i] = path, icloud.Calendar{Path: path}
		events := make([]icloud.Event, count)
		for j := range events {
			events[j] = icloud.Event{UID: "private-budget-uid", Title: "private-budget-title", Notes: "private-budget-notes",
				StartTime: start, EndTime: start.Add(time.Hour)}
		}
		byPath[path] = events
	}
	svc := &availabilityBudgetService{MockService: &icloud.MockService{Calendars: calendars}}
	svc.read = func(_ context.Context, path string) (icloud.SearchResult, error) {
		return icloud.SearchResult{Events: byPath[path]}, nil
	}
	return svc, paths
}

func newAvailabilityBudgetProtocol(t *testing.T, service icloud.Service, scope string) *client.Client {
	t.Helper()
	deps := freeSlotsTestDeps(service)
	deps.ResourceScopes = mustScopes(t, scope)
	s := server.NewMCPServer("availability-budget", "test", server.WithToolCapabilities(false))
	Register(s, deps, true)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "availability-budget", Version: "test"}
	if _, err := c.Initialize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	return c
}

func callAvailabilityBudget(t *testing.T, c *client.Client, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := freeSlotsTestRequest(mustParseTime("2026-07-01T09:00:00Z"), mustParseTime("2026-07-01T14:00:00Z"), args)
	req.Params.Name = "find_free_slots"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"request": req, "response": res})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	return res
}

func assertAvailabilityBudgetError(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	var payload toolErrorPayload
	decodeResult(t, res, &payload)
	text := resultText(t, res)
	if !res.IsError || payload.Code != "payload_too_large" || !strings.Contains(text, "narrow") || !strings.Contains(text, "calendars") {
		t.Errorf("expected safe aggregate-budget error with narrowing guidance, got %s", text)
	}
	for _, private := range []string{"private-budget", "/cal/", "slots", "events\""} {
		if strings.Contains(text, private) {
			t.Errorf("budget failure exposed partial data: %s", text)
		}
	}
}

func TestProtocolAvailabilityAggregateBudget(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("implicit-%v/events-%d", implicit, 10000+extra), func(t *testing.T) {
				svc, paths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500, extra})
				args := map[string]any{"duration_minutes": 60}
				if !implicit {
					args["calendars"] = strings.Join(paths, ",")
				}
				res := callAvailabilityBudget(t, newAvailabilityBudgetProtocol(t, svc, ""), args)
				if extra == 1 {
					assertAvailabilityBudgetError(t, res)
				} else {
					var got freeSlotsResponse
					decodeResult(t, res, &got)
					if res.IsError || got.Count != 4 || got.Slots[0].Start != "2026-07-01T10:00:00+00:00" {
						t.Fatalf("threshold availability=%s", resultText(t, res))
					}
				}
				t.Logf("calendars=%d events=%d calls=%d max_active=%d active_after=%d", len(paths), 10000+extra, len(svc.snapshot()), svc.max.Load(), svc.active.Load())
				if svc.max.Load() > 4 || svc.active.Load() != 0 {
					t.Fatal("unbounded or detached reads")
				}
			})
		}
	}
}

func TestProtocolAvailabilityBudgetStopsRetention(t *testing.T) {
	counts := make([]int, 64)
	for i := range counts {
		counts[i] = 2500
	}
	svc, paths := availabilityBudgetFixture(counts)
	res := callAvailabilityBudget(t, newAvailabilityBudgetProtocol(t, svc, ""), nil)
	assertAvailabilityBudgetError(t, res)
	got := svc.snapshot()
	t.Logf("implicit_calendars=%d per_calendar=2500 calls=%d max_active=%d", len(paths), len(got), svc.max.Load())
	if len(got) > 8 || len(got) < 5 {
		t.Fatalf("overflow must stop within the second four-read window, calls=%d", len(got))
	}
}

func TestProtocolAvailabilityBudgetCountsBeforeBusyFiltering(t *testing.T) {
	svc, paths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500, 1})
	read := svc.read
	svc.read = func(ctx context.Context, path string) (icloud.SearchResult, error) {
		res, err := read(ctx, path)
		for i := range res.Events {
			res.Events[i].Transp = "TRANSPARENT"
			res.Events[i].AllDay = true
		}
		return res, err
	}
	res := callAvailabilityBudget(t, newAvailabilityBudgetProtocol(t, svc, ""), map[string]any{
		"calendars": strings.Join(paths, ","), "include_all_day_busy": false,
	})
	assertAvailabilityBudgetError(t, res)
}

func TestProtocolAvailabilityBudgetScopeFirst(t *testing.T) {
	for _, deniedCount := range []int{0, 2500} {
		svc, paths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500, deniedCount})
		c := newAvailabilityBudgetProtocol(t, svc, "calendar.read="+strings.Join(paths[:4], ","))
		res := callAvailabilityBudget(t, c, map[string]any{"duration_minutes": 60})
		var got freeSlotsResponse
		decodeResult(t, res, &got)
		if res.IsError || got.Count != 4 || len(svc.snapshot()) != 4 {
			t.Fatalf("denied data consumed budget: %s calls=%v", resultText(t, res), svc.snapshot())
		}
		for _, path := range svc.snapshot() {
			if path == paths[4] {
				t.Fatal("queried forbidden calendar")
			}
		}
		res = callAvailabilityBudget(t, c, map[string]any{"calendars": strings.Join(paths, ",")})
		if !res.IsError || strings.Contains(resultText(t, res), paths[4]) || len(svc.snapshot()) != 4 {
			t.Fatalf("explicit denial did not precede accounting: %s", resultText(t, res))
		}
	}
}

func TestProtocolAvailabilityBudgetOrderedFailures(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, first := range []string{"failure", "truncated", "overflow", "success"} {
			t.Run(fmt.Sprintf("%s/reverse-%v", first, reverse), func(t *testing.T) {
				svc, paths := availabilityBudgetFixture([]int{2500, 2500, 2500, 2499, 2, 0})
				read := svc.read
				svc.read = func(ctx context.Context, path string) (icloud.SearchResult, error) {
					delay := time.Millisecond
					if (path == paths[4]) != reverse {
						delay = 5 * time.Millisecond
					}
					select {
					case <-ctx.Done():
						return icloud.SearchResult{}, ctx.Err()
					case <-time.After(delay):
					}
					if path == paths[4] {
						switch first {
						case "failure":
							return icloud.SearchResult{}, errors.New("first calendar unavailable")
						case "truncated":
							return icloud.SearchResult{TruncatedByExpansion: true}, nil
						case "success":
							return icloud.SearchResult{}, nil
						}
					}
					if path == paths[5] && first != "success" {
						return icloud.SearchResult{}, errors.New("later calendar unavailable")
					}
					return read(ctx, path)
				}
				res := callAvailabilityBudget(t, newAvailabilityBudgetProtocol(t, svc, ""), map[string]any{"calendars": strings.Join(paths, ","), "duration_minutes": 60})
				text := resultText(t, res)
				switch first {
				case "overflow":
					assertAvailabilityBudgetError(t, res)
				case "success":
					var got freeSlotsResponse
					decodeResult(t, res, &got)
					if res.IsError || got.Count != 4 {
						t.Fatalf("reordered success=%s", text)
					}
				default:
					want := "first calendar unavailable"
					if first == "truncated" {
						want = "recurrence expansion was truncated"
					}
					if !res.IsError || !strings.Contains(text, want) || strings.Contains(text, "slots") || strings.Contains(text, "later calendar") {
						t.Fatalf("completion order changed first failure: %s", text)
					}
				}
			})
		}
	}
}

func TestProtocolAvailabilityManyEmptyCalendars(t *testing.T) {
	svc, paths := availabilityBudgetFixture(make([]int, 1024))
	res := callAvailabilityBudget(t, newAvailabilityBudgetProtocol(t, svc, ""), map[string]any{"duration_minutes": 60})
	var got freeSlotsResponse
	decodeResult(t, res, &got)
	if res.IsError || got.Count != 5 || len(svc.snapshot()) != len(paths) || svc.max.Load() > 4 || svc.active.Load() != 0 {
		t.Fatalf("empty fan-out result=%s calls=%d max=%d active=%d", resultText(t, res), len(svc.snapshot()), svc.max.Load(), svc.active.Load())
	}
	t.Logf("empty_calendars=%d calls=%d max_active=%d", len(paths), len(svc.snapshot()), svc.max.Load())
}

// Direct invocation observes worker cleanup after handler return, which client cancellation cannot establish.
func TestAvailabilityBudgetCancellationJoinsReads(t *testing.T) {
	svc, _ := availabilityBudgetFixture(make([]int, 1024))
	started := make(chan struct{}, 4)
	svc.read = func(ctx context.Context, _ string) (icloud.SearchResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return icloud.SearchResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := findFreeSlotsHandler(freeSlotsTestDeps(svc))(ctx, freeSlotsTestRequest(
			mustParseTime("2026-07-01T09:00:00Z"), mustParseTime("2026-07-01T14:00:00Z"), nil))
		done <- res
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("four workers did not start")
		}
	}
	start := time.Now()
	cancel()
	select {
	case res := <-done:
		if !res.IsError || strings.Contains(resultText(t, res), "slots") || svc.active.Load() != 0 || len(svc.snapshot()) != 4 {
			t.Fatalf("cancellation result=%s active=%d calls=%d", resultText(t, res), svc.active.Load(), len(svc.snapshot()))
		}
		t.Logf("cancel_elapsed=%s calls=%d active_after=%d", time.Since(start), len(svc.snapshot()), svc.active.Load())
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not join bounded reads")
	}
}

func availabilityBudgetReport(calendar string) string {
	var body strings.Builder
	body.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:response><D:href>event.ics</D:href><D:propstat><D:prop><D:getetag>"budget"</D:getetag><C:calendar-data>`)
	if err := xml.EscapeText(&body, []byte(calendar)); err != nil {
		panic(err)
	}
	body.WriteString(`</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	return body.String()
}

func TestProtocolAvailabilityRealClientAggregateBudget(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(10000+extra), func(t *testing.T) {
			collections := map[string][]string{}
			var paths []string
			for i := range 6 {
				path := fmt.Sprintf("%sbudget-%d/", recurrenceProtocolHome, i)
				paths = append(paths, path)
				if i < 5 {
					collections[path] = []string{"minutes-2000.ics"}
				} else {
					collections[path] = nil
				}
			}
			p := newRecurrenceProtocol(t, "UTC", collections)
			if extra == 1 {
				p.dav.reports[paths[5]] = availabilityBudgetReport("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:private-budget-uid\r\nSUMMARY:private-budget-title\r\nDTSTART:20260102T100000Z\r\nDTEND:20260102T110000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
			}
			res := p.call(t, "find_free_slots", map[string]any{"calendars": strings.Join(paths, ","),
				"start": "2026-01-01T00:00:00Z", "end": "2026-01-02T12:00:00Z", "duration_minutes": 60})
			if extra == 1 {
				var result mcp.CallToolResult
				if err := json.Unmarshal(res, &result); err != nil {
					t.Fatal(err)
				}
				assertAvailabilityBudgetError(t, &result)
			} else {
				assertRecurrenceProtocolResult(t, res, false, json.RawMessage(`{"count":2,"slots":[{"start":"2026-01-02T09:20:00+00:00","end":"2026-01-02T10:20:00+00:00"},{"start":"2026-01-02T10:20:00+00:00","end":"2026-01-02T11:20:00+00:00"}]}`))
			}
		})
	}
}

func TestProtocolAvailabilityBudgetLargeBuffers(t *testing.T) {
	svc, _ := availabilityBudgetFixture([]int{2500, 2500, 2500, 2500})
	res := callAvailabilityBudget(t, newAvailabilityBudgetProtocol(t, svc, ""), map[string]any{
		"buffer_before_minutes": 1440, "buffer_after_minutes": 1440,
	})
	var got freeSlotsResponse
	decodeResult(t, res, &got)
	if res.IsError || !reflect.DeepEqual(got, freeSlotsResponse{Slots: []freeSlotOutput{}}) {
		t.Fatalf("large buffers exposed availability: %s", resultText(t, res))
	}
}
