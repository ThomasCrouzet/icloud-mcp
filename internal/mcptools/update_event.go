package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newUpdateEventTool(defaultLoc *time.Location) mcp.Tool {
	return mcp.NewTool("update_event",
		mcp.WithDescription("Updates an event series or one occurrence. Omitted fields stay unchanged; empty text clears. Use recurrenceId for an occurrence, etag for concurrency safety, and idempotency_key for retries."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithString("uid", mcp.Required(), mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Event UID; max 255 UTF-8 bytes")),
		mcp.WithString("calendar", mcp.Required(), mcp.MaxLength(1024), mcp.Description("Calendar path; max 1024 UTF-8 bytes")),
		mcp.WithString("title", mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("New title; empty clears; max 500 UTF-8 bytes")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("New location; empty clears; max 1000 UTF-8 bytes")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("New notes; empty clears; max 4000 UTF-8 bytes")),
		mcp.WithString("start", mcp.Description(datetimeParamDescription("New start time. Omitted = unchanged", defaultLoc))),
		mcp.WithString("end", mcp.Description(datetimeParamDescription("New end time. Omitted = unchanged", defaultLoc))),
		mcp.WithString("status", mcp.Description("TENTATIVE, CONFIRMED, or CANCELLED")),
		mcp.WithString("transparency", mcp.Description("OPAQUE or TRANSPARENT")),
		mcp.WithString("url", mcp.MaxLength(icloud.MaxURLLen), mcp.Description("http(s) URL; max 2000 UTF-8 bytes")),
		mcp.WithString("scope", mcp.Enum("series", "occurrence"), mcp.Description("series (default) or occurrence")),
		mcp.WithString("recurrence_id", mcp.Description("Required for scope=occurrence; copy search_events.recurrenceId. All-day uses YYYY-MM-DD; timed values follow "+datetimeParamDescription("start", defaultLoc))),
		mcp.WithString("etag", mcp.Description("If-Match ETag from get_event or search_events; not *")),
		mcp.WithString("idempotency_key", mcp.MaxLength(icloud.MaxUIDLen), mcp.Description("Retry key; reuse with different params conflicts")),
	)
}

type updateEventResponse struct {
	Success bool   `json:"success"`
	UID     string `json:"uid"`
	Scope   string `json:"scope,omitempty"`
}

func updateEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		uid, err := req.RequireString("uid")
		if err != nil {
			logCalendarMutation(deps.Audit, "update_event", "", "", "denied")
			return errResult(deps.Redactor, "uid parameter", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			logCalendarMutation(deps.Audit, "update_event", "", uid, "denied")
			return errResult(deps.Redactor, "calendar parameter", err), nil
		}

		deny := func(context string, err error) (*mcp.CallToolResult, error) {
			logCalendarMutation(deps.Audit, "update_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, context, err), nil
		}

		if err := icloud.ValidateUID(uid); err != nil {
			return deny("uid parameter", err)
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return deny("calendar parameter", err)
		}

		args := req.GetArguments()
		etag, err := optionalStringArg(req, "etag", "")
		if err != nil {
			return deny("validation", err)
		}
		if err := icloud.ValidateIfMatchETag(etag); err != nil {
			return deny("etag parameter", err)
		}
		update := &icloud.EventUpdate{
			IfMatchETag: etag,
		}

		optionalString := func(key string) (string, bool, error) {
			v, exists := args[key]
			if !exists {
				return "", false, nil
			}
			s, ok := v.(string)
			if !ok {
				return "", false, fmt.Errorf("%s must be a string", key)
			}
			return s, true, nil
		}

		if s, ok, err := optionalString("title"); err != nil {
			return deny("title parameter", err)
		} else if ok {
			if err := icloud.ValidateTextField("title", s, icloud.MaxTitleLen); err != nil {
				return deny("title parameter", err)
			}
			update.Title = &s
		}
		if s, ok, err := optionalString("location"); err != nil {
			return deny("location parameter", err)
		} else if ok {
			if err := icloud.ValidateTextField("location", s, icloud.MaxLocationLen); err != nil {
				return deny("location parameter", err)
			}
			update.Location = &s
		}
		if s, ok, err := optionalString("notes"); err != nil {
			return deny("notes parameter", err)
		} else if ok {
			if err := icloud.ValidateTextField("notes", s, icloud.MaxNotesLen); err != nil {
				return deny("notes parameter", err)
			}
			update.Notes = &s
		}

		var newStart, newEnd *time.Time
		if s, ok, err := optionalString("start"); err != nil {
			return deny("start parameter", err)
		} else if ok {
			t, err := icloud.ParseDateTime("start", s, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			update.StartTime = &t
			newStart = &t
		}
		if s, ok, err := optionalString("end"); err != nil {
			return deny("end parameter", err)
		} else if ok {
			t, err := icloud.ParseDateTime("end", s, deps.DefaultLocation)
			if err != nil {
				return deny("validation", err)
			}
			update.EndTime = &t
			newEnd = &t
		}
		if newStart != nil && newEnd != nil {
			if err := icloud.ValidateRange(*newStart, *newEnd); err != nil {
				return deny("validation", err)
			}
		}
		if s, ok, err := optionalString("status"); err != nil {
			return deny("status parameter", err)
		} else if ok {
			update.Status = &s
		}
		if s, ok, err := optionalString("transparency"); err != nil {
			return deny("transparency parameter", err)
		} else if ok {
			update.Transparency = &s
		}
		if s, ok, err := optionalString("url"); err != nil {
			return deny("url parameter", err)
		} else if ok {
			update.URL = &s
		}
		// Same status/transparency/URL policy as create_event / validate_event.
		// Reject before any service (CalDAV) call so invalid input never mutates.
		if err := icloud.ValidateEventUpdateFields(update); err != nil {
			return deny("validation", err)
		}
		icloud.NormalizeEventUpdateFields(update)

		scope, err := optionalStringArg(req, "scope", "series")
		if err != nil {
			return deny("validation", err)
		}
		ridStr, err := optionalStringArg(req, "recurrence_id", "")
		if err != nil {
			return deny("validation", err)
		}
		switch scope {
		case "", "series":
			if ridStr != "" {
				return deny("validation", fmt.Errorf("recurrence_id requires scope=occurrence"))
			}
			update.Scope = icloud.ScopeSeries
		case "occurrence":
			update.Scope = icloud.ScopeOccurrence
			if ridStr == "" {
				return deny("validation", fmt.Errorf("recurrence_id is required when scope=occurrence"))
			}
			rid, rerr := icloud.ParseRecurrenceID("recurrence_id", ridStr, deps.DefaultLocation)
			if rerr != nil {
				return deny("validation", rerr)
			}
			update.RecurrenceID = &rid
		default:
			return deny("validation", fmt.Errorf("scope must be series or occurrence"))
		}

		if update.Title == nil && update.Location == nil && update.Notes == nil &&
			update.StartTime == nil && update.EndTime == nil &&
			update.Status == nil && update.Transparency == nil && update.URL == nil {
			return deny("validation", fmt.Errorf("no field to update was provided"))
		}

		idemKey, err := optionalStringArg(req, "idempotency_key", "")
		if err != nil {
			return deny("validation", err)
		}
		var paramsHash string
		var nsKey string
		var idemReady bool
		if idemKey != "" {
			paramsHash, err = hashIdempotencyParams(map[string]any{
				"tool":          "update_event",
				"uid":           uid,
				"calendar":      calendarPath,
				"etag":          etag,
				"title":         update.Title,
				"location":      update.Location,
				"notes":         update.Notes,
				"start":         update.StartTime,
				"end":           update.EndTime,
				"status":        update.Status,
				"transparency":  update.Transparency,
				"url":           update.URL,
				"scope":         update.Scope,
				"recurrence_id": update.RecurrenceID,
			})
			if err != nil {
				return deny("validation", err)
			}
			nsKey = namespacedIdempotencyKey("update_event", idemKey)
			payload, conflict, hit, ready := defaultIdempotency.beginContext(ctx, nsKey, paramsHash)
			if conflict {
				return deny("validation", icloud.NewError(icloud.CodeConflict, 0,
					"idempotency_key was reused with different update parameters", nil))
			}
			if hit {
				return writeCalendarEncoded(deps.Redactor, []byte(payload)), nil
			}
			if !ready {
				if ctx.Err() != nil {
					return deny("idempotency wait", icloud.NewError(
						icloud.CodeTimeout, 0, "tool deadline reached while waiting for the idempotency key", nil,
					))
				}
				return deny("validation", icloud.NewError(icloud.CodeConflict, 0,
					"idempotency_key cache is full; retry without a key or later", nil))
			}
			idemReady = true
		}
		idemDone := false
		if idemReady {
			defer func() {
				if !idemDone {
					defaultIdempotency.abort(nsKey, paramsHash)
				}
			}()
		}

		if err := deps.Service.UpdateEvent(ctx, calendarPath, uid, update); err != nil {
			logCalendarMutation(deps.Audit, "update_event", calendarPath, uid, calendarMutationErrorStatus(err))
			return errResult(deps.Redactor, "updating event", err), nil
		}
		logCalendarMutation(deps.Audit, "update_event", calendarPath, uid, "success")

		resp := updateEventResponse{
			Success: true,
			UID:     uid,
			Scope:   string(update.Scope),
		}
		result := writeCalendarJSON(deps.Redactor, resp)
		if idemReady {
			if text, ok := calendarResultText(result); ok {
				defaultIdempotency.complete(nsKey, paramsHash, text)
				idemDone = true
			}
		}
		return result, nil
	}
}
