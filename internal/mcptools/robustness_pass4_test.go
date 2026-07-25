package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestDeleteEventStrictControlsDenyAndAudit(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "dry run wrong type", args: map[string]any{"dry_run": "true"}},
		{name: "scope wrong type", args: map[string]any{"scope": true}},
		{name: "recurrence id wrong type", args: map[string]any{"scope": "occurrence", "recurrence_id": 1.5}},
		{name: "recurrence id without occurrence scope", args: map[string]any{"recurrence_id": "2026-07-01T10:00:00Z"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &icloud.MockService{}
			var audit bytes.Buffer
			deps := Deps{
				Service: svc, Audit: security.NewAuditLogger(&audit),
				Redactor: security.NewRedactor("unused-secret"),
			}
			args := map[string]any{"uid": "uid-1", "calendar": "/cal/home/"}
			for key, value := range tc.args {
				args[key] = value
			}
			result, err := deleteEventHandler(deps)(context.Background(), newReq(args))
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			var payload toolErrorPayload
			if err := json.Unmarshal([]byte(resultText(t, result)), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Code != string(icloud.CodeValidation) {
				t.Fatalf("code=%q, want validation", payload.Code)
			}
			if svc.DeleteCallCount != 0 || !strings.Contains(audit.String(), `"status":"denied"`) {
				t.Fatalf("delete calls=%d audit=%s", svc.DeleteCallCount, audit.String())
			}
		})
	}
}

func TestUpdateEventStrictScopeControlsDenyAndAudit(t *testing.T) {
	for _, args := range []map[string]any{
		{"uid": "uid-1", "calendar": "/cal/home/", "title": "x", "scope": false},
		{"uid": "uid-1", "calendar": "/cal/home/", "title": "x", "scope": "occurrence", "recurrence_id": 1.5},
	} {
		svc := &icloud.MockService{}
		var audit bytes.Buffer
		deps := Deps{Service: svc, Audit: security.NewAuditLogger(&audit), Redactor: security.NewRedactor("unused-secret")}
		result, err := updateEventHandler(deps)(context.Background(), newReq(args))
		if err != nil || result == nil || !result.IsError || svc.UpdateCallCount != 0 {
			t.Fatalf("result=%+v err=%v calls=%d", result, err, svc.UpdateCallCount)
		}
		if !strings.Contains(audit.String(), `"status":"denied"`) {
			t.Fatalf("missing denied audit: %s", audit.String())
		}
	}
}

func TestCalendarNumericArgumentsRejectFractions(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*icloud.MockService) server.ToolHandlerFunc
		args    map[string]any
		calls   func(*icloud.MockService) int
	}{
		{
			name: "create alarm", handler: func(s *icloud.MockService) server.ToolHandlerFunc { return createEventHandler(testDeps(s)) },
			args: map[string]any{
				"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
				"calendar": "/cal/home/", "alarm_minutes_before": 1.5,
			},
			calls: func(s *icloud.MockService) int { return s.CreateCallCount },
		},
		{
			name: "create recurrence interval", handler: func(s *icloud.MockService) server.ToolHandlerFunc { return createEventHandler(testDeps(s)) },
			args: map[string]any{
				"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
				"calendar": "/cal/home/", "recurrence_frequency": "daily", "recurrence_interval": 1.5,
			},
			calls: func(s *icloud.MockService) int { return s.CreateCallCount },
		},
		{
			name: "search limit", handler: func(s *icloud.MockService) server.ToolHandlerFunc { return searchEventsHandler(testDeps(s)) },
			args: map[string]any{
				"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
				"calendar": "/cal/home/", "limit": 1.5,
			},
			calls: func(s *icloud.MockService) int { return s.SearchCallCount },
		},
		{
			name: "free slot duration", handler: func(s *icloud.MockService) server.ToolHandlerFunc { return findFreeSlotsHandler(testDeps(s)) },
			args: map[string]any{
				"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
				"calendar": "/cal/home/", "duration_minutes": 30.5,
			},
			calls: func(s *icloud.MockService) int { return s.SearchCallCount },
		},
		{
			name: "validate recurrence count", handler: func(s *icloud.MockService) server.ToolHandlerFunc { return validateEventHandler(testDeps(s)) },
			args: map[string]any{
				"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
				"recurrence_frequency": "daily", "recurrence_count": 2.5,
			},
			calls: func(*icloud.MockService) int { return 0 },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &icloud.MockService{}
			result, err := tc.handler(svc)(context.Background(), newReq(tc.args))
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if calls := tc.calls(svc); calls != 0 {
				t.Fatalf("service calls=%d, want 0", calls)
			}
		})
	}
}

func TestCalendarNumericSchemasUseInteger(t *testing.T) {
	tests := []struct {
		tool  mcp.Tool
		names []string
	}{
		{newCreateEventTool(time.UTC), []string{"alarm_minutes_before", "recurrence_interval", "recurrence_count"}},
		{newValidateEventTool(time.UTC), []string{"alarm_minutes_before", "recurrence_interval", "recurrence_count"}},
		{newSearchEventsTool(time.UTC), []string{"limit", "offset"}},
		{newFindFreeSlotsTool(time.UTC), []string{"duration_minutes", "buffer_before_minutes", "buffer_after_minutes", "limit"}},
	}
	for _, tc := range tests {
		for _, name := range tc.names {
			property, ok := tc.tool.InputSchema.Properties[name].(map[string]any)
			if !ok || property["type"] != "integer" {
				t.Errorf("%s.%s schema = %#v, want integer", tc.tool.Name, name, tc.tool.InputSchema.Properties[name])
			}
		}
	}
}

func TestErrorResultsAreBoundedAndUTF8Safe(t *testing.T) {
	attacker := strings.Repeat("\u754c", maxCalendarResultBytes)
	result := errResult(nil, "validation", errors.New(attacker))
	text := resultText(t, result)
	if len(text) > maxCalendarResultBytes {
		t.Fatalf("error result is %d bytes", len(text))
	}
	if !utf8.ValidString(text) {
		t.Fatal("error result is not valid UTF-8")
	}
	if strings.Contains(text, attacker) || !strings.Contains(text, "[truncated]") {
		t.Fatal("attacker input was not safely truncated")
	}
}

func TestRecoveryWithNilRedactorCannotRepanic(t *testing.T) {
	handler := RecoverRedactMiddleware(nil)(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		panic(fmt.Errorf("panic sentinel"))
	})
	result, err := handler(context.Background(), mcp.CallToolRequest{})
	if err != nil || result == nil || !result.IsError || !strings.Contains(resultText(t, result), "panic sentinel") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCreateEventRuntimeByteLimitRejectsMultibyteInput(t *testing.T) {
	svc := &icloud.MockService{}
	result, err := createEventHandler(testDeps(svc))(context.Background(), newReq(map[string]any{
		"title": strings.Repeat("\u00e9", 251), "start": "2026-07-01T10:00:00Z",
		"end": "2026-07-01T11:00:00Z", "calendar": "/cal/home/",
	}))
	if err != nil || result == nil || !result.IsError || svc.CreateCallCount != 0 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, svc.CreateCallCount)
	}
	titleSchema := newCreateEventTool(time.UTC).InputSchema.Properties["title"].(map[string]any)
	if !strings.Contains(titleSchema["description"].(string), "UTF-8 bytes") {
		t.Fatalf("title description does not clarify byte limit: %v", titleSchema["description"])
	}
}

func TestRegisterUnifiedFailsFastOnNilDependencies(t *testing.T) {
	validDeps := func() Deps {
		return Deps{
			Service: &icloud.MockService{}, Audit: security.NewAuditLogger(&discardWriter{}),
			Redactor: security.NewRedactor("unused-secret"),
		}
	}
	tests := []struct {
		name string
		deps func() Deps
		plan CapabilityPlan
	}{
		{name: "calendar service", deps: func() Deps { d := validDeps(); d.Service = nil; return d }, plan: NewCapabilityPlan(true, false, false, false, false)},
		{name: "redactor", deps: func() Deps { d := validDeps(); d.Redactor = nil; return d }, plan: NewCapabilityPlan(true, false, false, false, false)},
		{name: "mutation audit", deps: func() Deps { d := validDeps(); d.Audit = nil; return d }, plan: NewCapabilityPlan(false, false, false, false, false)},
		{name: "contacts service", deps: validDeps, plan: NewCapabilityPlan(true, true, false, false, false)},
		{name: "mail service", deps: validDeps, plan: NewCapabilityPlan(true, false, true, false, false)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "mcptools:") {
					t.Fatalf("panic = %v, want clear mcptools dependency panic", recovered)
				}
			}()
			RegisterUnified(server.NewMCPServer("test", "test"), tc.deps(), tc.plan)
		})
	}
}

func TestRegisterUnifiedReadOnlyDoesNotRequireAudit(t *testing.T) {
	deps := Deps{Service: &icloud.MockService{}, Redactor: security.NewRedactor("unused-secret")}
	RegisterUnified(server.NewMCPServer("test", "test"), deps, NewCapabilityPlan(true, false, false, false, false))
}

func TestRegisterUnifiedRejectsNilServer(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "nil MCP server") {
			t.Fatalf("panic = %v", recovered)
		}
	}()
	RegisterUnified(nil, Deps{}, NewCapabilityPlan(true, false, false, false, false))
}
