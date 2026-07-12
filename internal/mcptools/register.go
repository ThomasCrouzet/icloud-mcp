package mcptools

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Deps regroupe les dépendances partagées par tous les handlers de tools.
type Deps struct {
	Service  icloud.Service
	Audit    *security.AuditLogger
	Redactor *security.Redactor
}

// Register enregistre les tools MCP sur le serveur. En mode readOnly, les 3
// tools d'écriture (create_event/update_event/delete_event) ne sont PAS
// enregistrés : ils sont donc absents de tools/list, pas seulement refusés à
// l'exécution, exigence explicite de la spec pour ICLOUD_MCP_READ_ONLY=1.
func Register(s *server.MCPServer, deps Deps, readOnly bool) {
	s.AddTool(newListCalendarsTool(), listCalendarsHandler(deps))
	s.AddTool(newSearchEventsTool(), searchEventsHandler(deps))
	if readOnly {
		return
	}
	s.AddTool(newCreateEventTool(), createEventHandler(deps))
	s.AddTool(newUpdateEventTool(), updateEventHandler(deps))
	s.AddTool(newDeleteEventTool(), deleteEventHandler(deps))
}
