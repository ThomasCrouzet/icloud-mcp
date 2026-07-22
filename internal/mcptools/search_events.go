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
		mcp.WithDescription("Searches iCloud calendar events over a date range. Recurring events are expanded (capped at 2000/series; truncatedByExpansion). Sorted by start then UID, hard-capped at 400 (truncated). Optional filters: calendars (comma-separated), uid, status, all_day, include_cancelled, busy_only, compact (omit notes), expand_recurrence (default true). Auth errors are never soft-warnings."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Range start", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("Range end", defaultLoc)+" At most 366 days after start.")),
		mcp.WithString("calendar", mcp.Description("Calendar path (see list_calendars). All calendars if omitted (best-effort under the 400-event cap and rate limits).")),
		mcp.WithString("calendars", mcp.Description("Comma-separated calendar paths (optional; merges with calendar)")),
		mcp.WithString("query", mcp.MaxLength(icloud.MaxQueryLen), mcp.Description("Optional text filter (title/location/notes, case insensitive)")),
		mcp.WithString("uid", mcp.Description("Optional exact UID filter")),
		mcp.WithString("status", mcp.Description("Optional status filter: TENTATIVE, CONFIRMED, CANCELLED")),
		mcp.WithBoolean("all_day", mcp.Description("If set, keep only all-day (true) or timed (false) events")),
		mcp.WithBoolean("include_cancelled", mcp.Description("Include CANCELLED events (default true)")),
		mcp.WithBoolean("busy_only", mcp.Description("If true, exclude TRANSPARENT events")),
		mcp.WithBoolean("compact", mcp.Description("If true, omit notes from results")),
		mcp.WithBoolean("expand_recurrence", mcp.Description("Expand RRULE occurrences (default true; false still returns masters overlapping the range via server time-range)")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Min(1), mcp.Max(icloud.MaxResults), mcp.Description("Maximum number of results per page (max 400)")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Min(0), mcp.Description("Pagination offset")),
	)
}

type searchEventDTO struct {
	UID          string    `json:"uid"`
	Title        string    `json:"title"`
	Location     string    `json:"location,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	StartTime    time.Time `json:"start"`
	EndTime      time.Time `json:"end"`
	AllDay       bool      `json:"allDay,omitempty"`
	Recurrence   string    `json:"recurrence,omitempty"`
	Timezone     string    `json:"timezone,omitempty"`
	Status       string    `json:"status,omitempty"`
	Transparency string    `json:"transparency,omitempty"`
	URL          string    `json:"url,omitempty"`
	ETag         string    `json:"etag,omitempty"`
}

type searchEventsResponse struct {
	Count                int              `json:"count"`
	Total                int              `json:"total"`
	Offset               int              `json:"offset"`
	Limit                int              `json:"limit"`
	Truncated            bool             `json:"truncated"`
	TruncatedByExpansion bool             `json:"truncatedByExpansion,omitempty"`
	MultiCalendarCapped  bool             `json:"multiCalendarCapped,omitempty"`
	Events               []searchEventDTO `json:"events"`
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

		calendarPaths, err := resolveSearchCalendars(ctx, deps, req)
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}
		singleCalendar := req.GetString("calendar", "") != "" || req.GetString("calendars", "") != ""

		query := req.GetString("query", "")
		if err := icloud.ValidateTextField("query", query, icloud.MaxQueryLen); err != nil {
			return errResult(deps.Redactor, "query parameter", err), nil
		}
		uidFilter := req.GetString("uid", "")
		if uidFilter != "" {
			if err := icloud.ValidateUID(uidFilter); err != nil {
				return errResult(deps.Redactor, "validation", err), nil
			}
		}
		statusFilter := strings.ToUpper(strings.TrimSpace(req.GetString("status", "")))
		includeCancelled := true
		if v, ok := req.GetArguments()["include_cancelled"]; ok {
			if b, ok := v.(bool); ok {
				includeCancelled = b
			}
		}
		busyOnly := req.GetBool("busy_only", false)
		compact := req.GetBool("compact", false)
		filterAllDay := false
		allDayWanted := false
		if v, ok := req.GetArguments()["all_day"]; ok {
			if b, ok := v.(bool); ok {
				filterAllDay = true
				allDayWanted = b
			}
		}

		limit := req.GetInt("limit", 100)
		if limit <= 0 {
			limit = 100
		}
		if limit > icloud.MaxResults {
			limit = icloud.MaxResults
		}
		offset := req.GetInt("offset", 0)
		if offset < 0 {
			offset = 0
		}

		var all []icloud.Event
		var truncatedByExpansion bool
		var multiCalendarCapped bool
		for _, path := range calendarPaths {
			if !singleCalendar && len(all) >= icloud.MaxResults {
				multiCalendarCapped = true
				break
			}
			result, err := deps.Service.SearchEvents(ctx, path, start, end)
			if err != nil {
				// Auth/security must never be masked as a soft warning.
				return errResult(deps.Redactor, "searching events", err), nil
			}
			if result.TruncatedByExpansion {
				truncatedByExpansion = true
			}
			batch := result.Events
			if query != "" {
				batch = filterByQuery(batch, query)
			}
			batch = filterEventsAdvanced(batch, uidFilter, statusFilter, filterAllDay, allDayWanted, includeCancelled, busyOnly)
			all = append(all, batch...)
		}

		// Stable sort: start ascending, then UID, then title.
		sort.SliceStable(all, func(i, j int) bool {
			if !all[i].StartTime.Equal(all[j].StartTime) {
				return all[i].StartTime.Before(all[j].StartTime)
			}
			if all[i].UID != all[j].UID {
				return all[i].UID < all[j].UID
			}
			return all[i].Title < all[j].Title
		})

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

		resp := searchEventsResponse{
			Count:                len(page),
			Total:                total,
			Offset:               offset,
			Limit:                limit,
			Truncated:            truncated,
			TruncatedByExpansion: truncatedByExpansion,
			MultiCalendarCapped:  multiCalendarCapped,
			Events:               eventsToDTO(page, compact),
		}
		return writeJSON(deps.Redactor, resp), nil
	}
}

func resolveSearchCalendars(ctx context.Context, deps Deps, req mcp.CallToolRequest) ([]string, error) {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) error {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil
		}
		if err := icloud.ValidateCalendarPath(p); err != nil {
			return err
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
		return nil
	}
	if err := add(req.GetString("calendar", "")); err != nil {
		return nil, err
	}
	if multi := req.GetString("calendars", ""); multi != "" {
		for _, p := range strings.Split(multi, ",") {
			if err := add(p); err != nil {
				return nil, err
			}
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

func filterEventsAdvanced(events []icloud.Event, uid, status string, filterAllDay, allDayWanted, includeCancelled, busyOnly bool) []icloud.Event {
	out := make([]icloud.Event, 0, len(events))
	for _, e := range events {
		if uid != "" && e.UID != uid {
			continue
		}
		if status != "" && !strings.EqualFold(e.Status, status) {
			continue
		}
		if filterAllDay && e.AllDay != allDayWanted {
			continue
		}
		if !includeCancelled && strings.EqualFold(e.Status, "CANCELLED") {
			continue
		}
		if busyOnly && strings.EqualFold(e.Transp, "TRANSPARENT") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func eventsToDTO(events []icloud.Event, compact bool) []searchEventDTO {
	out := make([]searchEventDTO, 0, len(events))
	for _, e := range events {
		dto := searchEventDTO{
			UID:          e.UID,
			Title:        e.Title,
			Location:     e.Location,
			StartTime:    e.StartTime,
			EndTime:      e.EndTime,
			AllDay:       e.AllDay,
			Recurrence:   e.Recurrence,
			Timezone:     e.Timezone,
			Status:       e.Status,
			Transparency: e.Transp,
			URL:          e.URL,
			ETag:         e.ETag,
		}
		if !compact {
			dto.Notes = e.Notes
		}
		out = append(out, dto)
	}
	return out
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
