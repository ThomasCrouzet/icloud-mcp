package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newUpdateEventTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("update_event",
		mcp.WithDescription("Updates the provided fields of an existing event, located by UID. Omitted fields remain unchanged; a text field provided as empty clears the existing value."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithString("uid", mcp.Required(), mcp.Description("Event UID (see search_events)")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Path of the calendar containing the event")),
		mcp.WithString("title", mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("New title. Omitted = unchanged; empty = cleared.")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("New location. Omitted = unchanged; empty = cleared.")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("New notes. Omitted = unchanged; empty = cleared.")),
		mcp.WithString("start", mcp.Description(datetimeParamDescription("New start time. Omitted = unchanged", defaultLoc))),
		mcp.WithString("end", mcp.Description(datetimeParamDescription("New end time. Omitted = unchanged", defaultLoc))),
	)
}

type updateEventResponse struct {
	Success bool   `json:"success"`
	UID     string `json:"uid"`
}

func updateEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		uid, err := req.RequireString("uid")
		if err != nil {
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		deny := func(context string, err error) (*mcp.CallToolResult, error) {
			deps.Audit.LogMutation("update_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, context, err), nil
		}

		if err := icloud.ValidateUID(uid); err != nil {
			return deny("uid parameter", err)
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return deny("calendar parameter", err)
		}

		args := req.GetArguments()
		update := &icloud.EventUpdate{}

		if v, exists := args["title"]; exists {
			s, _ := v.(string)
			if err := icloud.ValidateTextField("title", s, icloud.MaxTitleLen); err != nil {
				return deny("title parameter", err)
			}
			update.Title = &s
		}
		if v, exists := args["location"]; exists {
			s, _ := v.(string)
			if err := icloud.ValidateTextField("location", s, icloud.MaxLocationLen); err != nil {
				return deny("location parameter", err)
			}
			update.Location = &s
		}
		if v, exists := args["notes"]; exists {
			s, _ := v.(string)
			if err := icloud.ValidateTextField("notes", s, icloud.MaxNotesLen); err != nil {
				return deny("notes parameter", err)
			}
			update.Notes = &s
		}

		var newStart, newEnd *time.Time
		if v, exists := args["start"]; exists {
			s, _ := v.(string)
			t, err := icloud.ParseDateTime("start", s, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			update.StartTime = &t
			newStart = &t
		}
		if v, exists := args["end"]; exists {
			s, _ := v.(string)
			t, err := icloud.ParseDateTime("end", s, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			update.EndTime = &t
			newEnd = &t
		}
		if newStart != nil && newEnd != nil {
			if err := icloud.ValidateRange(*newStart, *newEnd); err != nil {
				return deny("validation", err)
			}
		}

		if update.Title == nil && update.Location == nil && update.Notes == nil && update.StartTime == nil && update.EndTime == nil {
			return deny("validation", fmt.Errorf("no field to update was provided (title/location/notes/start/end)"))
		}

		if err := deps.Service.UpdateEvent(ctx, calendarPath, uid, update); err != nil {
			deps.Audit.LogMutation("update_event", calendarPath, uid, "error")
			return errResult(deps.Redactor, "updating event", err), nil
		}
		deps.Audit.LogMutation("update_event", calendarPath, uid, "success")

		return writeJSON(deps.Redactor, updateEventResponse{Success: true, UID: uid}), nil
	}
}
