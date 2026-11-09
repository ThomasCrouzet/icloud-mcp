package mcptools

import (
	"context"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestUpdateIdempotencyIsScopedPerRegistration(t *testing.T) {
	firstService := &icloud.MockService{CreatedUID: "first"}
	firstDeps := testDeps(firstService)
	firstDeps.Idempotency = newIdempotencyStore()
	first := updateEventHandler(firstDeps)

	args := map[string]any{
		"uid": "same-uid", "calendar": "/cal/home/", "title": "Same",
		"idempotency_key": "same-key",
	}
	result, err := first(context.Background(), newReq(args))
	if err != nil || result.IsError {
		t.Fatalf("first registration: err=%v result=%s", err, resultText(t, result))
	}
	if firstService.UpdateCallCount != 1 {
		t.Fatalf("first registration writes = %d", firstService.UpdateCallCount)
	}

	secondService := &icloud.MockService{UpdateErr: &icloud.Error{Code: icloud.CodeForbidden, Message: "second account denied"}}
	secondDeps := testDeps(secondService)
	secondDeps.Idempotency = newIdempotencyStore()
	second := updateEventHandler(secondDeps)
	secondResult, err := second(context.Background(), newReq(args))
	if err != nil {
		t.Fatalf("second registration protocol error = %v", err)
	}
	if !secondResult.IsError {
		t.Fatal("second registration replayed the first registration's cached success")
	}
	if secondService.UpdateCallCount != 1 {
		t.Fatalf("second registration writes = %d", secondService.UpdateCallCount)
	}
}
