package mcptools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func newGetEventTool() mcp.Tool {
	return mcp.NewTool("get_event",
		mcp.WithDescription("Fetches a single iCloud calendar event by calendar path and exact UID. Returns structured fields (title, times, status, transparency, URL, recurrence, alarms, etag) plus bounded overrides[] with recurrenceId for exception targeting. Timed start/end use RFC3339 with an explicit offset in ICLOUD_MCP_DEFAULT_TZ. overridesTruncated and warnings report omissions required by the 256 KiB result budget. Does not expose internal server paths. Available in read-only mode."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithString("calendar", mcp.Required(), mcp.MaxLength(1024), mcp.Description("Calendar path (see list_calendars); runtime limit 1024 UTF-8 bytes")),
		mcp.WithString("uid", mcp.Required(), mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Event UID (exact match); runtime limit 255 UTF-8 bytes")),
	)
}

func getEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}
		uid, err := req.RequireString("uid")
		if err != nil {
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		if err := icloud.ValidateUID(uid); err != nil {
			return errResult(deps.Redactor, "validation", err), nil
		}
		detail, err := deps.Service.GetEvent(ctx, calendarPath, uid)
		if err != nil {
			return errResult(deps.Redactor, "getting event", err), nil
		}
		// Ensure path never leaks even if a future change serializes it.
		detail.Path = ""
		return writeGetEventJSON(deps.Redactor, detail, deps.DefaultLocation), nil
	}
}

type getEventDTO struct {
	UID                string             `json:"uid"`
	Title              string             `json:"title"`
	Location           string             `json:"location,omitempty"`
	Notes              string             `json:"notes,omitempty"`
	Start              string             `json:"start"`
	End                string             `json:"end"`
	AllDay             bool               `json:"allDay,omitempty"`
	Recurrence         string             `json:"recurrence,omitempty"`
	RecurrenceID       string             `json:"recurrenceId,omitempty"`
	IsOverride         bool               `json:"isOverride,omitempty"`
	Timezone           string             `json:"timezone,omitempty"`
	Status             string             `json:"status,omitempty"`
	Transparency       string             `json:"transparency,omitempty"`
	URL                string             `json:"url,omitempty"`
	ETag               string             `json:"etag,omitempty"`
	Alarms             []icloud.AlarmInfo `json:"alarms,omitempty"`
	IsRecurring        bool               `json:"isRecurring,omitempty"`
	OverrideCount      int                `json:"overrideCount,omitempty"`
	Overrides          []occurrenceDTO    `json:"overrides,omitempty"`
	OverridesTruncated bool               `json:"overridesTruncated,omitempty"`
	Warnings           []string           `json:"warnings,omitempty"`
}

type occurrenceDTO struct {
	RecurrenceID string `json:"recurrenceId"`
	Start        string `json:"start"`
	End          string `json:"end"`
	Title        string `json:"title,omitempty"`
	IsOverride   bool   `json:"isOverride"`
}

func eventDetailToDTO(detail *icloud.EventDetail, loc *time.Location) getEventDTO {
	dto := getEventDTO{
		UID:           detail.UID,
		Title:         detail.Title,
		Location:      detail.Location,
		Notes:         detail.Notes,
		Start:         icloud.FormatEventTime(detail.StartTime, detail.AllDay, loc),
		End:           icloud.FormatEventTime(detail.EndTime, detail.AllDay, loc),
		AllDay:        detail.AllDay,
		Recurrence:    detail.Recurrence,
		IsOverride:    detail.IsOverride,
		Timezone:      detail.Timezone,
		Status:        detail.Status,
		Transparency:  detail.Transp,
		URL:           detail.URL,
		ETag:          detail.ETag,
		Alarms:        detail.Alarms,
		IsRecurring:   detail.IsRecurring,
		OverrideCount: detail.OverrideCount,
		Warnings:      append([]string(nil), detail.Warnings...),
	}
	if !detail.RecurrenceID.IsZero() {
		dto.RecurrenceID = icloud.FormatEventTime(detail.RecurrenceID, detail.AllDay, loc)
	}
	return dto
}

func writeGetEventJSON(red *security.Redactor, detail *icloud.EventDetail, loc *time.Location) *mcp.CallToolResult {
	if detail == nil {
		return errResult(red, "formatting response", icloud.NewError(icloud.CodeInternal, 0, "Calendar event detail is missing", nil))
	}
	copyDetail := eventDetailToDTO(detail, loc)
	allOverrides := detail.Overrides
	copyDetail.Overrides = make([]occurrenceDTO, 0, len(allOverrides))

	// Reserve the flags/key overhead before selecting complete override
	// objects. Each override is marshaled once for sizing; the whole detail is
	// marshaled only after the bounded selection has been built.
	probe := copyDetail
	probe.OverridesTruncated = len(allOverrides) > 0
	if probe.OverridesTruncated {
		probe.Warnings = append(append([]string(nil), detail.Warnings...), "override summaries were omitted to fit the 256 KiB result limit")
	}
	base, err := json.Marshal(probe)
	if err != nil {
		return errResult(red, "formatting response", err)
	}
	used := len(red.Redact(string(base))) + 64
	if used > maxCalendarResultBytes {
		return writeCalendarEncoded(red, base)
	}
	for i := range allOverrides {
		ov := occurrenceDTO{
			RecurrenceID: icloud.FormatEventTime(allOverrides[i].RecurrenceID, detail.AllDay, loc),
			Start:        icloud.FormatEventTime(allOverrides[i].StartTime, detail.AllDay, loc),
			End:          icloud.FormatEventTime(allOverrides[i].EndTime, detail.AllDay, loc),
			Title:        allOverrides[i].Title,
			IsOverride:   allOverrides[i].IsOverride,
		}
		encoded, err := json.Marshal(ov)
		if err != nil {
			return errResult(red, "formatting response", err)
		}
		addition := len(red.Redact(string(encoded)))
		if len(copyDetail.Overrides) > 0 {
			addition++
		}
		if used+addition > maxCalendarResultBytes {
			break
		}
		copyDetail.Overrides = append(copyDetail.Overrides, ov)
		used += addition
	}

	if len(copyDetail.Overrides) < len(allOverrides) {
		copyDetail.OverridesTruncated = true
		copyDetail.Warnings = append(copyDetail.Warnings, "override summaries were omitted to fit the 256 KiB result limit")
	}
	for {
		encoded, err := json.Marshal(copyDetail)
		if err != nil {
			return errResult(red, "formatting response", err)
		}
		if len(red.Redact(string(encoded))) <= maxCalendarResultBytes {
			return writeCalendarEncoded(red, encoded)
		}
		if len(copyDetail.Overrides) == 0 {
			return writeCalendarEncoded(red, encoded)
		}
		copyDetail.Overrides = copyDetail.Overrides[:len(copyDetail.Overrides)-1]
		copyDetail.OverridesTruncated = true
		if len(copyDetail.Warnings) == len(detail.Warnings) {
			copyDetail.Warnings = append(copyDetail.Warnings, "override summaries were omitted to fit the 256 KiB result limit")
		}
	}
}
