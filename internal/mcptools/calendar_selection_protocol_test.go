package mcptools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/health"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const (
	selectionA       = "/cal/allowed-a/"
	selectionB       = "/cal/allowed-b/"
	selectionDeniedA = "/cal/forbidden-a/"
	selectionDeniedB = "/cal/forbidden-b/"
)

type calendarSelectionProtocol struct {
	client  *client.Client
	service *freeSlotsTestService
	guarded *icloud.GuardedService
	audit   bytes.Buffer
}

func newCalendarSelectionProtocol(t *testing.T, service *freeSlotsTestService, scopes string) *calendarSelectionProtocol {
	t.Helper()
	f := &calendarSelectionProtocol{service: service, guarded: icloud.NewGuardedService(service, 0, time.Millisecond)}
	redactor := security.NewRedactor("synthetic-selection-secret")
	s := server.NewMCPServer("calendar-selection", "test", server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(RecoverRedactMiddleware(redactor)))
	Register(s, Deps{Service: f.guarded, Redactor: redactor, ResourceScopes: mustScopes(t, scopes),
		DefaultLocation: time.UTC, HealthEnabled: true, Audit: security.NewAuditLogger(&f.audit)}, true)
	var err error
	f.client, err = client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Close() })
	if err := f.client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "calendar-selection-test", Version: "test"}
	if _, err := f.client.Initialize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *calendarSelectionProtocol) call(t *testing.T, tool string, extra map[string]any) *mcp.CallToolResult {
	t.Helper()
	args := map[string]any{"start": "2026-07-01T09:00:00Z", "end": "2026-07-01T14:00:00Z"}
	if tool == "find_free_slots" {
		args["duration_minutes"] = 60
	}
	if tool == "calendar_capabilities" || tool == "icloud_capabilities" {
		args = map[string]any{}
	}
	for key, value := range extra {
		args[key] = value
	}
	req := mcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = tool, args
	res, err := f.client.CallTool(t.Context(), req)
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

func (f *calendarSelectionProtocol) assertCalls(t *testing.T, lists int, paths []string) {
	t.Helper()
	want, got := map[string]int{}, map[string]int{}
	for _, path := range paths {
		want[path]++
	}
	for _, call := range f.service.searchSnapshot() {
		got[call.path]++
	}
	t.Logf("backend_list_calls=%d backend_calendar_calls=%v", f.service.listCalls, got)
	if f.service.listCalls != lists || !reflect.DeepEqual(got, want) {
		t.Errorf("backend calls: lists=%d calendars=%v, want lists=%d calendars=%v", f.service.listCalls, got, lists, want)
	}
}

func assertSelectionPrivate(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{selectionDeniedA, selectionDeniedB} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("forbidden path in public output: %s", encoded)
		}
	}
}

func selectionFixture(deniedBusy bool) *freeSlotsTestService {
	start := mustParseTime("2026-07-01T09:00:00Z")
	event := func(uid string, from, to int) icloud.Event {
		return icloud.Event{UID: uid, Title: uid, StartTime: start.Add(time.Duration(from) * time.Hour), EndTime: start.Add(time.Duration(to) * time.Hour)}
	}
	s := &freeSlotsTestService{
		calendars: []icloud.Calendar{{Path: selectionDeniedA}, {Path: selectionA}, {Path: selectionDeniedB}, {Path: selectionB}},
		events:    map[string][]icloud.Event{selectionA: {event("allowed-a", 1, 2)}, selectionB: {event("allowed-b", 2, 3)}},
	}
	if deniedBusy {
		s.events[selectionDeniedA] = []icloud.Event{event("forbidden-a", 0, 1)}
		s.events[selectionDeniedB] = []icloud.Event{event("forbidden-b", 3, 5)}
	}
	return s
}

func TestProtocolCalendarSelectionScopes(t *testing.T) {
	all := []string{selectionDeniedA, selectionA, selectionDeniedB, selectionB}
	for _, tool := range []string{"find_free_slots", "search_events"} {
		t.Run(tool, func(t *testing.T) {
			for _, tc := range []struct {
				name, scope string
				paths       []string
			}{
				{"default", "", all},
				{"wildcard", "calendar.read=*", all},
				{"one-permitted", "calendar.read=" + selectionA, []string{selectionA}},
				{"multiple-permitted", "calendar.read=" + selectionB + "," + selectionA, []string{selectionA, selectionB}},
				{"none-permitted", "calendar.read=/cal/not-discovered/", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var previous string
					for _, deniedBusy := range []bool{false, true} {
						t.Run(fmt.Sprintf("denied-busy-%v", deniedBusy), func(t *testing.T) {
							f := newCalendarSelectionProtocol(t, selectionFixture(deniedBusy), tc.scope)
							res := f.call(t, tool, nil)
							f.assertCalls(t, 1, tc.paths)
							if len(tc.paths) == 0 {
								var failure toolErrorPayload
								decodeResult(t, res, &failure)
								if !res.IsError || failure.Code != "authorization" || strings.Contains(resultText(t, res), "slots") {
									t.Errorf("no permitted selection must fail without availability: %s", resultText(t, res))
								}
							} else if res.IsError {
								t.Fatalf("unexpected error: %s", resultText(t, res))
							} else if tool == "find_free_slots" {
								var got freeSlotsResponse
								decodeResult(t, res, &got)
								want := []freeSlotOutput{
									{Start: "2026-07-01T09:00:00+00:00", End: "2026-07-01T10:00:00+00:00"},
									{Start: "2026-07-01T12:00:00+00:00", End: "2026-07-01T13:00:00+00:00"},
									{Start: "2026-07-01T13:00:00+00:00", End: "2026-07-01T14:00:00+00:00"},
								}
								if len(tc.paths) == 1 {
									want = append(want[:1], append([]freeSlotOutput{{Start: "2026-07-01T11:00:00+00:00", End: "2026-07-01T12:00:00+00:00"}}, want[1:]...)...)
								}
								if len(tc.paths) == 4 && deniedBusy {
									want = []freeSlotOutput{}
								}
								if got.Count != len(want) || !reflect.DeepEqual(got.Slots, want) {
									t.Errorf("availability=%+v, want %+v", got, want)
								}
							} else {
								var got searchEventsResponse
								decodeResult(t, res, &got)
								wantCount := len(tc.paths)
								if len(tc.paths) == 4 && !deniedBusy {
									wantCount = 2
								}
								if got.Count != wantCount || got.Total != wantCount {
									t.Errorf("events=%+v, want count=%d", got, wantCount)
								}
								for _, event := range got.Events {
									if !scopeContains(tc.paths, event.Calendar) {
										t.Errorf("unauthorized search event: %+v", event)
									}
								}
							}
							if len(tc.paths) < 4 {
								assertSelectionPrivate(t, res)
								text := resultText(t, res)
								if deniedBusy && text != previous {
									t.Errorf("denied busy data changed scoped result:\nbefore=%s\nafter=%s", previous, text)
								}
								previous = text
							}
							for _, localTool := range []string{"calendar_capabilities", "icloud_capabilities"} {
								assertSelectionPrivate(t, f.call(t, localTool, nil))
							}
							status := health.Snapshot("test", nil, map[string]any{"calendar": f.guarded.RateLimitStatus()})
							assertSelectionPrivate(t, status)
							assertSelectionPrivate(t, f.audit.String())
							if f.audit.Len() != 0 {
								t.Errorf("read selection wrote mutation audit: %s", f.audit.String())
							}
							f.assertCalls(t, 1, tc.paths)
						})
					}
				})
			}
		})
	}
}

func TestProtocolCalendarSelectionExplicitValidation(t *testing.T) {
	tooMany := make([]string, 11)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("/cal/%d/", i)
	}
	for _, tool := range []string{"find_free_slots", "search_events"} {
		t.Run(tool, func(t *testing.T) {
			for _, tc := range []struct {
				name, scope string
				args        map[string]any
				paths       []string
				failure     string
				lists       int
			}{
				{"single", "calendar.read=" + selectionA, map[string]any{"calendar": selectionA}, []string{selectionA}, "", 0},
				{"default", "", map[string]any{"calendar": selectionDeniedA}, []string{selectionDeniedA}, "", 0},
				{"wildcard", "calendar.read=*", map[string]any{"calendars": selectionA + "," + selectionDeniedA}, []string{selectionA, selectionDeniedA}, "", 0},
				{"merged-deduplicated", "calendar.read=" + selectionA + "," + selectionB, map[string]any{"calendar": selectionA, "calendars": " , " + selectionB + "," + selectionA + "," + selectionB + ", "}, []string{selectionA, selectionB}, "", 0},
				{"denied-single", "calendar.read=" + selectionA, map[string]any{"calendar": selectionDeniedA}, nil, "authorization", 0},
				{"mixed-multi", "calendar.read=" + selectionA, map[string]any{"calendars": selectionA + "," + selectionDeniedA}, nil, "authorization", 0},
				{"denied-single-allowed-multi", "calendar.read=" + selectionA, map[string]any{"calendar": selectionDeniedA, "calendars": selectionA}, nil, "authorization", 0},
				{"allowed-single-denied-multi", "calendar.read=" + selectionA, map[string]any{"calendar": selectionA, "calendars": selectionDeniedB}, nil, "authorization", 0},
				{"invalid-path", "calendar.read=*", map[string]any{"calendar": "//invalid.example/path"}, nil, "path-absolute", 0},
				{"too-many", "calendar.read=*", map[string]any{"calendars": strings.Join(tooMany, ",")}, nil, "at most 10", 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newCalendarSelectionProtocol(t, selectionFixture(true), tc.scope)
					res := f.call(t, tool, tc.args)
					if res.IsError != (tc.failure != "") || !strings.Contains(resultText(t, res), tc.failure) {
						t.Errorf("unexpected result: %s", resultText(t, res))
					}
					if tc.failure != "" {
						assertSelectionPrivate(t, res)
					}
					f.assertCalls(t, tc.lists, tc.paths)
				})
			}
			for _, args := range []map[string]any{{"calendar": ""}, {"calendars": " , "}, {"calendar": "", "calendars": ""}} {
				t.Run(fmt.Sprint(args), func(t *testing.T) {
					f := newCalendarSelectionProtocol(t, selectionFixture(true), "calendar.read="+selectionA)
					res := f.call(t, tool, args)
					if tool == "find_free_slots" {
						if !res.IsError || !strings.Contains(resultText(t, res), "at least one non-empty path") {
							t.Errorf("empty explicit availability selection=%s", resultText(t, res))
						}
						f.assertCalls(t, 0, nil)
					} else {
						if res.IsError {
							t.Errorf("empty search selection must discover: %s", resultText(t, res))
						}
						f.assertCalls(t, 1, []string{selectionA})
					}
				})
			}
		})
	}
}

func TestProtocolCalendarSelectionOrdering(t *testing.T) {
	for _, tool := range []string{"find_free_slots", "search_events"} {
		for _, implicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/implicit-%v", tool, implicit), func(t *testing.T) {
				svc := selectionFixture(true)
				svc.searchErrs = map[string]error{selectionA: errors.New("first permitted unavailable"), selectionB: errors.New("second permitted unavailable")}
				f := newCalendarSelectionProtocol(t, svc, "calendar.read="+selectionA+","+selectionB)
				args := map[string]any{"calendar": selectionA, "calendars": selectionB}
				if implicit {
					args = nil
				}
				res := f.call(t, tool, args)
				first := "first permitted unavailable"
				if tool == "find_free_slots" && !implicit {
					first = "second permitted unavailable"
				}
				text := resultText(t, res)
				if !res.IsError || !strings.Contains(text, first) {
					t.Errorf("ordered failure=%s, want %s", text, first)
				}
				if tool == "search_events" && !strings.Contains(text, "calendar[0] query failed: first permitted unavailable") {
					t.Errorf("search index order=%s", text)
				}
				assertSelectionPrivate(t, res)
				lists := 0
				if implicit {
					lists = 1
				}
				f.assertCalls(t, lists, []string{selectionA, selectionB})
			})
		}
	}
}

func TestProtocolCalendarSelectionDiscoveryBounds(t *testing.T) {
	for _, tool := range []string{"find_free_slots", "search_events"} {
		t.Run(tool, func(t *testing.T) {
			t.Run("empty", func(t *testing.T) {
				f := newCalendarSelectionProtocol(t, &freeSlotsTestService{}, "")
				res := f.call(t, tool, nil)
				if res.IsError != (tool == "find_free_slots") {
					t.Errorf("empty discovery changed: %s", resultText(t, res))
				}
				f.assertCalls(t, 1, nil)
			})
			t.Run("empty-restricted", func(t *testing.T) {
				f := newCalendarSelectionProtocol(t, &freeSlotsTestService{}, "calendar.read="+selectionA)
				res := f.call(t, tool, nil)
				if !res.IsError {
					t.Errorf("empty scoped discovery must fail: %s", resultText(t, res))
				}
				f.assertCalls(t, 1, nil)
			})
			t.Run("discovery-not-explicit-cap", func(t *testing.T) {
				svc := &freeSlotsTestService{}
				var paths []string
				for i := 0; i < 11; i++ {
					path := fmt.Sprintf("/cal/%d/", i)
					paths = append(paths, path)
					svc.calendars = append(svc.calendars, icloud.Calendar{Path: path})
				}
				f := newCalendarSelectionProtocol(t, svc, "calendar.read=*")
				res := f.call(t, tool, nil)
				if res.IsError {
					t.Errorf("discovery must not inherit explicit cap: %s", resultText(t, res))
				}
				f.assertCalls(t, 1, paths)
			})
		})
	}
}
