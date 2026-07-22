package mcptools

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newValidateEventTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("validate_event",
		mcp.WithDescription("Validates event create/update fields locally with NO network access. Returns normalized representation, structured errors, and warnings (e.g. DST ambiguity). Available in read-only mode."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("title", mcp.Required(), mcp.MinLength(1), mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Event title")),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Start time", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("End time", defaultLoc))),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Location (optional)")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Notes (optional)")),
		mcp.WithBoolean("all_day", mcp.Description("All-day event (VALUE=DATE)")),
		mcp.WithString("timezone", mcp.Description("IANA timezone name (optional)")),
		mcp.WithString("status", mcp.Description("TENTATIVE, CONFIRMED, or CANCELLED")),
		mcp.WithString("transparency", mcp.Description("OPAQUE or TRANSPARENT")),
		mcp.WithString("url", mcp.Description("http(s) URL (optional)")),
		mcp.WithString("rrule", mcp.MaxLength(1024), mcp.Description("Raw RRULE without prefix (optional)")),
		mcp.WithNumber("alarm_minutes_before", mcp.Min(0), mcp.Max(maxAlarmMinutesBefore), mcp.Description("Legacy single alarm minutes before start")),
		mcp.WithString("client_uid", mcp.Description("Optional client-supplied UID for idempotent create validation")),
	)
}

func validateEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// ctx is intentionally unused: this tool must never touch the network.
		_ = ctx
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
		allDay := req.GetBool("all_day", false)
		var start, end time.Time
		if allDay {
			start, err = parseAllDayDate("start", startStr, deps.DefaultLocation)
			if err != nil {
				return errResult(deps.Redactor, "validation", err), nil
			}
			end, err = parseAllDayDate("end", endStr, deps.DefaultLocation)
			if err != nil {
				return errResult(deps.Redactor, "validation", err), nil
			}
			if !end.After(start) {
				end = start.Add(24 * time.Hour)
			}
		} else {
			start, err = icloud.ParseDateTime("start", startStr, deps.DefaultLocation)
			if err != nil {
				return errResult(deps.Redactor, "validation", err), nil
			}
			end, err = icloud.ParseDateTime("end", endStr, deps.DefaultLocation)
			if err != nil {
				return errResult(deps.Redactor, "validation", err), nil
			}
		}
		in := &icloud.EventInput{
			Title:        title,
			Location:     req.GetString("location", ""),
			Notes:        req.GetString("notes", ""),
			StartTime:    start,
			EndTime:      end,
			AllDay:       allDay,
			Timezone:     req.GetString("timezone", ""),
			Status:       req.GetString("status", ""),
			Transparency: req.GetString("transparency", ""),
			URL:          req.GetString("url", ""),
			Recurrence:   req.GetString("rrule", ""),
			AlarmMinutes: req.GetInt("alarm_minutes_before", 0),
			ClientUID:    req.GetString("client_uid", ""),
		}
		res := icloud.ValidateEventInput(in, deps.DefaultLocation)
		if !res.OK {
			return writeJSON(deps.Redactor, res), nil
		}
		return writeJSON(deps.Redactor, res), nil
	}
}
