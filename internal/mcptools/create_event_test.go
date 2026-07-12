package mcptools

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestCreateEventHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "new-uid-123"}
	handler := createEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"title":    "Meeting",
		"start":    "2026-07-01T10:00:00Z",
		"end":      "2026-07-01T11:00:00Z",
		"calendar": "/cal/home/",
		"location": "Room B",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}

	var payload createEventResponse
	decodeResult(t, res, &payload)
	if !payload.Success || payload.UID != "new-uid-123" {
		t.Fatalf("payload = %+v", payload)
	}
	if svc.LastCreated == nil || svc.LastCreated.Title != "Meeting" {
		t.Fatalf("LastCreated = %+v", svc.LastCreated)
	}
}

func TestCreateEventHandler_ValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{
			name: "start >= end",
			args: map[string]any{"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T09:00:00Z", "calendar": "/cal/"},
		},
		{
			name: "invalid calendar path",
			args: map[string]any{"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z", "calendar": "no-slash"},
		},
		{
			name: "invalid date",
			args: map[string]any{"title": "x", "start": "not a date", "end": "2026-07-01T11:00:00Z", "calendar": "/cal/"},
		},
		{
			name: "alarm out of bounds",
			args: map[string]any{"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z", "calendar": "/cal/", "alarm_minutes_before": float64(-5)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &icloud.MockService{}
			handler := createEventHandler(testDeps(svc))
			res, err := handler(context.Background(), newReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected an error result for %s", tt.name)
			}
			if svc.CreateCallCount != 0 {
				t.Errorf("CreateEvent should not have been called (validation denied)")
			}
		})
	}
}

func TestCreateEventHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{CreateErr: fmt.Errorf("iCloud unavailable")}
	handler := createEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"title": "x", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z", "calendar": "/cal/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(resultText(t, res), "iCloud unavailable") {
		t.Errorf("expected message containing 'iCloud unavailable': %s", resultText(t, res))
	}
}

func TestCreateEventHandler_AuditLogged(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "audit-uid"}
	var buf bytes.Buffer
	deps := Deps{
		Service:  svc,
		Audit:    security.NewAuditLogger(&buf),
		Redactor: security.NewRedactor("unused-secret-value"),
	}
	handler := createEventHandler(deps)

	_, err := handler(context.Background(), newReq(map[string]any{
		"title": "Secret title", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}

	logLine := buf.String()
	if !strings.Contains(logLine, "tool=create_event") || !strings.Contains(logLine, "status=success") {
		t.Errorf("unexpected audit line: %s", logLine)
	}
	if !strings.Contains(logLine, "uid=audit-uid") {
		t.Errorf("audit line should contain the uid: %s", logLine)
	}
	if strings.Contains(logLine, "Secret title") {
		t.Errorf("audit line must NEVER contain the title: %s", logLine)
	}
}
