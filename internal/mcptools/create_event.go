package mcptools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// maxAlarmMinutesBefore caps the alarm at 4 weeks before the event.
const maxAlarmMinutesBefore = 40320

func newCreateEventTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("create_event",
		mcp.WithDescription("Creates a new event in an iCloud calendar. Supports timed/all-day events, optional raw rrule OR structured recurrence (frequency/interval/count/until/by_day), status/transparency/URL, multiple alarms (alarm_minutes_before and/or alarms_minutes comma list, max 5), and client_uid for idempotent create (conflict if UID exists; never silent overwrite). No attendees/invitations."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithString("title", mcp.Required(), mcp.MinLength(1), mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Event title")),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Start time", defaultLoc)+" For all_day, a date (YYYY-MM-DD) or any datetime (date component used) is accepted.")),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("End time", defaultLoc)+" Must be after start. For all_day, exclusive end date (day after the last day of the event).")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Calendar path (see list_calendars)")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Location (optional)")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Notes/description (optional)")),
		mcp.WithNumber("alarm_minutes_before", mcp.Min(0), mcp.Max(maxAlarmMinutesBefore), mcp.Description("Legacy single alarm N minutes before start, 0 = none (optional)")),
		mcp.WithString("alarms_minutes", mcp.Description("Comma-separated alarm offsets in minutes before start (e.g. 15,60,1440). Max 5 alarms total with alarm_minutes_before.")),
		mcp.WithBoolean("all_day", mcp.Description("If true, write an all-day event (VALUE=DATE). Default false.")),
		mcp.WithString("rrule", mcp.MaxLength(1024), mcp.Description("Optional raw RRULE value without the RRULE: prefix. Mutually exclusive with structured recurrence fields when both would disagree.")),
		mcp.WithString("recurrence_frequency", mcp.Description("Structured recurrence: daily, weekly, monthly, or yearly (optional; prefer over raw rrule)")),
		mcp.WithNumber("recurrence_interval", mcp.Min(1), mcp.Max(366), mcp.Description("Structured recurrence interval (default 1)")),
		mcp.WithNumber("recurrence_count", mcp.Min(1), mcp.Max(2000), mcp.Description("Structured recurrence COUNT (mutually exclusive with recurrence_until)")),
		mcp.WithString("recurrence_until", mcp.Description("Structured recurrence UNTIL (RFC3339 or YYYY-MM-DD)")),
		mcp.WithString("recurrence_by_day", mcp.Description("Structured recurrence BYDAY, comma-separated MO,TU,WE,TH,FR,SA,SU")),
		mcp.WithString("recurrence_exceptions", mcp.Description("Comma-separated exception datetimes (EXDATE) interpreted with ICLOUD_MCP_DEFAULT_TZ rules")),
		mcp.WithString("status", mcp.Description("TENTATIVE, CONFIRMED, or CANCELLED (optional)")),
		mcp.WithString("transparency", mcp.Description("OPAQUE or TRANSPARENT (optional)")),
		mcp.WithString("url", mcp.Description("http(s) URL (optional)")),
		mcp.WithString("timezone", mcp.Description("IANA timezone name (informational; timed events stored as UTC instants)")),
		mcp.WithString("client_uid", mcp.Description("Optional client UID for idempotent create; conflict if already exists")),
		mcp.WithString("idempotency_key", mcp.Description("Alias of client_uid when client_uid omitted")),
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
		status := req.GetString("status", "")
		transp := req.GetString("transparency", "")
		eventURL := req.GetString("url", "")
		timezone := req.GetString("timezone", "")
		clientUID := req.GetString("client_uid", "")
		if clientUID == "" {
			clientUID = req.GetString("idempotency_key", "")
		}

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
		alarms, aerr := parseAlarmsMinutesList(req.GetString("alarms_minutes", ""))
		if aerr != nil {
			return deny("alarms_minutes parameter", aerr)
		}
		structured, serr := parseStructuredRecurrence(req, deps.DefaultLocation)
		if serr != nil {
			return deny("validation", serr)
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

		input := &icloud.EventInput{
			Title: title, Location: location, Notes: notes,
			StartTime: start, EndTime: end, AllDay: allDay,
			Timezone: timezone, Status: status, Transparency: transp,
			URL: eventURL, Recurrence: rrule, AlarmMinutes: alarm,
			Alarms: alarms, Structured: structured, ClientUID: clientUID,
		}
		// Local validate for status/url/uid/recurrence/alarms before network.
		vr := icloud.ValidateEventInput(input, deps.DefaultLocation)
		if !vr.OK {
			return deny("validation", fmt.Errorf("%s", joinErrors(vr.Errors)))
		}
		// Prefer normalized RRULE from structured recurrence when present.
		finalRule := rrule
		var exDates []time.Time
		if structured != nil {
			finalRule = vr.Normalized.Recurrence
			// Re-run structured conversion for EXDATE times (Validate only warns).
			if built, ex, berr := icloud.StructuredRecurrenceToRRULE(structured, deps.DefaultLocation); berr == nil {
				finalRule = built
				exDates = ex
			}
		}

		ne := &icloud.NewEvent{
			Title:              title,
			Location:           location,
			Notes:              notes,
			StartTime:          start,
			EndTime:            end,
			AlarmMinutesBefore: alarm,
			Alarms:             alarms,
			AllDay:             allDay,
			Recurrence:         finalRule,
			ExDates:            exDates,
			Status:             status,
			Transparency:       transp,
			URL:                eventURL,
			Timezone:           timezone,
			ClientUID:          clientUID,
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

func joinErrors(errs []string) string {
	if len(errs) == 0 {
		return "validation failed"
	}
	out := errs[0]
	for i := 1; i < len(errs); i++ {
		out += "; " + errs[i]
	}
	return out
}

// parseAlarmsMinutesList parses "15,60,1440" into AlarmSpec values.
func parseAlarmsMinutesList(s string) ([]icloud.AlarmSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []icloud.AlarmSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid alarms_minutes entry %q", part)
		}
		if n < 0 || n > maxAlarmMinutesBefore {
			return nil, fmt.Errorf("alarms_minutes entry %d out of range 0..%d", n, maxAlarmMinutesBefore)
		}
		if n == 0 {
			continue
		}
		out = append(out, icloud.AlarmSpec{MinutesBefore: n})
		if len(out) > icloud.MaxAlarms {
			return nil, fmt.Errorf("at most %d alarms allowed", icloud.MaxAlarms)
		}
	}
	return out, nil
}

// parseStructuredRecurrence reads optional structured recurrence MCP fields.
// Returns nil when frequency is empty.
func parseStructuredRecurrence(req mcp.CallToolRequest, defaultLoc *time.Location) (*icloud.StructuredRecurrence, error) {
	freq := strings.TrimSpace(req.GetString("recurrence_frequency", ""))
	if freq == "" {
		// Reject orphan structured fields without frequency.
		if req.GetString("recurrence_until", "") != "" ||
			req.GetString("recurrence_by_day", "") != "" ||
			req.GetString("recurrence_exceptions", "") != "" ||
			req.GetInt("recurrence_count", 0) > 0 ||
			req.GetInt("recurrence_interval", 0) > 1 {
			return nil, fmt.Errorf("recurrence_frequency is required when using structured recurrence fields")
		}
		return nil, nil
	}
	sr := &icloud.StructuredRecurrence{
		Frequency: freq,
		Interval:  req.GetInt("recurrence_interval", 1),
		Count:     req.GetInt("recurrence_count", 0),
		Until:     req.GetString("recurrence_until", ""),
	}
	if by := req.GetString("recurrence_by_day", ""); by != "" {
		for _, d := range strings.Split(by, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				sr.ByDay = append(sr.ByDay, d)
			}
		}
	}
	if ex := req.GetString("recurrence_exceptions", ""); ex != "" {
		for _, part := range strings.Split(ex, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				sr.Exceptions = append(sr.Exceptions, part)
			}
		}
	}
	_ = defaultLoc
	return sr, nil
}
