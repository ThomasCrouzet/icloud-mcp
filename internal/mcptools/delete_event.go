package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newDeleteEventTool() mcp.Tool {
	return mcp.NewTool("delete_event",
		mcp.WithDescription("Permanently deletes an event, located by UID. This action is irreversible; the calling agent should obtain human confirmation before performing any real deletion. The deleted event's title is returned as confirmation."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("uid", mcp.Required(), mcp.Description("Event UID (see search_events)")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Path of the calendar containing the event")),
	)
}

type deleteEventResponse struct {
	Success      bool   `json:"success"`
	UID          string `json:"uid"`
	DeletedTitle string `json:"deletedTitle"`
}

func deleteEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		uid, err := req.RequireString("uid")
		if err != nil {
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		if err := icloud.ValidateUID(uid); err != nil {
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		title, err := deps.Service.DeleteEvent(ctx, calendarPath, uid)
		if err != nil {
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "error")
			return errResult(deps.Redactor, "deleting event", err), nil
		}
		deps.Audit.LogMutation("delete_event", calendarPath, uid, "success")

		return writeJSON(deps.Redactor, deleteEventResponse{Success: true, UID: uid, DeletedTitle: title}), nil
	}
}
