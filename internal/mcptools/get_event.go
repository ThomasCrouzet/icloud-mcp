package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newGetEventTool() mcp.Tool {
	return mcp.NewTool("get_event",
		mcp.WithDescription("Fetches a single iCloud calendar event by calendar path and exact UID. Returns structured fields (title, times, status, transparency, URL, recurrence, alarms, etag). Does not expose internal server paths. Available in read-only mode."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Calendar path (see list_calendars)")),
		mcp.WithString("uid", mcp.Required(), mcp.Description("Event UID (exact match)")),
	)
}

func getEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}
		uid, err := req.RequireString("uid")
		if err != nil {
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if err := icloud.ValidateUID(uid); err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		detail, err := deps.Service.GetEvent(ctx, calendarPath, uid)
		if err != nil {
			return errResult(deps.Redactor, "getting event", err), nil
		}
		// Ensure path never leaks even if a future change serializes it.
		detail.Path = ""
		return writeJSON(deps.Redactor, detail), nil
	}
}
