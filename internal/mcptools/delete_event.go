package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newDeleteEventTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("delete_event",
		mcp.WithDescription("Deletes an event by UID. scope=series (default) removes the whole object; scope=occurrence adds EXDATE for recurrence_id and never deletes the series. Optional etag (If-Match) yields concurrent_modification on 412. dry_run=true validates and looks up without any PUT/DELETE. Idempotent for series: deleting a missing event returns not_found. Obtain human confirmation before real deletions."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("uid", mcp.Required(), mcp.Description("Event UID (see search_events)")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Path of the calendar containing the event")),
		mcp.WithString("scope", mcp.Description("series (default) or occurrence")),
		mcp.WithString("recurrence_id", mcp.Description(datetimeParamDescription("Occurrence RECURRENCE-ID when scope=occurrence", defaultLoc))),
		mcp.WithString("etag", mcp.Description("Optional If-Match ETag from get_event")),
		mcp.WithBoolean("dry_run", mcp.Description("If true, no PUT/DELETE is sent")),
	)
}

type deleteEventResponse struct {
	Success      bool   `json:"success"`
	UID          string `json:"uid"`
	DeletedTitle string `json:"deletedTitle,omitempty"`
	DryRun       bool   `json:"dryRun,omitempty"`
	Scope        string `json:"scope,omitempty"`
	WouldMutate  bool   `json:"wouldMutate,omitempty"`
}

func deleteEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		uid, err := req.RequireString("uid")
		if err != nil {
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		if err := icloud.ValidateUID(uid); err != nil {
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		opts := &icloud.DeleteOptions{
			IfMatchETag: req.GetString("etag", ""),
			DryRun:      req.GetBool("dry_run", false),
		}
		scope := req.GetString("scope", "series")
		switch scope {
		case "", "series":
			opts.Scope = icloud.ScopeSeries
		case "occurrence":
			opts.Scope = icloud.ScopeOccurrence
			ridStr := req.GetString("recurrence_id", "")
			if ridStr == "" {
				deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
				return errResult(deps.Redactor, "validation", fmt.Errorf("recurrence_id is required when scope=occurrence")), nil
			}
			rid, rerr := icloud.ParseDateTime("recurrence_id", ridStr, deps.DefaultLocation)
			if rerr != nil {
				deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
				return errResult(deps.Redactor, "validation", rerr), nil
			}
			opts.RecurrenceID = &rid
		default:
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "validation", fmt.Errorf("scope must be series or occurrence")), nil
		}

		res, err := deps.Service.DeleteEvent(ctx, calendarPath, uid, opts)
		if err != nil {
			deps.Audit.LogMutation("delete_event", calendarPath, uid, "error")
			return errResult(deps.Redactor, "deleting event", err), nil
		}
		status := "success"
		if res.DryRun {
			status = "dry_run"
		}
		deps.Audit.LogMutation("delete_event", calendarPath, uid, status)

		return writeJSON(deps.Redactor, deleteEventResponse{
			Success:      true,
			UID:          uid,
			DeletedTitle: res.Title,
			DryRun:       res.DryRun,
			Scope:        res.Scope,
			WouldMutate:  res.WouldMutate,
		}), nil
	}
}
