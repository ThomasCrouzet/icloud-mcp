package mcptools

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Deps groups the dependencies shared by all tool handlers.
type Deps struct {
	Service  icloud.Service
	Audit    *security.AuditLogger
	Redactor *security.Redactor
}

// Register registers the MCP tools on the server. In readOnly mode, the 3
// write tools (create_event/update_event/delete_event) are NOT registered:
// they are therefore absent from tools/list, not merely rejected at
// execution time, which is the required behavior for ICLOUD_MCP_READ_ONLY=1.
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
