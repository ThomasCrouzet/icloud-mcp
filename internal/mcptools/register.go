package mcptools

import (
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// ServerVersion is set by main via RegisterOptions when available; used by
// calendar_capabilities. Empty means "dev".
var ServerVersion = "dev"

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

	// ReadOnly and HealthEnabled are surfaced by calendar_capabilities only
	// (never email, secrets, shard, or paths).
	ReadOnly      bool
	HealthEnabled bool
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

// Register registers the MCP tools on the server. In readOnly mode, write
// tools (create_event/update_event/delete_event) are NOT registered: they
// are therefore absent from tools/list, not merely rejected at execution
// time. Read-only tools (including get_event, validate_event,
// calendar_capabilities, find_free_slots) remain available.
func Register(s *server.MCPServer, deps Deps, readOnly bool) {
	deps.ReadOnly = readOnly
	s.AddTool(newListCalendarsTool(), listCalendarsHandler(deps))
	s.AddTool(newSearchEventsTool(deps.DefaultLocation), searchEventsHandler(deps))
	s.AddTool(newGetEventTool(), getEventHandler(deps))
	s.AddTool(newFindFreeSlotsTool(deps.DefaultLocation), findFreeSlotsHandler(deps))
	s.AddTool(newValidateEventTool(deps.DefaultLocation), validateEventHandler(deps))
	s.AddTool(newCalendarCapabilitiesTool(), calendarCapabilitiesHandler(deps))
	if readOnly {
		return
	}
	s.AddTool(newCreateEventTool(deps.DefaultLocation), createEventHandler(deps))
	s.AddTool(newUpdateEventTool(deps.DefaultLocation), updateEventHandler(deps))
	s.AddTool(newDeleteEventTool(deps.DefaultLocation), deleteEventHandler(deps))
}
