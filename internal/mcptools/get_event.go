package mcptools

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func newGetEventTool() mcp.Tool {
	return mcp.NewTool("get_event",
		mcp.WithDescription("Fetches a single iCloud calendar event by calendar path and exact UID. Returns structured fields (title, times, status, transparency, URL, recurrence, alarms, etag) plus bounded overrides[] with recurrenceId for exception targeting. overridesTruncated and warnings report omissions required by the 256 KiB result budget. Does not expose internal server paths. Available in read-only mode."),
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
		return writeGetEventJSON(deps.Redactor, detail), nil
	}
}

func writeGetEventJSON(red *security.Redactor, detail *icloud.EventDetail) *mcp.CallToolResult {
	if detail == nil {
		return errResult(red, "formatting response", icloud.NewError(icloud.CodeInternal, 0, "Calendar event detail is missing", nil))
	}
	copyDetail := *detail
	allOverrides := detail.Overrides
	copyDetail.Overrides = make([]icloud.OccurrenceRef, 0, len(allOverrides))
	copyDetail.Warnings = append([]string(nil), detail.Warnings...)

	// Reserve the flags/key overhead before selecting complete override
	// objects. Each override is marshaled once for sizing; the whole detail is
	// marshaled only after the bounded selection has been built.
	probe := copyDetail
	probe.OverridesTruncated = len(allOverrides) > 0
	if probe.OverridesTruncated {
		probe.Warnings = append(probe.Warnings, "override summaries were omitted to fit the 256 KiB result limit")
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
		encoded, err := json.Marshal(allOverrides[i])
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
		copyDetail.Overrides = append(copyDetail.Overrides, allOverrides[i])
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
