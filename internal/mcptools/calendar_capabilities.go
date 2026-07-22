package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newCalendarCapabilitiesTool() mcp.Tool {
	return mcp.NewTool("calendar_capabilities",
		mcp.WithDescription("Returns local server capabilities and limits with NO network access and NO secrets (no email, password path, shard, DSID, local paths, env, calendars, or events). Available in read-only mode."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

type capabilitiesResponse struct {
	Version           string          `json:"version"`
	ReadOnly          bool            `json:"readOnly"`
	HealthcheckActive bool            `json:"healthcheckActive"`
	DefaultTimezone   string          `json:"defaultTimezone"`
	Features          map[string]bool `json:"features"`
	Limits            map[string]int  `json:"limits"`
	Tools             []string        `json:"tools"`
}

func calendarCapabilitiesHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx
		_ = req
		tools := []string{
			"list_calendars", "search_events", "get_event",
			"find_free_slots", "validate_event", "calendar_capabilities",
		}
		if !deps.ReadOnly {
			tools = append(tools, "create_event", "update_event", "delete_event")
		}
		resp := capabilitiesResponse{
			Version:           ServerVersion,
			ReadOnly:          deps.ReadOnly,
			HealthcheckActive: deps.HealthEnabled,
			DefaultTimezone:   defaultLocationName(deps.DefaultLocation),
			Features: map[string]bool{
				"recurrence_expansion":  true,
				"etag_if_match":         true,
				"series_occurrence":     true,
				"free_slots":            true,
				"structured_errors":     true,
				"validate_event_local":  true,
				"client_uid_idempotent": true,
				"this_and_future":       false, // not shipped unless proven correct
				"attendees_invitations": false,
			},
			Limits: map[string]int{
				"max_search_results":     icloud.MaxResults,
				"max_range_days":         icloud.MaxRangeDays,
				"max_title_len":          icloud.MaxTitleLen,
				"max_location_len":       icloud.MaxLocationLen,
				"max_notes_len":          icloud.MaxNotesLen,
				"max_alarms":             icloud.MaxAlarms,
				"max_occurrences_series": 2000,
				"read_rate_per_min":      60,
				"write_rate_per_min":     20,
				"http_timeout_seconds":   30,
				"tool_timeout_seconds":   25,
			},
			Tools: tools,
		}
		return writeJSON(deps.Redactor, resp), nil
	}
}
