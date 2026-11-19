package mcptools

import (
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// TestUpdateEventSchemaDocumentsValueForms states the accepted update value
// forms in the advertised schema. A DATE-TIME input for an all-day event is
// rejected, so the description must say that YYYY-MM-DD is required there.
func TestUpdateEventSchemaDocumentsValueForms(t *testing.T) {
	tool := newUpdateEventTool(time.UTC)
	description := strings.ToLower(tool.Description)
	for _, required := range []string{"value form", "all-day"} {
		if !strings.Contains(description, required) {
			t.Errorf("update_event description omits %q: %q", required, tool.Description)
		}
	}
	for _, param := range []string{"start", "end"} {
		property, ok := tool.InputSchema.Properties[param]
		if !ok {
			t.Fatalf("update_event schema has no %q property", param)
		}
		schema, ok := property.(map[string]any)
		if !ok {
			t.Fatalf("%s schema = %T", param, property)
		}
		text, _ := schema["description"].(string)
		lower := strings.ToLower(text)
		if !strings.Contains(lower, "yyyy-mm-dd") || !strings.Contains(lower, "all-day") {
			t.Errorf("%s description does not state the DATE form rule: %q", param, text)
		}
		if !strings.Contains(lower, "rejected") && !strings.Contains(lower, "fails") {
			t.Errorf("%s description does not state that a mismatch is rejected: %q", param, text)
		}
	}
}

// TestUpdateEventHandlerSurfacesDateFormValidation keeps the typed validation
// code visible through MCP dispatch.
func TestUpdateEventHandlerSurfacesDateFormValidation(t *testing.T) {
	svc := &icloud.MockService{UpdateErr: func() error {
		err := icloud.NewValidationError("start is a DATE-TIME value but the existing event is all-day (VALUE=DATE)")
		err.Details = map[string]string{"reconciliation": "Send start as YYYY-MM-DD."}
		return err
	}()}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(t.Context(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
		"start": "2026-07-03T23:30:00+02:00",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a validation error result")
	}
	text := resultText(t, res)
	if !strings.Contains(text, `"code":"validation"`) {
		t.Errorf("result does not expose the validation code: %s", text)
	}
	if !strings.Contains(text, "VALUE=DATE") {
		t.Errorf("result lost the value-form message: %s", text)
	}
}
