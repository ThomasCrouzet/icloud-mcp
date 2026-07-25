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

// maxExplicitFreeSlotCalendars bounds caller-controlled fan-out. Calendar
// discovery remains uncapped so omitting calendar selections still means all.
const maxExplicitFreeSlotCalendars = 10

func newFindFreeSlotsTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("find_free_slots",
		mcp.WithDescription("Computes free time slots locally from calendar busy intervals. Expands recurrences, ignores TRANSPARENT and CANCELLED events, merges overlaps, applies working hours/buffers/DST. Never reveals the events that cause busy time. Available in read-only mode."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Search range start", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("Search range end", defaultLoc)+" At most 366 days after start.")),
		mcp.WithInteger("duration_minutes", mcp.Required(), mcp.Min(1), mcp.Max(24*60), mcp.Description("Required free slot length in minutes")),
		mcp.WithString("calendar", mcp.Description("Single calendar path. Prefer calendars for multi-calendar.")),
		mcp.WithString("calendars", mcp.Description("Comma-separated calendar paths (at most 10 unique paths, including calendar). All calendars if both calendar and calendars omitted.")),
		mcp.WithString("timezone", mcp.Description("IANA timezone for working hours (default: ICLOUD_MCP_DEFAULT_TZ)")),
		mcp.WithString("working_hours_start", mcp.Description("Local start of working day HH:MM (optional)")),
		mcp.WithString("working_hours_end", mcp.Description("Local end of working day HH:MM (optional; may be before start for overnight)")),
		mcp.WithString("days_of_week", mcp.Description("Comma-separated weekdays: 0=Sunday .. 6=Saturday, or mon,tue,... Empty = all days.")),
		mcp.WithInteger("buffer_before_minutes", mcp.Min(0), mcp.Max(24*60), mcp.Description("Busy padding before each event (minutes)")),
		mcp.WithInteger("buffer_after_minutes", mcp.Min(0), mcp.Max(24*60), mcp.Description("Busy padding after each event (minutes)")),
		mcp.WithBoolean("include_all_day_busy", mcp.Description("Treat all-day events as busy (default true)")),
		mcp.WithInteger("limit", mcp.DefaultNumber(50), mcp.Min(1), mcp.Max(200), mcp.Description("Max free slots returned")),
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
		durMin, err := optionalIntArg(req, "duration_minutes", 0)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if durMin <= 0 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("duration_minutes must be positive")), nil
		}
		if durMin > 24*60 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("duration_minutes must be at most 1440 (24h)")), nil
		}
		bufBefore, err := optionalIntArg(req, "buffer_before_minutes", 0)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		bufAfter, err := optionalIntArg(req, "buffer_after_minutes", 0)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if bufBefore < 0 || bufBefore > 24*60 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("buffer_before_minutes must be between 0 and 1440")), nil
		}
		if bufAfter < 0 || bufAfter > 24*60 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("buffer_after_minutes must be between 0 and 1440")), nil
		}
		bufferBefore := time.Duration(bufBefore) * time.Minute
		bufferAfter := time.Duration(bufAfter) * time.Minute
		queryStart := start.Add(-bufferAfter)
		queryEnd := end.Add(bufferBefore)
		if queryStart.Year() < 0 || queryStart.Year() > 9999 || queryEnd.Year() < 0 || queryEnd.Year() > 9999 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("buffered search range is outside supported date bounds")), nil
		}
		if err := icloud.ValidateRange(queryStart, queryEnd); err != nil {
			return errResult(deps.Redactor, "validation", fmt.Errorf("buffered search range: %w", err)), nil
		}
		paths, err := resolveCalendarPaths(ctx, deps, req)
		if err != nil {
			return errResult(deps.Redactor, "calendars", err), nil
		}
		loc := deps.DefaultLocation
		if tz := req.GetString("timezone", ""); tz != "" {
			l, lerr := time.LoadLocation(tz)
			if lerr != nil {
				return errResult(deps.Redactor, "validation", fmt.Errorf("invalid timezone")), nil
			}
			loc = l
		}
		limit, err := optionalIntArg(req, "limit", 50)
		if err != nil || limit < 1 || limit > 200 {
			if err == nil {
				err = fmt.Errorf("limit must be between 1 and 200")
			}
			return errResult(deps.Redactor, "validation", err), nil
		}
		includeAllDayBusy, err := optionalBoolArg(req, "include_all_day_busy", true)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		opts := icloud.FreeSlotOptions{
			RangeStart:        start,
			RangeEnd:          end,
			Duration:          time.Duration(durMin) * time.Minute,
			Location:          loc,
			BufferBefore:      bufferBefore,
			BufferAfter:       bufferAfter,
			IncludeAllDayBusy: includeAllDayBusy,
			Limit:             limit,
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
			if err := ctx.Err(); err != nil {
				return errResult(deps.Redactor, "searching events", err), nil
			}
			res, serr := deps.Service.SearchEvents(ctx, path, queryStart, queryEnd, nil)
			if serr != nil {
				return errResult(deps.Redactor, "searching events", serr), nil
			}
			if err := ctx.Err(); err != nil {
				return errResult(deps.Redactor, "searching events", err), nil
			}
			if res.TruncatedByExpansion {
				return errResult(deps.Redactor, "searching events", icloud.NewError(
					icloud.CodePartialFailure, 0, "recurrence expansion was truncated; availability cannot be determined", nil,
				)), nil
			}
			allEvents = append(allEvents, res.Events...)
		}
		busy := icloud.BusyFromEvents(allEvents, opts.IncludeAllDayBusy, opts.BufferBefore, opts.BufferAfter, opts.Location)
		slots, err := icloud.FindFreeSlots(busy, opts)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		out := freeSlotsResponse{Count: len(slots), Slots: make([]freeSlotOutput, 0, len(slots))}
		for _, s := range slots {
			out.Slots = append(out.Slots, freeSlotOutput{
				Start: icloud.FormatEventTime(s.Start, false, opts.Location),
				End:   icloud.FormatEventTime(s.End, false, opts.Location),
			})
		}
		return writeCalendarJSON(deps.Redactor, out), nil
	}
}

func resolveCalendarPaths(ctx context.Context, deps Deps, req mcp.CallToolRequest) ([]string, error) {
	var paths []string
	seen := make(map[string]struct{})
	add := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		path = strings.TrimSpace(path)
		if path == "" {
			return nil
		}
		if err := icloud.ValidateCalendarPath(path); err != nil {
			return err
		}
		if _, ok := seen[path]; ok {
			return nil
		}
		if len(paths) >= maxExplicitFreeSlotCalendars {
			return fmt.Errorf("calendar selection must contain at most %d unique paths", maxExplicitFreeSlotCalendars)
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
		return nil
	}

	args := req.GetArguments()
	_, hasCalendars := args["calendars"]
	_, hasCalendar := args["calendar"]
	if multi := req.GetString("calendars", ""); hasCalendars {
		for _, p := range strings.Split(multi, ",") {
			if err := add(p); err != nil {
				return nil, err
			}
		}
	}
	if hasCalendar {
		if err := add(req.GetString("calendar", "")); err != nil {
			return nil, err
		}
	}
	if hasCalendars || hasCalendar {
		if len(paths) == 0 {
			return nil, fmt.Errorf("calendar selection must contain at least one non-empty path")
		}
		return paths, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cals, err := deps.Service.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cals {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.Path != "" {
			paths = append(paths, c.Path)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no calendars are available for free-slot search")
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
			return nil, fmt.Errorf("invalid day_of_week")
		}
	}
	return out, nil
}
