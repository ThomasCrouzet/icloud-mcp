package mcptools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newFindFreeSlotsTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("find_free_slots",
		mcp.WithDescription("Computes free time slots locally from calendar busy intervals. Expands recurrences, ignores TRANSPARENT and CANCELLED events, merges overlaps, applies working hours/buffers/DST. Never reveals the events that cause busy time. Available in read-only mode."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Search range start", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("Search range end", defaultLoc)+" At most 366 days after start.")),
		mcp.WithNumber("duration_minutes", mcp.Required(), mcp.Min(1), mcp.Max(24*60), mcp.Description("Required free slot length in minutes")),
		mcp.WithString("calendar", mcp.Description("Single calendar path. Prefer calendars for multi-calendar.")),
		mcp.WithString("calendars", mcp.Description("Comma-separated calendar paths. All calendars if both calendar and calendars omitted.")),
		mcp.WithString("timezone", mcp.Description("IANA timezone for working hours (default: ICLOUD_MCP_DEFAULT_TZ)")),
		mcp.WithString("working_hours_start", mcp.Description("Local start of working day HH:MM (optional)")),
		mcp.WithString("working_hours_end", mcp.Description("Local end of working day HH:MM (optional; may be before start for overnight)")),
		mcp.WithString("days_of_week", mcp.Description("Comma-separated weekdays: 0=Sunday .. 6=Saturday, or mon,tue,... Empty = all days.")),
		mcp.WithNumber("buffer_before_minutes", mcp.Min(0), mcp.Max(24*60), mcp.Description("Busy padding before each event (minutes)")),
		mcp.WithNumber("buffer_after_minutes", mcp.Min(0), mcp.Max(24*60), mcp.Description("Busy padding after each event (minutes)")),
		mcp.WithBoolean("include_all_day_busy", mcp.Description("Treat all-day events as busy (default true)")),
		mcp.WithNumber("limit", mcp.DefaultNumber(50), mcp.Min(1), mcp.Max(200), mcp.Description("Max free slots returned")),
	)
}

type freeSlotsResponse struct {
	Count int              `json:"count"`
	Slots []freeSlotOutput `json:"slots"`
}

type freeSlotOutput struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func findFreeSlotsHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		startStr, err := req.RequireString("start")
		if err != nil {
			return errResult(deps.Redactor, "start parameter", err), nil
		}
		endStr, err := req.RequireString("end")
		if err != nil {
			return errResult(deps.Redactor, "end parameter", err), nil
		}
		start, err := icloud.ParseDateTime("start", startStr, deps.DefaultLocation)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		end, err := icloud.ParseDateTime("end", endStr, deps.DefaultLocation)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if err := icloud.ValidateRange(start, end); err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		durMin := req.GetInt("duration_minutes", 0)
		if durMin <= 0 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("duration_minutes must be positive")), nil
		}
		paths, err := resolveCalendarPaths(ctx, deps, req)
		if err != nil {
			return errResult(deps.Redactor, "calendars", err), nil
		}
		loc := deps.DefaultLocation
		if tz := req.GetString("timezone", ""); tz != "" {
			l, lerr := time.LoadLocation(tz)
			if lerr != nil {
				return errResult(deps.Redactor, "validation", fmt.Errorf("invalid timezone %q", tz)), nil
			}
			loc = l
		}
		opts := icloud.FreeSlotOptions{
			RangeStart:        start,
			RangeEnd:          end,
			Duration:          time.Duration(durMin) * time.Minute,
			Location:          loc,
			BufferBefore:      time.Duration(req.GetInt("buffer_before_minutes", 0)) * time.Minute,
			BufferAfter:       time.Duration(req.GetInt("buffer_after_minutes", 0)) * time.Minute,
			IncludeAllDayBusy: true,
			Limit:             req.GetInt("limit", 50),
		}
		// mcp-go GetBool default: if key absent use true for include_all_day_busy.
		if v, ok := req.GetArguments()["include_all_day_busy"]; ok {
			if b, ok := v.(bool); ok {
				opts.IncludeAllDayBusy = b
			}
		}
		if whs := req.GetString("working_hours_start", ""); whs != "" {
			m, e := icloud.ParseWorkingHours(whs)
			if e != nil {
				return errResult(deps.Redactor, "validation", e), nil
			}
			opts.WorkingHourStart = m
		}
		if whe := req.GetString("working_hours_end", ""); whe != "" {
			m, e := icloud.ParseWorkingHours(whe)
			if e != nil {
				return errResult(deps.Redactor, "validation", e), nil
			}
			opts.WorkingHourEnd = m
		}
		if dow := req.GetString("days_of_week", ""); dow != "" {
			days, e := parseDaysOfWeek(dow)
			if e != nil {
				return errResult(deps.Redactor, "validation", e), nil
			}
			opts.DaysOfWeek = days
		}

		var allEvents []icloud.Event
		for _, path := range paths {
			res, serr := deps.Service.SearchEvents(ctx, path, start, end)
			if serr != nil {
				// Auth/security must not become a soft warning.
				if ie := icloud.AsICloudError(serr); ie != nil {
					switch ie.Code {
					case icloud.CodeAuthenticationRefused, icloud.CodeForbidden, icloud.CodeAuthentication, icloud.CodeAuthorization:
						return errResult(deps.Redactor, "searching events", serr), nil
					}
				}
				return errResult(deps.Redactor, "searching events", serr), nil
			}
			allEvents = append(allEvents, res.Events...)
		}
		busy := icloud.BusyFromEvents(allEvents, opts.IncludeAllDayBusy, opts.BufferBefore, opts.BufferAfter)
		slots, err := icloud.FindFreeSlots(busy, opts)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		out := freeSlotsResponse{Count: len(slots), Slots: make([]freeSlotOutput, 0, len(slots))}
		for _, s := range slots {
			out.Slots = append(out.Slots, freeSlotOutput{
				Start: s.Start.Format(time.RFC3339),
				End:   s.End.Format(time.RFC3339),
			})
		}
		return writeJSON(deps.Redactor, out), nil
	}
}

func resolveCalendarPaths(ctx context.Context, deps Deps, req mcp.CallToolRequest) ([]string, error) {
	var paths []string
	if multi := req.GetString("calendars", ""); multi != "" {
		for _, p := range strings.Split(multi, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if err := icloud.ValidateCalendarPath(p); err != nil {
				return nil, err
			}
			paths = append(paths, p)
		}
	}
	if single := req.GetString("calendar", ""); single != "" {
		if err := icloud.ValidateCalendarPath(single); err != nil {
			return nil, err
		}
		// Avoid duplicate if also in calendars.
		found := false
		for _, p := range paths {
			if p == single {
				found = true
				break
			}
		}
		if !found {
			paths = append(paths, single)
		}
	}
	if len(paths) > 0 {
		return paths, nil
	}
	cals, err := deps.Service.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cals {
		paths = append(paths, c.Path)
	}
	return paths, nil
}

func parseDaysOfWeek(s string) ([]time.Weekday, error) {
	var out []time.Weekday
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		switch part {
		case "0", "sun", "sunday":
			out = append(out, time.Sunday)
		case "1", "mon", "monday":
			out = append(out, time.Monday)
		case "2", "tue", "tuesday":
			out = append(out, time.Tuesday)
		case "3", "wed", "wednesday":
			out = append(out, time.Wednesday)
		case "4", "thu", "thursday":
			out = append(out, time.Thursday)
		case "5", "fri", "friday":
			out = append(out, time.Friday)
		case "6", "sat", "saturday":
			out = append(out, time.Saturday)
		default:
			return nil, fmt.Errorf("invalid day_of_week %q", part)
		}
	}
	return out, nil
}
