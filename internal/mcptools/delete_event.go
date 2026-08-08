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
		mcp.WithDescription("Deletes a series or excludes one occurrence. Use recurrenceId for an occurrence, etag for concurrency safety, and dry_run to validate. Obtain human confirmation before deletion."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("uid", mcp.Required(), mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Event UID; max 255 UTF-8 bytes")),
		mcp.WithString("calendar", mcp.Required(), mcp.MaxLength(1024), mcp.Description("Calendar path; max 1024 UTF-8 bytes")),
		mcp.WithString("scope", mcp.Enum("series", "occurrence"), mcp.Description("series (default) or occurrence")),
		mcp.WithString("recurrence_id", mcp.Description("Required for scope=occurrence; copy search_events.recurrenceId. All-day uses YYYY-MM-DD; timed values follow "+datetimeParamDescription("start", defaultLoc))),
		mcp.WithString("etag", mcp.Description("If-Match ETag from get_event or search_events; not *")),
		mcp.WithBoolean("dry_run", mcp.DefaultBool(false), mcp.Description("Validate without PUT or DELETE")),
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
			logCalendarMutation(deps.Audit, "delete_event", "", "", "denied")
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			logCalendarMutation(deps.Audit, "delete_event", "", uid, "denied")
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		if err := icloud.ValidateUID(uid); err != nil {
			logCalendarMutation(deps.Audit, "delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			logCalendarMutation(deps.Audit, "delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		deny := func(context string, err error) (*mcp.CallToolResult, error) {
			logCalendarMutation(deps.Audit, "delete_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, context, err), nil
		}

		etag, err := optionalStringArg(req, "etag", "")
		if err != nil {
			return deny("validation", err)
		}
		if err := icloud.ValidateIfMatchETag(etag); err != nil {
			return deny("etag parameter", err)
		}
		dryRun, err := optionalBoolArg(req, "dry_run", false)
		if err != nil {
			return deny("validation", err)
		}
		scope, err := optionalStringArg(req, "scope", "series")
		if err != nil {
			return deny("validation", err)
		}
		ridStr, err := optionalStringArg(req, "recurrence_id", "")
		if err != nil {
			return deny("validation", err)
		}
		opts := &icloud.DeleteOptions{
			IfMatchETag: etag,
			DryRun:      dryRun,
		}
		switch scope {
		case "", "series":
			if ridStr != "" {
				return deny("validation", fmt.Errorf("recurrence_id requires scope=occurrence"))
			}
			opts.Scope = icloud.ScopeSeries
		case "occurrence":
			opts.Scope = icloud.ScopeOccurrence
			if ridStr == "" {
				return deny("validation", fmt.Errorf("recurrence_id is required when scope=occurrence"))
			}
			rid, rerr := icloud.ParseRecurrenceID("recurrence_id", ridStr, deps.DefaultLocation)
			if rerr != nil {
				return deny("validation", rerr)
			}
			opts.RecurrenceID = &rid
		default:
			return deny("validation", fmt.Errorf("scope must be series or occurrence"))
		}

		res, err := deps.Service.DeleteEvent(ctx, calendarPath, uid, opts)
		if err != nil {
			logCalendarMutation(deps.Audit, "delete_event", calendarPath, uid, calendarMutationErrorStatus(err))
			return errResult(deps.Redactor, "deleting event", err), nil
		}
		status := "success"
		if res.DryRun {
			status = "dry_run"
		}
		logCalendarMutation(deps.Audit, "delete_event", calendarPath, uid, status)

		return writeCalendarJSON(deps.Redactor, deleteEventResponse{
			Success:      true,
			UID:          uid,
			DeletedTitle: res.Title,
			DryRun:       res.DryRun,
			Scope:        res.Scope,
			WouldMutate:  res.WouldMutate,
		}), nil
	}
}
