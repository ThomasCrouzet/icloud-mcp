package mcptools

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newSearchEventsTool() mcp.Tool {
	return mcp.NewTool("search_events",
		mcp.WithDescription("Recherche les événements du calendrier iCloud sur une plage de dates. Les récurrences sont développées en occurrences individuelles. Résultat trié par date, borné à 400 événements maximum, paginé."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("start", mcp.Required(), mcp.Description("Début de la plage, RFC3339 (ex. 2026-07-01T00:00:00Z)")),
		mcp.WithString("end", mcp.Required(), mcp.Description("Fin de la plage, RFC3339 (max 366 jours après start)")),
		mcp.WithString("calendar", mcp.Description("Path du calendrier (voir list_calendars). Tous les calendriers si omis.")),
		mcp.WithString("query", mcp.MaxLength(icloud.MaxQueryLen), mcp.Description("Filtre texte optionnel (titre/lieu/notes, insensible à la casse)")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Min(1), mcp.Max(icloud.MaxResults), mcp.Description("Nombre maximum de résultats par page (max 400)")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Min(0), mcp.Description("Décalage de pagination")),
	)
}

type searchEventsResponse struct {
	Count     int            `json:"count"`
	Total     int            `json:"total"`
	Offset    int            `json:"offset"`
	Limit     int            `json:"limit"`
	Truncated bool           `json:"truncated"`
	Events    []icloud.Event `json:"events"`
}

func searchEventsHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		startStr, err := req.RequireString("start")
		if err != nil {
			return errResult(deps.Redactor, "paramètre start", err), nil
		}
		endStr, err := req.RequireString("end")
		if err != nil {
			return errResult(deps.Redactor, "paramètre end", err), nil
		}
		start, err := icloud.ParseRFC3339("start", startStr)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		end, err := icloud.ParseRFC3339("end", endStr)
		if err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if err := icloud.ValidateRange(start, end); err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}

		calendarPath := req.GetString("calendar", "")
		if calendarPath != "" {
			if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
				return errResult(deps.Redactor, "paramètre calendar", err), nil
			}
		}

		query := req.GetString("query", "")
		if err := icloud.ValidateTextField("query", query, icloud.MaxQueryLen); err != nil {
			return errResult(deps.Redactor, "paramètre query", err), nil
		}

		limit := req.GetInt("limit", 100)
		if limit <= 0 {
			limit = 100
		}
		if limit > icloud.MaxResults {
			// Ceinture + bretelles : le schéma (Max) borne déjà à 400, mais
			// certains clients MCP ne valident pas côté client.
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
				return errResult(deps.Redactor, "liste des calendriers", err), nil
			}
			for _, c := range cals {
				calendarPaths = append(calendarPaths, c.Path)
			}
		}

		var all []icloud.Event
		for _, path := range calendarPaths {
			events, err := deps.Service.SearchEvents(ctx, path, start, end)
			if err != nil {
				return errResult(deps.Redactor, "recherche d'événements", err), nil
			}
			all = append(all, events...)
		}

		if query != "" {
			all = filterByQuery(all, query)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].StartTime.Before(all[j].StartTime) })

		total := len(all)
		truncated := total > icloud.MaxResults
		workable := all
		if truncated {
			workable = all[:icloud.MaxResults]
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
			Count:     len(page),
			Total:     total,
			Offset:    offset,
			Limit:     limit,
			Truncated: truncated,
			Events:    page,
		}
		return writeJSON(deps.Redactor, resp), nil
	}
}

// filterByQuery filtre les événements dont le titre, le lieu ou les notes
// contiennent query (insensible à la casse).
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
