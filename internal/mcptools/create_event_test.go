package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// TestCreateEventHandler_LocalTimeUsesConfiguredDefaultTZ is the end-to-end
// regression lock for the 2026-07-12 "Grand ménage" timezone incident: the
// user confirmed an event "10h à 14h" (Europe/Paris local time), and the
// calling agent MUST now be able to pass that literal hour with no offset
// ("2026-07-12T10:00:00") and have the server resolve it, via the
// deployment's ICLOUD_MCP_DEFAULT_TZ (Deps.DefaultLocation), to the correct
// UTC instant handed to icloud.Service.CreateEvent (08:00 UTC, CEST = UTC+2)
// instead of the buggy literal-UTC interpretation (10:00 UTC, which iCloud
// would have rendered as 12h, 2h late).
func TestCreateEventHandler_LocalTimeUsesConfiguredDefaultTZ(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("LoadLocation(Europe/Paris): %v", err)
	}
	svc := &icloud.MockService{CreatedUID: "grand-menage-uid"}
	deps := testDeps(svc)
	deps.DefaultLocation = paris
	handler := createEventHandler(deps)

	res, err := handler(context.Background(), newReq(map[string]any{
		"title":    "Grand ménage",
		"start":    "2026-07-12T10:00:00",
		"end":      "2026-07-12T14:00:00",
		"calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}

	if svc.LastCreated == nil {
		t.Fatal("CreateEvent was not called")
	}
	wantStart := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC) // 10h Paris (CEST, UTC+2) = 08h UTC
	wantEnd := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)  // 14h Paris = 12h UTC
	if !svc.LastCreated.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v (2h shift bug: was previously 10:00 UTC instead of 08:00 UTC)", svc.LastCreated.StartTime, wantStart)
	}
	if !svc.LastCreated.EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v", svc.LastCreated.EndTime, wantEnd)
	}
}

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
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logLine)), &entry); err != nil {
		t.Fatalf("audit line is not valid JSON: %v\n%s", err, logLine)
	}
	if entry["tool"] != "create_event" || entry["status"] != "success" {
		t.Errorf("unexpected audit entry: %v", entry)
	}
	if entry["uid"] != "audit-uid" {
		t.Errorf("audit entry should contain the uid: %v", entry)
	}
	if strings.Contains(logLine, "Secret title") {
		t.Errorf("audit line must NEVER contain the title: %s", logLine)
	}
}
