package mcptools

import (
	"context"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// TestSchedulingApprovalRequiresExplicitOperatorGrant proves an agent cannot
// enable the group-scheduled path by inventing acknowledge_scheduling alone.
func TestSchedulingApprovalRequiresExplicitOperatorGrant(t *testing.T) {
	svc := &icloud.MockService{}
	deps := testDeps(svc)
	handler := updateEventHandler(deps)

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid":                    "uid-1",
		"calendar":               "/cal/home/",
		"title":                  "Changed",
		"etag":                   `"v1"`,
		"acknowledge_scheduling": true,
	}))
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch error: %s", resultText(t, res))
	}
	if svc.LastUpdate == nil || svc.LastUpdate.AllowGroupScheduled {
		t.Fatalf("legacy policy granted scheduling approval: %+v", svc.LastUpdate)
	}

	svc = &icloud.MockService{}
	deps = testDeps(svc)
	deps.CalendarWriteExplicit = true
	handler = updateEventHandler(deps)
	res, err = handler(context.Background(), newReq(map[string]any{
		"uid":                    "uid-1",
		"calendar":               "/cal/home/",
		"title":                  "Changed",
		"etag":                   `"v1"`,
		"acknowledge_scheduling": true,
	}))
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch error: %s", resultText(t, res))
	}
	if svc.LastUpdate == nil || svc.LastUpdate.AllowGroupScheduled {
		t.Fatalf("write grant alone authorized scheduling: %+v", svc.LastUpdate)
	}

	svc = &icloud.MockService{}
	deps = testDeps(svc)
	deps.CalendarWriteExplicit = true
	deps.CalendarScheduleExplicit = true
	handler = updateEventHandler(deps)
	res, err = handler(context.Background(), newReq(map[string]any{
		"uid":                    "uid-1",
		"calendar":               "/cal/home/",
		"title":                  "Changed",
		"etag":                   `"v1"`,
		"acknowledge_scheduling": true,
	}))
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch error: %s", resultText(t, res))
	}
	if svc.LastUpdate == nil || !svc.LastUpdate.AllowGroupScheduled {
		t.Fatalf("explicit operator grant did not reach the service: %+v", svc.LastUpdate)
	}
}

// TestDeleteSchedulingApprovalAndDryRun covers the delete gate and the
// non-sensitive dry-run fields.
func TestDeleteSchedulingApprovalAndDryRun(t *testing.T) {
	svc := &icloud.MockService{}
	deps := testDeps(svc)
	deps.CalendarDeleteExplicit = true
	deps.CalendarScheduleExplicit = true
	handler := deleteEventHandler(deps)

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
		"etag":     `"v1"`,
		"dry_run":  true,
	}))
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("dry-run error: %s", resultText(t, res))
	}
	if svc.LastDeleteOpts == nil || !svc.LastDeleteOpts.DryRun || svc.LastDeleteOpts.AllowGroupScheduled {
		t.Fatalf("dry-run opts = %+v", svc.LastDeleteOpts)
	}

	svc = &icloud.MockService{}
	deps = testDeps(svc)
	deps.CalendarDeleteExplicit = true
	deps.CalendarScheduleExplicit = true
	handler = deleteEventHandler(deps)
	res, err = handler(context.Background(), newReq(map[string]any{
		"uid":                    "uid-1",
		"calendar":               "/cal/home/",
		"etag":                   `"v1"`,
		"acknowledge_scheduling": true,
	}))
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch error: %s", resultText(t, res))
	}
	if svc.LastDeleteOpts == nil || !svc.LastDeleteOpts.AllowGroupScheduled {
		t.Fatalf("approved delete did not reach the service: %+v", svc.LastDeleteOpts)
	}
}
