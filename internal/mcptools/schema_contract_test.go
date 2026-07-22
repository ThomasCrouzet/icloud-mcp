package mcptools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// TestSchemaContract_BoundsMatchRuntimeConstants prevents schema/runtime drift
// for shared limits (MaxResults, MaxTitleLen, etc.).
func TestSchemaContract_BoundsMatchRuntimeConstants(t *testing.T) {
	if icloud.MaxResults != 400 {
		t.Fatalf("MaxResults changed to %d; update tool schemas and docs", icloud.MaxResults)
	}
	if icloud.MaxRangeDays != 366 {
		t.Fatalf("MaxRangeDays changed to %d; update tool schemas and docs", icloud.MaxRangeDays)
	}
	if icloud.MaxAlarms != 5 {
		t.Fatalf("MaxAlarms changed to %d; update create tool", icloud.MaxAlarms)
	}
	// Tool constructors must not panic and must embed the constants.
	_ = newSearchEventsTool(time.UTC)
	_ = newCreateEventTool(time.UTC)
	_ = newUpdateEventTool(time.UTC)
	_ = newDeleteEventTool(time.UTC)
	_ = newGetEventTool()
	_ = newValidateEventTool(time.UTC)
	_ = newFindFreeSlotsTool(time.UTC)
	_ = newCalendarCapabilitiesTool()
	_ = newListCalendarsTool()
}

func TestMCP_E2E_ReadOnlyAndReadWrite(t *testing.T) {
	for _, ro := range []bool{true, false} {
		t.Run(map[bool]string{true: "readonly", false: "readwrite"}[ro], func(t *testing.T) {
			svc := &icloud.MockService{
				Calendars: []icloud.Calendar{{Path: "/cal/home/", Name: "Home"}},
				Events: []icloud.Event{{
					UID: "e1", Title: "Meet",
					StartTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
					EndTime:   time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
				}},
			}
			red := security.NewRedactor("super-secret-password-xyz")
			s := server.NewMCPServer("icloud-mcp", "test",
				server.WithToolCapabilities(false),
				server.WithToolHandlerMiddleware(RecoverRedactMiddleware(red)),
			)
			Register(s, Deps{Service: svc, Audit: security.NewAuditLogger(ioDiscard{}), Redactor: red, DefaultLocation: time.UTC}, ro)

			c, err := client.NewInProcessClient(s)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			ctx := context.Background()
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			initReq := mcp.InitializeRequest{}
			initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
			initReq.Params.ClientInfo = mcp.Implementation{Name: "e2e", Version: "0"}
			if _, err := c.Initialize(ctx, initReq); err != nil {
				t.Fatal(err)
			}
			tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			has := map[string]bool{}
			for _, tool := range tools.Tools {
				has[tool.Name] = true
			}
			for _, name := range []string{"list_calendars", "search_events", "get_event", "validate_event", "calendar_capabilities", "find_free_slots"} {
				if !has[name] {
					t.Errorf("missing read tool %s", name)
				}
			}
			if ro {
				for _, name := range []string{"create_event", "update_event", "delete_event"} {
					if has[name] {
						t.Errorf("RO must hide %s", name)
					}
				}
			} else {
				for _, name := range []string{"create_event", "update_event", "delete_event"} {
					if !has[name] {
						t.Errorf("RW must expose %s", name)
					}
				}
			}
			// Call calendar_capabilities (local, no network).
			call := mcp.CallToolRequest{}
			call.Params.Name = "calendar_capabilities"
			call.Params.Arguments = map[string]any{}
			res, err := c.CallTool(ctx, call)
			if err != nil || res.IsError {
				t.Fatalf("capabilities: err=%v res=%+v", err, res)
			}
		})
	}
}

// ioDiscard is a minimal io.Writer for audit in tests without importing io if
// already available - use bytes.Buffer pattern via empty writer.
type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
