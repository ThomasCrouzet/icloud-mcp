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
		mcp.WithDescription("Validates event fields locally without network access. Returns normalized data, structured errors, and warnings."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("title", mcp.Required(), mcp.MinLength(1), mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Event title; max 500 UTF-8 bytes")),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Start time", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("End time", defaultLoc))),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Location; max 1000 UTF-8 bytes")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Notes; max 4000 UTF-8 bytes")),
		mcp.WithBoolean("all_day", mcp.DefaultBool(false), mcp.Description("All-day event (VALUE=DATE)")),
		mcp.WithString("timezone", mcp.MaxLength(255), mcp.Description("IANA timezone name (optional)")),
		mcp.WithString("status", mcp.Enum("TENTATIVE", "CONFIRMED", "CANCELLED"), mcp.Description("Optional event status")),
		mcp.WithString("transparency", mcp.Enum("OPAQUE", "TRANSPARENT"), mcp.Description("Optional time transparency")),
		mcp.WithString("url", mcp.MaxLength(icloud.MaxURLLen), mcp.Description("http(s) URL; max 2000 UTF-8 bytes")),
		mcp.WithString("rrule", mcp.MaxLength(1024), mcp.Description("Raw RRULE without prefix; max 1024 UTF-8 bytes")),
		mcp.WithString("recurrence_frequency", mcp.Enum("daily", "weekly", "monthly", "yearly"), mcp.Description("Structured recurrence frequency")),
		mcp.WithInteger("recurrence_interval", mcp.DefaultNumber(1), mcp.Min(1), mcp.Max(366), mcp.Description("Structured recurrence interval")),
		mcp.WithInteger("recurrence_count", mcp.Min(1), mcp.Max(2000), mcp.Description("Structured recurrence COUNT")),
		mcp.WithString("recurrence_until", mcp.Description("Structured recurrence UNTIL")),
		mcp.WithString("recurrence_by_day", mcp.Description("Structured BYDAY comma list")),
		mcp.WithString("recurrence_exceptions", mcp.Description("Comma-separated EXDATE datetimes")),
		mcp.WithInteger("alarm_minutes_before", mcp.Min(0), mcp.Max(maxAlarmMinutesBefore), mcp.Description("Legacy single alarm minutes before start")),
		mcp.WithString("alarms_minutes", mcp.Description("Comma-separated alarm offsets in minutes (max 5 total)")),
		mcp.WithString("client_uid", mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Optional client-supplied UID for idempotent create validation")),
		mcp.WithString("idempotency_key", mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Alias of client_uid (same as create_event)")),
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
		allDay, err := optionalBoolArg(req, "all_day", false)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
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
			if end.Equal(start) {
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
		alarmList, aerr := optionalStringArg(req, "alarms_minutes", "")
		if aerr != nil {
			return errResult(deps.Redactor, "validation", aerr), nil
		}
		alarms, aerr := parseAlarmsMinutesList(alarmList)
		if aerr != nil {
			return errResult(deps.Redactor, "validation", aerr), nil
		}
		structured, serr := parseStructuredRecurrence(req, deps.DefaultLocation)
		if serr != nil {
			return errResult(deps.Redactor, "validation", serr), nil
		}
		alarmMinutes, aerr := optionalIntArg(req, "alarm_minutes_before", 0)
		if aerr != nil {
			return errResult(deps.Redactor, "validation", aerr), nil
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
			AlarmMinutes: alarmMinutes,
			Alarms:       alarms,
			Structured:   structured,
			ClientUID:    req.GetString("client_uid", ""),
		}
		if in.ClientUID == "" {
			in.ClientUID = req.GetString("idempotency_key", "")
		}
		res := icloud.ValidateEventInputContext(ctx, in, deps.DefaultLocation)
		return writeJSON(deps.Redactor, res), nil
	}
}
