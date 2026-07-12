package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// maxAlarmMinutesBefore caps the alarm at 4 weeks before the event.
const maxAlarmMinutesBefore = 40320

func newCreateEventTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("create_event",
		mcp.WithDescription("Creates a new event in an iCloud calendar."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithString("title", mcp.Required(), mcp.MinLength(1), mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Event title")),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Start time", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("End time", defaultLoc)+" Must be after start.")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Calendar path (see list_calendars)")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Location (optional)")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Notes/description (optional)")),
		mcp.WithNumber("alarm_minutes_before", mcp.Min(0), mcp.Max(maxAlarmMinutesBefore), mcp.Description("Alarm N minutes before start, 0 = none (optional)")),
	)
}

type createEventResponse struct {
	Success  bool   `json:"success"`
	UID      string `json:"uid"`
	Calendar string `json:"calendar"`
}

func createEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		title, err := req.RequireString("title")
		if err != nil {
			return errResult(deps.Redactor, "title parameter", err), nil
		}
		startStr, err := req.RequireString("start")
		if err != nil {
			return errResult(deps.Redactor, "start parameter", err), nil
		}
		endStr, err := req.RequireString("end")
		if err != nil {
			return errResult(deps.Redactor, "end parameter", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}
		location := req.GetString("location", "")
		notes := req.GetString("notes", "")
		alarm := req.GetInt("alarm_minutes_before", 0)

		deny := func(context string, err error) (*mcp.CallToolResult, error) {
			deps.Audit.LogMutation("create_event", calendarPath, "", "denied")
			return errResult(deps.Redactor, context, err), nil
		}

		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return deny("calendar parameter", err)
		}
		if err := icloud.ValidateTextField("title", title, icloud.MaxTitleLen); err != nil {
			return deny("title parameter", err)
		}
		if title == "" {
			return deny("title parameter", fmt.Errorf("title cannot be empty"))
		}
		if err := icloud.ValidateTextField("location", location, icloud.MaxLocationLen); err != nil {
			return deny("location parameter", err)
		}
		if err := icloud.ValidateTextField("notes", notes, icloud.MaxNotesLen); err != nil {
			return deny("notes parameter", err)
		}
		if alarm < 0 || alarm > maxAlarmMinutesBefore {
			return deny("alarm_minutes_before parameter", fmt.Errorf("must be between 0 and %d (4 weeks)", maxAlarmMinutesBefore))
		}
		start, err := icloud.ParseDateTime("start", startStr, deps.DefaultLocation)
		if err != nil {
			return deny("validation", err)
		}
		end, err := icloud.ParseDateTime("end", endStr, deps.DefaultLocation)
		if err != nil {
			return deny("validation", err)
		}
		if err := icloud.ValidateRange(start, end); err != nil {
			return deny("validation", err)
		}

		uid, err := deps.Service.CreateEvent(ctx, calendarPath, &icloud.NewEvent{
			Title:              title,
			Location:           location,
			Notes:              notes,
			StartTime:          start,
			EndTime:            end,
			AlarmMinutesBefore: alarm,
		})
		if err != nil {
			deps.Audit.LogMutation("create_event", calendarPath, "", "error")
			return errResult(deps.Redactor, "creating event", err), nil
		}
		deps.Audit.LogMutation("create_event", calendarPath, uid, "success")

		return writeJSON(deps.Redactor, createEventResponse{Success: true, UID: uid, Calendar: calendarPath}), nil
	}
}
