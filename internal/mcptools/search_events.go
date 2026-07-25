package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const maxExplicitSearchCalendars = 10

func newSearchEventsTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("search_events",
		mcp.WithDescription("Searches iCloud calendar events over a date range. Recurring events are expanded (capped at 2000/series; truncatedByExpansion). Each occurrence includes recurrenceId (use with scope=occurrence) and etag when known; expanded rows omit master RRULE. Sorted by start then UID, hard-capped at 400 and 256 KiB at event boundaries (truncated). Multi-calendar: all calendars are queried then capped fairly; non-auth errors become partialFailure+warnings. Optional filters: calendars, uid, status, all_day, include_cancelled, busy_only, compact, expand_recurrence (default true). Auth, payload-limit, and protocol errors are never soft-warnings."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Required(), mcp.Description(datetimeParamDescription("Range start", defaultLoc))),
		mcp.WithString("end", mcp.Required(), mcp.Description(datetimeParamDescription("Range end", defaultLoc)+" At most 366 days after start.")),
		mcp.WithString("calendar", mcp.Description("Calendar path (see list_calendars). All calendars if omitted (best-effort under the 400-event cap and rate limits).")),
		mcp.WithString("calendars", mcp.Description("Comma-separated calendar paths (at most 10 unique paths, including calendar; optional; merges with calendar)")),
		mcp.WithString("query", mcp.MaxLength(icloud.MaxQueryLen), mcp.Description("Optional text filter (title/location/notes, case insensitive); runtime limit 200 UTF-8 bytes")),
		mcp.WithString("uid", mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Optional exact UID filter; runtime limit 255 UTF-8 bytes")),
		mcp.WithString("status", mcp.Enum("TENTATIVE", "CONFIRMED", "CANCELLED"), mcp.Description("Optional status filter")),
		mcp.WithBoolean("all_day", mcp.Description("If set, keep only all-day (true) or timed (false) events")),
		mcp.WithBoolean("include_cancelled", mcp.DefaultBool(true), mcp.Description("Include CANCELLED events")),
		mcp.WithBoolean("busy_only", mcp.DefaultBool(false), mcp.Description("If true, exclude TRANSPARENT events")),
		mcp.WithBoolean("compact", mcp.DefaultBool(false), mcp.Description("If true, omit notes from results. Prefer true for wide ranges to reduce PII in the agent context; false keeps notes for detail views.")),
		mcp.WithBoolean("expand_recurrence", mcp.DefaultBool(true), mcp.Description("Expand RRULE occurrences; false still returns masters overlapping the range via server time-range")),
		mcp.WithInteger("limit", mcp.DefaultNumber(100), mcp.Min(1), mcp.Max(icloud.MaxResults), mcp.Description("Maximum number of results per page (max 400)")),
		mcp.WithInteger("offset", mcp.DefaultNumber(0), mcp.Min(0), mcp.Description("Pagination offset")),
	)
}

type searchEventDTO struct {
	UID          string `json:"uid"`
	Title        string `json:"title"`
	Location     string `json:"location,omitempty"`
	Notes        string `json:"notes,omitempty"`
	StartTime    string `json:"start"`
	EndTime      string `json:"end"`
	AllDay       bool   `json:"allDay,omitempty"`
	Recurrence   string `json:"recurrence,omitempty"`
	RecurrenceID string `json:"recurrenceId,omitempty"`
	IsOverride   bool   `json:"isOverride,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Status       string `json:"status,omitempty"`
	Transparency string `json:"transparency,omitempty"`
	URL          string `json:"url,omitempty"`
	ETag         string `json:"etag,omitempty"`
}

type searchEventsResponse struct {
	Count                int              `json:"count"`
	Total                int              `json:"total"`
	Offset               int              `json:"offset"`
	Limit                int              `json:"limit"`
	Truncated            bool             `json:"truncated"`
	TruncatedByExpansion bool             `json:"truncatedByExpansion,omitempty"`
	MultiCalendarCapped  bool             `json:"multiCalendarCapped,omitempty"`
	PartialFailure       bool             `json:"partialFailure,omitempty"`
	Warnings             []string         `json:"warnings,omitempty"`
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
		if statusFilter != "" && statusFilter != "TENTATIVE" && statusFilter != "CONFIRMED" && statusFilter != "CANCELLED" {
			return errResult(deps.Redactor, "validation", fmt.Errorf("status must be TENTATIVE, CONFIRMED, or CANCELLED")), nil
		}
		includeCancelled, err := optionalBoolArg(req, "include_cancelled", true)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		busyOnly, err := optionalBoolArg(req, "busy_only", false)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		compact, err := optionalBoolArg(req, "compact", false)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		// expand_recurrence defaults true when omitted.
		expandRecurrence, err := optionalBoolArg(req, "expand_recurrence", true)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		searchOpts := &icloud.SearchOptions{ExpandRecurrence: expandRecurrence}
		filterAllDay := false
		allDayWanted := false
		if _, ok := req.GetArguments()["all_day"]; ok {
			allDayWanted, err = optionalBoolArg(req, "all_day", false)
			if err != nil {
				return errResult(deps.Redactor, "validation", err), nil
			}
			filterAllDay = true
		}

		limit, err := optionalIntArg(req, "limit", 100)
		if err != nil || limit < 1 || limit > icloud.MaxResults {
			if err == nil {
				err = fmt.Errorf("limit must be between 1 and %d", icloud.MaxResults)
			}
			return errResult(deps.Redactor, "validation", err), nil
		}
		offset, err := optionalIntArg(req, "offset", 0)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if offset < 0 {
			return errResult(deps.Redactor, "validation", fmt.Errorf("offset must be non-negative")), nil
		}

		var all []icloud.Event
		var truncatedByExpansion bool
		var multiCalendarCapped bool
		var warnings []string
		var partialFailure bool
		multi := len(calendarPaths) > 1 || !singleCalendar
		for _, path := range calendarPaths {
			result, err := deps.Service.SearchEvents(ctx, path, start, end, searchOpts)
			if err != nil {
				// Auth/security must never be masked as a soft warning.
				if ie := icloud.AsICloudError(err); ie != nil {
					switch ie.Code {
					case icloud.CodeAuthenticationRefused, icloud.CodeForbidden,
						icloud.CodeAuthentication, icloud.CodeAuthorization,
						icloud.CodePayloadTooLarge, icloud.CodeProtocolError:
						return errResult(deps.Redactor, "searching events", err), nil
					}
				}
				if !multi {
					return errResult(deps.Redactor, "searching events", err), nil
				}
				partialFailure = true
				warnings = append(warnings, deps.Redactor.Redact(fmt.Sprintf("calendar %s: %v", path, err)))
				continue
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
		if multi && partialFailure && len(all) == 0 && len(warnings) > 0 {
			return errResult(deps.Redactor, "searching events", icloud.NewError(
				icloud.CodePartialFailure, 0,
				"all calendars failed: "+strings.Join(warnings, "; "), nil,
			)), nil
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

		if total > icloud.MaxResults {
			multiCalendarCapped = multiCalendarCapped || multi
		}
		resp := searchEventsResponse{
			Count:                len(page),
			Total:                total,
			Offset:               offset,
			Limit:                limit,
			Truncated:            truncated,
			TruncatedByExpansion: truncatedByExpansion,
			MultiCalendarCapped:  multiCalendarCapped,
			PartialFailure:       partialFailure,
			Warnings:             warnings,
			Events:               eventsToDTO(page, compact, deps.DefaultLocation),
		}
		return writeSearchEventsJSON(deps.Redactor, &resp), nil
	}
}

func writeSearchEventsJSON(red *security.Redactor, resp *searchEventsResponse) *mcp.CallToolResult {
	if resp == nil {
		return errResult(red, "formatting response", icloud.NewError(icloud.CodeInternal, 0, "Calendar search result is missing", nil))
	}
	allEvents := resp.Events
	probe := *resp
	probe.Events = []searchEventDTO{}
	probe.Count = len(allEvents)
	probe.Truncated = true
	base, err := json.Marshal(probe)
	if err != nil {
		return errResult(red, "formatting response", err)
	}
	used := len(red.Redact(string(base)))
	if used > maxCalendarResultBytes {
		return writeCalendarEncoded(red, base)
	}

	selected := make([]searchEventDTO, 0, len(allEvents))
	for i := range allEvents {
		encoded, err := json.Marshal(allEvents[i])
		if err != nil {
			return errResult(red, "formatting response", err)
		}
		addition := len(red.Redact(string(encoded)))
		if len(selected) > 0 {
			addition++ // comma between event objects
		}
		if used+addition > maxCalendarResultBytes {
			break
		}
		selected = append(selected, allEvents[i])
		used += addition
	}

	resp.Events = selected
	resp.Count = len(selected)
	if len(selected) < len(allEvents) {
		resp.Truncated = true
	}
	for {
		encoded, err := json.Marshal(resp)
		if err != nil {
			return errResult(red, "formatting response", err)
		}
		if len(red.Redact(string(encoded))) <= maxCalendarResultBytes {
			return writeCalendarEncoded(red, encoded)
		}
		if len(resp.Events) == 0 {
			return writeCalendarEncoded(red, encoded)
		}
		resp.Events = resp.Events[:len(resp.Events)-1]
		resp.Count = len(resp.Events)
		resp.Truncated = true
	}
}

func resolveSearchCalendars(ctx context.Context, deps Deps, req mcp.CallToolRequest) ([]string, error) {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p = strings.TrimSpace(p)
		if p == "" {
			return nil
		}
		if err := icloud.ValidateCalendarPath(p); err != nil {
			return err
		}
		if !seen[p] {
			if len(paths) >= maxExplicitSearchCalendars {
				return fmt.Errorf("calendar selection must contain at most %d unique paths", maxExplicitSearchCalendars)
			}
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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

func eventsToDTO(events []icloud.Event, compact bool, loc *time.Location) []searchEventDTO {
	out := make([]searchEventDTO, 0, len(events))
	for _, e := range events {
		dto := searchEventDTO{
			UID:          e.UID,
			Title:        e.Title,
			Location:     e.Location,
			StartTime:    icloud.FormatEventTime(e.StartTime, e.AllDay, loc),
			EndTime:      icloud.FormatEventTime(e.EndTime, e.AllDay, loc),
			AllDay:       e.AllDay,
			Recurrence:   e.Recurrence,
			Timezone:     e.Timezone,
			Status:       e.Status,
			Transparency: e.Transp,
			URL:          e.URL,
			ETag:         e.ETag,
			IsOverride:   e.IsOverride,
		}
		if !e.RecurrenceID.IsZero() {
			dto.RecurrenceID = icloud.FormatEventTime(e.RecurrenceID, e.AllDay, loc)
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
