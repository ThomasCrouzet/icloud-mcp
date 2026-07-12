package mcptools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// maxAlarmMinutesBefore borne l'alarme à 4 semaines avant l'événement.
const maxAlarmMinutesBefore = 40320

func newCreateEventTool() mcp.Tool {
	return mcp.NewTool("create_event",
		mcp.WithDescription("Crée un nouvel événement dans un calendrier iCloud."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithString("title", mcp.Required(), mcp.MinLength(1), mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Titre de l'événement")),
		mcp.WithString("start", mcp.Required(), mcp.Description("Début, RFC3339 (ex. 2026-07-01T10:00:00Z)")),
		mcp.WithString("end", mcp.Required(), mcp.Description("Fin, RFC3339, doit être après start")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Path du calendrier (voir list_calendars)")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Lieu (optionnel)")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Notes/description (optionnel)")),
		mcp.WithNumber("alarm_minutes_before", mcp.Min(0), mcp.Max(maxAlarmMinutesBefore), mcp.Description("Alarme N minutes avant le début, 0 = aucune (optionnel)")),
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
			return errResult(deps.Redactor, "paramètre title", err), nil
		}
		startStr, err := req.RequireString("start")
		if err != nil {
			return errResult(deps.Redactor, "paramètre start", err), nil
		}
		endStr, err := req.RequireString("end")
		if err != nil {
			return errResult(deps.Redactor, "paramètre end", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "paramètre calendar", err), nil
		}
		location := req.GetString("location", "")
		notes := req.GetString("notes", "")
		alarm := req.GetInt("alarm_minutes_before", 0)

		deny := func(context string, err error) (*mcp.CallToolResult, error) {
			deps.Audit.LogMutation("create_event", calendarPath, "", "denied")
			return errResult(deps.Redactor, context, err), nil
		}

		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return deny("paramètre calendar", err)
		}
		if err := icloud.ValidateTextField("title", title, icloud.MaxTitleLen); err != nil {
			return deny("paramètre title", err)
		}
		if title == "" {
			return deny("paramètre title", fmt.Errorf("le titre ne peut pas être vide"))
		}
		if err := icloud.ValidateTextField("location", location, icloud.MaxLocationLen); err != nil {
			return deny("paramètre location", err)
		}
		if err := icloud.ValidateTextField("notes", notes, icloud.MaxNotesLen); err != nil {
			return deny("paramètre notes", err)
		}
		if alarm < 0 || alarm > maxAlarmMinutesBefore {
			return deny("paramètre alarm_minutes_before", fmt.Errorf("doit être compris entre 0 et %d (4 semaines)", maxAlarmMinutesBefore))
		}
		start, err := icloud.ParseRFC3339("start", startStr)
		if err != nil {
			return deny("validation", err)
		}
		end, err := icloud.ParseRFC3339("end", endStr)
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
			return errResult(deps.Redactor, "création de l'événement", err), nil
		}
		deps.Audit.LogMutation("create_event", calendarPath, uid, "success")

		return writeJSON(deps.Redactor, createEventResponse{Success: true, UID: uid, Calendar: calendarPath}), nil
	}
}
