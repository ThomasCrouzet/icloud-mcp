package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newUpdateEventTool() mcp.Tool {
	return mcp.NewTool("update_event",
		mcp.WithDescription("Met à jour les champs fournis d'un événement existant, localisé par UID. Les champs omis restent inchangés ; un champ texte fourni vide efface la valeur existante."),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithString("uid", mcp.Required(), mcp.Description("UID de l'événement (voir search_events)")),
		mcp.WithString("calendar", mcp.Required(), mcp.Description("Path du calendrier contenant l'événement")),
		mcp.WithString("title", mcp.MaxLength(icloud.MaxTitleLen), mcp.Description("Nouveau titre. Omis = inchangé ; vide = effacé.")),
		mcp.WithString("location", mcp.MaxLength(icloud.MaxLocationLen), mcp.Description("Nouveau lieu. Omis = inchangé ; vide = effacé.")),
		mcp.WithString("notes", mcp.MaxLength(icloud.MaxNotesLen), mcp.Description("Nouvelles notes. Omis = inchangé ; vide = effacé.")),
		mcp.WithString("start", mcp.Description("Nouveau début, RFC3339. Omis = inchangé.")),
		mcp.WithString("end", mcp.Description("Nouvelle fin, RFC3339. Omis = inchangé.")),
	)
}

type updateEventResponse struct {
	Success bool   `json:"success"`
	UID     string `json:"uid"`
}

func updateEventHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		uid, err := req.RequireString("uid")
		if err != nil {
			return errResult(deps.Redactor, "paramètre uid", err), nil
		}
		calendarPath, err := req.RequireString("calendar")
		if err != nil {
			return errResult(deps.Redactor, "paramètre calendar", err), nil
		}

		deny := func(context string, err error) (*mcp.CallToolResult, error) {
			deps.Audit.LogMutation("update_event", calendarPath, uid, "denied")
			return errResult(deps.Redactor, context, err), nil
		}

		if err := icloud.ValidateUID(uid); err != nil {
			return deny("paramètre uid", err)
		}
		if err := icloud.ValidateCalendarPath(calendarPath); err != nil {
			return deny("paramètre calendar", err)
		}

		args := req.GetArguments()
		update := &icloud.EventUpdate{}

		if v, exists := args["title"]; exists {
			s, _ := v.(string)
			if err := icloud.ValidateTextField("title", s, icloud.MaxTitleLen); err != nil {
				return deny("paramètre title", err)
			}
			update.Title = &s
		}
		if v, exists := args["location"]; exists {
			s, _ := v.(string)
			if err := icloud.ValidateTextField("location", s, icloud.MaxLocationLen); err != nil {
				return deny("paramètre location", err)
			}
			update.Location = &s
		}
		if v, exists := args["notes"]; exists {
			s, _ := v.(string)
			if err := icloud.ValidateTextField("notes", s, icloud.MaxNotesLen); err != nil {
				return deny("paramètre notes", err)
			}
			update.Notes = &s
		}

		var newStart, newEnd *time.Time
		if v, exists := args["start"]; exists {
			s, _ := v.(string)
			t, err := icloud.ParseRFC3339("start", s)
			if err != nil {
				return deny("validation", err)
			}
			update.StartTime = &t
			newStart = &t
		}
		if v, exists := args["end"]; exists {
			s, _ := v.(string)
			t, err := icloud.ParseRFC3339("end", s)
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

		if update.Title == nil && update.Location == nil && update.Notes == nil && update.StartTime == nil && update.EndTime == nil {
			return deny("validation", fmt.Errorf("aucun champ à modifier n'a été fourni (title/location/notes/start/end)"))
		}

		if err := deps.Service.UpdateEvent(ctx, calendarPath, uid, update); err != nil {
			deps.Audit.LogMutation("update_event", calendarPath, uid, "error")
			return errResult(deps.Redactor, "mise à jour de l'événement", err), nil
		}
		deps.Audit.LogMutation("update_event", calendarPath, uid, "success")

		return writeJSON(deps.Redactor, updateEventResponse{Success: true, UID: uid}), nil
	}
}
