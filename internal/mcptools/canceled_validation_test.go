package mcptools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// canceledSelectorRule needs selector work that observes the context.
const canceledSelectorRule = "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=2"

// TestCanceledValidationIsTimeout proves that a tool deadline during local
// validation returns timeout, not validation, and sends no request.
func TestCanceledValidationIsTimeout(t *testing.T) {
	event := map[string]any{
		"calendar": "/cal/home/", "title": "Synthetic",
		"start": "2026-01-01T09:00:00Z", "end": "2026-01-01T10:00:00Z",
		"rrule": canceledSelectorRule,
	}
	for name, handler := range map[string]func(Deps) func(context.Context, map[string]any) (string, bool, error){
		"create_event": func(deps Deps) func(context.Context, map[string]any) (string, bool, error) {
			return func(ctx context.Context, args map[string]any) (string, bool, error) {
				res, err := createEventHandler(deps)(ctx, newReq(args))
				if err != nil {
					return "", false, err
				}
				return resultText(t, res), res.IsError, nil
			}
		},
		"validate_event": func(deps Deps) func(context.Context, map[string]any) (string, bool, error) {
			return func(ctx context.Context, args map[string]any) (string, bool, error) {
				res, err := validateEventHandler(deps)(ctx, newReq(args))
				if err != nil {
					return "", false, err
				}
				return resultText(t, res), res.IsError, nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			svc := &icloud.MockService{}
			text, isError, err := handler(testDeps(svc))(ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			}
			if !isError || json.Unmarshal([]byte(text), &payload) != nil || payload.Code != string(icloud.CodeTimeout) || payload.Retryable {
				t.Fatalf("canceled validation = %s, want timeout that is not retryable", text)
			}
			if svc.CreateCallCount != 0 || svc.ListCallCount != 0 {
				t.Fatalf("canceled validation reached the service: create=%d list=%d", svc.CreateCallCount, svc.ListCallCount)
			}
		})
	}
}
