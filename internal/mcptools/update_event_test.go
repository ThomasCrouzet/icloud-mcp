package mcptools

import (
	"context"
	"fmt"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestUpdateEventHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
		"title":    "New title",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}
	if svc.LastUpdate == nil || svc.LastUpdate.Title == nil || *svc.LastUpdate.Title != "New title" {
		t.Fatalf("LastUpdate = %+v", svc.LastUpdate)
	}
}

func TestUpdateEventHandler_DistinguishesAbsentFromEmpty(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	_, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
		"location": "", // provided and empty: clears the value
		// notes absent: unchanged
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	up := svc.LastUpdate
	if up == nil {
		t.Fatal("UpdateEvent not called")
	}
	if up.Location == nil || *up.Location != "" {
		t.Errorf("Location = %+v, want pointer to empty string (clear)", up.Location)
	}
	if up.Notes != nil {
		t.Errorf("Notes = %+v, want nil (absent = unchanged)", up.Notes)
	}
	if up.Title != nil {
		t.Errorf("Title = %+v, want nil (absent = unchanged)", up.Title)
	}
}

func TestUpdateEventHandler_NoFieldsProvided(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error when no field is provided")
	}
	if svc.UpdateCallCount != 0 {
		t.Errorf("UpdateEvent should not have been called")
	}
}

func TestUpdateEventHandler_StartAfterEndRejected(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
		"start": "2026-07-01T12:00:00Z",
		"end":   "2026-07-01T11:00:00Z",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a start >= end error")
	}
	if svc.UpdateCallCount != 0 {
		t.Errorf("UpdateEvent should not have been called")
	}
}

func TestUpdateEventHandler_InvalidUID(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "../../etc/passwd", "calendar": "/cal/home/", "title": "x",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an invalid UID error")
	}
}

func TestUpdateEventHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{UpdateErr: fmt.Errorf("event not found (uid=uid-1)")}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/", "title": "x",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result")
	}
}
