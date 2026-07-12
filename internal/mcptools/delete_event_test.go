package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestDeleteEventHandler_EchoesDeletedTitle(t *testing.T) {
	svc := &icloud.MockService{DeletedTitle: "Dentist appointment"}
	handler := deleteEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}

	var payload deleteEventResponse
	decodeResult(t, res, &payload)
	if !payload.Success || payload.DeletedTitle != "Dentist appointment" {
		t.Fatalf("payload = %+v", payload)
	}
	if svc.LastDeleteUID != "uid-1" {
		t.Errorf("LastDeleteUID = %q", svc.LastDeleteUID)
	}
}

func TestDeleteEventHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{DeleteErr: fmt.Errorf("event not found")}
	handler := deleteEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "unknown-uid", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result")
	}
}

func TestDeleteEventHandler_AuditNeverContainsTitle(t *testing.T) {
	svc := &icloud.MockService{DeletedTitle: "Very private title"}
	var buf bytes.Buffer
	deps := Deps{
		Service:  svc,
		Audit:    security.NewAuditLogger(&buf),
		Redactor: security.NewRedactor("unused-secret-value"),
	}
	handler := deleteEventHandler(deps)

	_, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}

	logLine := buf.String()
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logLine)), &entry); err != nil {
		t.Fatalf("audit line is not valid JSON: %v\n%s", err, logLine)
	}
	if entry["tool"] != "delete_event" || entry["status"] != "success" || entry["uid"] != "uid-1" {
		t.Errorf("unexpected audit entry: %v", entry)
	}
	if strings.Contains(logLine, "Very private title") {
		t.Fatalf("audit line must NEVER contain the deleted title: %s", logLine)
	}
}

func TestDeleteEventHandler_InvalidCalendarPath(t *testing.T) {
	svc := &icloud.MockService{}
	handler := deleteEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "invalid-path",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an invalid calendar path error")
	}
	if svc.DeleteCallCount != 0 {
		t.Errorf("DeleteEvent should not have been called (validation denied)")
	}
}
