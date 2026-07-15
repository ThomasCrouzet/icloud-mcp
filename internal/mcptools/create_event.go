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
		mcp.WithDescription("Creates a new event in an iCloud calendar. Supports timed events, all-day events (all_day=true), and optional RRULE recurrence on the master VEVENT only."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithString("title", mcp.Required(), mcp.MinLength(1), mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Event title")),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Start time", defaultLoc)+" For all_day, a date (YYYY-MM-DD) or any datetime (date component used) is accepted.")),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("End time", defaultLoc)+" Must be after start. For all_day, exclusive end date (day after the last day of the event).")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Calendar path (see list_calendars)")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Location (optional)")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Notes/description (optional)")),
		mcp.WithNumber("alarm_minutes_before", mcp.Min(0), mcp.Max(maxAlarmMinutesBefore), mcp.Description("Alarm N minutes before start, 0 = none (optional)")),
		mcp.WithBoolean("all_day", mcp.Description("If true, write an all-day event (VALUE=DATE). Default false.")),
		mcp.WithString("rrule", mcp.MaxLength(1024), mcp.Description("Optional RRULE value without the RRULE: prefix (e.g. FREQ=WEEKLY;COUNT=10). Master VEVENT only; high-frequency rules require COUNT or UNTIL.")),
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
		allDay := req.GetBool("all_day", false)
		rrule := req.GetString("rrule", "")

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
		if rrule != "" {
			if err := icloud.ValidateRRULE(rrule); err != nil {
				return deny("rrule parameter", err)
			}
		}

		var start, end time.Time
		if allDay {
			start, err = parseAllDayDate("start", startStr, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			end, err = parseAllDayDate("end", endStr, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			// Exclusive end: a single-day event may send the same calendar
			// date for start and end; extend by one day.
			if !end.After(start) {
				end = start.Add(24 * time.Hour)
			}
		} else {
			start, err = icloud.ParseDateTime("start", startStr, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			end, err = icloud.ParseDateTime("end", endStr, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
		}
		if err := icloud.ValidateRange(start, end); err != nil {
			return deny("validation", err)
		}

		ne := &icloud.NewEvent{
			Title:              title,
			Location:           location,
			Notes:              notes,
			StartTime:          start,
			EndTime:            end,
			AlarmMinutesBefore: alarm,
			AllDay:             allDay,
			Recurrence:         rrule,
		}

		uid, err := deps.Service.CreateEvent(ctx, calendarPath, ne)
		if err != nil {
			deps.Audit.LogMutation("create_event", calendarPath, "", "error")
			return errResult(deps.Redactor, "creating event", err), nil
		}
		deps.Audit.LogMutation("create_event", calendarPath, uid, "success")

		return writeJSON(deps.Redactor, createEventResponse{Success: true, UID: uid, Calendar: calendarPath}), nil
	}
}

// parseAllDayDate accepts YYYY-MM-DD or a full datetime (date component only)
// and returns midnight UTC on that calendar date for VALUE=DATE writing.
func parseAllDayDate(name, value string, defaultLoc *time.Location) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", value, time.UTC); err == nil {
		return t, nil
	}
	t, err := icloud.ParseDateTime(name, value, defaultLoc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid all-day %s (%q): use YYYY-MM-DD or a datetime", name, value)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
}
