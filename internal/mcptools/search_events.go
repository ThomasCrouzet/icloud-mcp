package mcptools

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newSearchEventsTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("search_events",
		mcp.WithDescription("Searches iCloud calendar events over a date range. Recurring events are expanded into individual occurrences (capped at 2000 per series; truncatedByExpansion is set if a series was capped). Results are sorted by date, capped at 400 events (truncated=true), and paginated. When calendar is omitted, calendars are scanned in list order until 400 matching events are held (query filter applied per calendar before the budget; remaining calendars skipped: multiCalendarCapped=true)."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Range start", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("Range end", defaultLoc)+" At most 366 days after start.")),
		mcp.WithString("calendar", mcp.Description("Calendar path (see list_calendars). All calendars if omitted (best-effort under the 400-event cap and rate limits).")),
		mcp.WithString("query", mcp.MaxLength(icloud.MaxQueryLen), mcp.Description("Optional text filter (title/location/notes, case insensitive)")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Min(1), mcp.Max(icloud.MaxResults), mcp.Description("Maximum number of results per page (max 400)")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Min(0), mcp.Description("Pagination offset")),
	)
}

type searchEventsResponse struct {
	Count                int            `json:"count"`
	Total                int            `json:"total"`
	Offset               int            `json:"offset"`
	Limit                int            `json:"limit"`
	Truncated            bool           `json:"truncated"`
	TruncatedByExpansion bool           `json:"truncatedByExpansion,omitempty"`
	MultiCalendarCapped  bool           `json:"multiCalendarCapped,omitempty"`
	Events               []icloud.Event `json:"events"`
}

func searchEventsHandler(deps Deps) server.ToolHandlerFunc {
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

		calendarPath := req.GetString("calendar", "")
		if calendarPath != "" {
			if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
				return errResult(deps.Redactor, "calendar parameter", err), nil
			}
		}

		query := req.GetString("query", "")
		if err := icloud.ValidateTextField("query", query, icloud.MaxQueryLen); err != nil {
			return errResult(deps.Redactor, "query parameter", err), nil
		}

		limit := req.GetInt("limit", 100)
		if limit <= 0 {
			limit = 100
		}
		if limit > icloud.MaxResults {
			// Belt and suspenders: the schema (Max) already caps at 400, but
			// some MCP clients do not validate on their side.
			limit = icloud.MaxResults
		}
		offset := req.GetInt("offset", 0)
		if offset < 0 {
			offset = 0
		}

		var calendarPaths []string
		if calendarPath != "" {
			calendarPaths = []string{calendarPath}
		} else {
			cals, err := deps.Service.ListCalendars(ctx)
			if err != nil {
				return errResult(deps.Redactor, "listing calendars", err), nil
			}
			for _, c := range cals {
				calendarPaths = append(calendarPaths, c.Path)
			}
		}

		var all []icloud.Event
		var truncatedByExpansion bool
		var multiCalendarCapped bool
		// Multi-calendar policy: fetch calendars in list order; stop starting
		// new calendars once we already hold MaxResults *matching* events
		// (hard cap). Query filtering is applied per calendar BEFORE the
		// budget check so a busy first calendar full of non-matches cannot
		// hide later calendars that contain hits. Remaining calendars are
		// not queried (saves rate-limit tokens) once the cap is filled.
		for _, path := range calendarPaths {
			if calendarPath == "" && len(all) >= icloud.MaxResults {
				multiCalendarCapped = true
				break
			}
			result, err := deps.Service.SearchEvents(ctx, path, start, end)
			if err != nil {
				return errResult(deps.Redactor, "searching events", err), nil
			}
			if result.TruncatedByExpansion {
				truncatedByExpansion = true
			}
			batch := result.Events
			if query != "" {
				batch = filterByQuery(batch, query)
			}
			all = append(all, batch...)
		}

		sort.Slice(all, func(i, j int) bool { return all[i].StartTime.Before(all[j].StartTime) })

		total := len(all)
		truncated := total > icloud.MaxResults || multiCalendarCapped
		workable := all
		if total > icloud.MaxResults {
			workable = all[:icloud.MaxResults]
			truncated = true
		}

		pageStart := offset
		if pageStart > len(workable) {
			pageStart = len(workable)
		}
		pageEnd := pageStart + limit
		if pageEnd > len(workable) {
			pageEnd = len(workable)
		}
		page := workable[pageStart:pageEnd]
		if page == nil {
			page = []icloud.Event{}
		}

		resp := searchEventsResponse{
			Count:                len(page),
			Total:                total,
			Offset:               offset,
			Limit:                limit,
			Truncated:            truncated,
			TruncatedByExpansion: truncatedByExpansion,
			MultiCalendarCapped:  multiCalendarCapped,
			Events:               page,
		}
		return writeJSON(deps.Redactor, resp), nil
	}
}

// filterByQuery keeps the events whose title, location or notes contain
// query (case insensitive).
func filterByQuery(events []icloud.Event, query string) []icloud.Event {
	q := strings.ToLower(query)
	out := make([]icloud.Event, 0, len(events))
	for _, e := range events {
		if strings.Contains(strings.ToLower(e.Title), q) ||
			strings.Contains(strings.ToLower(e.Location), q) ||
			strings.Contains(strings.ToLower(e.Notes), q) {
			out = append(out, e)
		}
	}
	return out
}
