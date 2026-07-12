package mcptools

import (
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Deps groups the dependencies shared by all tool handlers.
type Deps struct {
	Service  icloud.Service
	Audit    *security.AuditLogger
	Redactor *security.Redactor

	// DefaultLocation is the timezone used to interpret start/end values
	// supplied without an explicit RFC3339 offset (ICLOUD_MCP_DEFAULT_TZ).
	// nil is treated as UTC by icloud.ParseDateTime and by
	// datetimeParamDescription.
	DefaultLocation *time.Location
}

// defaultLocationName returns the display name used in tool descriptions
// and error messages: loc.String() if set, "UTC" otherwise (mirrors the nil
// handling in icloud.ParseDateTime so the schema never lies about the
// actual parsing behavior).
func defaultLocationName(loc *time.Location) string {
	if loc == nil {
		return "UTC"
	}
	return loc.String()
}

// Register registers the MCP tools on the server. In readOnly mode, the 3
// write tools (create_event/update_event/delete_event) are NOT registered:
// they are therefore absent from tools/list, not merely rejected at
// execution time, which is the required behavior for ICLOUD_MCP_READ_ONLY=1.
func Register(s *server.MCPServer, deps Deps, readOnly bool) {
	s.AddTool(newListCalendarsTool(), listCalendarsHandler(deps))
	s.AddTool(newSearchEventsTool(deps.DefaultLocation), searchEventsHandler(deps))
	if readOnly {
		return
	}
	s.AddTool(newCreateEventTool(deps.DefaultLocation), createEventHandler(deps))
	s.AddTool(newUpdateEventTool(deps.DefaultLocation), updateEventHandler(deps))
	s.AddTool(newDeleteEventTool(), deleteEventHandler(deps))
}
