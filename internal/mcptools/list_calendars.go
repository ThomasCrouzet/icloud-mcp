package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func newListCalendarsTool() mcp.Tool {
	return mcp.NewTool("list_calendars",
		mcp.WithDescription("Liste les calendriers iCloud disponibles (nom, path, couleur, description). À appeler en premier pour obtenir les paths utilisés par les autres tools."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

type calendarsResponse struct {
	Count     int               `json:"count"`
	Calendars []icloud.Calendar `json:"calendars"`
}

func listCalendarsHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cals, err := deps.Service.ListCalendars(ctx)
		if err != nil {
			return errResult(deps.Redactor, "liste des calendriers", err), nil
		}
		if cals == nil {
			cals = []icloud.Calendar{}
		}
		resp := calendarsResponse{Count: len(cals), Calendars: cals}
		return writeJSON(deps.Redactor, resp), nil
	}
}
