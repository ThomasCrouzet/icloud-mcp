package mcptools

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// Deps groups the dependencies shared by all tool handlers.
type Deps struct {
	Service         icloud.Service
	ContactsService contacts.Service
	MailService     maildomain.Service
	Audit           *security.AuditLogger
	Redactor        *security.Redactor

	// DefaultLocation is the timezone used to interpret start/end values
	// supplied without an explicit RFC3339 offset (ICLOUD_MCP_DEFAULT_TZ).
	// nil is treated as UTC by icloud.ParseDateTime and by
	// datetimeParamDescription.
	DefaultLocation *time.Location

	// Version is the binary version (main.version); empty is treated as "dev"
	// by both capability tools. Set once at registration; not a process-wide
	// mutable global.
	Version string

	// ReadOnly and HealthEnabled are surfaced by the local capability tools
	// (never email, secrets, hosts, shards, or paths).
	ReadOnly      bool
	HealthEnabled bool
}

// CapabilityPlan is the immutable, effective registration plan. Its private
// fields prevent registration and capability reporting from being changed
// independently after construction.
type CapabilityPlan struct {
	readOnly        bool
	contactsEnabled bool
	mailEnabled     bool
	mailMutations   bool
	mailSend        bool
}

// NewCapabilityPlan applies the global read-only and domain enable gates to
// the configured capability booleans.
func NewCapabilityPlan(readOnly, contactsEnabled, mailEnabled, mailMutations, mailSend bool) CapabilityPlan {
	return CapabilityPlan{
		readOnly:        readOnly,
		contactsEnabled: contactsEnabled,
		mailEnabled:     mailEnabled,
		mailMutations:   mailEnabled && mailMutations && !readOnly,
		mailSend:        mailEnabled && mailSend && !readOnly,
	}
}

// ReadOnly reports whether the global mutation kill switch is active.
func (p CapabilityPlan) ReadOnly() bool { return p.readOnly }

// ContactsEnabled reports whether the Contacts domain is configured.
func (p CapabilityPlan) ContactsEnabled() bool { return p.contactsEnabled }

// MailEnabled reports whether the Mail domain is configured.
func (p CapabilityPlan) MailEnabled() bool { return p.mailEnabled }

// CalendarWritesEnabled reports whether Calendar mutations are effective.
func (p CapabilityPlan) CalendarWritesEnabled() bool { return !p.readOnly }

// ContactsWritesEnabled reports whether Contacts mutations are effective.
func (p CapabilityPlan) ContactsWritesEnabled() bool {
	return p.contactsEnabled && !p.readOnly
}

// MailMutationsEnabled reports whether IMAP mutations are effective.
func (p CapabilityPlan) MailMutationsEnabled() bool { return p.mailMutations }

// MailSendEnabled reports whether SMTP submission is effective.
func (p CapabilityPlan) MailSendEnabled() bool { return p.mailSend }

// RegisteredTools returns a sorted copy of the exact registration manifest.
func (p CapabilityPlan) RegisteredTools() []string {
	names := calendarReadToolNames()
	if p.CalendarWritesEnabled() {
		names = append(names, calendarWriteToolNames()...)
	}
	if p.contactsEnabled {
		names = append(names, contactsReadToolNames()...)
		if p.ContactsWritesEnabled() {
			names = append(names, contactsWriteToolNames()...)
		}
	}
	if p.mailEnabled {
		names = append(names, mailReadToolNames()...)
		if p.mailMutations {
			names = append(names, mailMutationToolNames()...)
		}
		if p.mailSend {
			names = append(names, "send_message")
		}
	}
	names = append(names, "icloud_capabilities")
	sort.Strings(names)
	return names
}

// ToolCount returns the number of tools in the finalized manifest.
func (p CapabilityPlan) ToolCount() int { return len(p.RegisteredTools()) }

func calendarReadToolNames() []string {
	return []string{
		"list_calendars", "search_events", "get_event", "find_free_slots",
		"validate_event", "calendar_capabilities",
	}
}

func calendarWriteToolNames() []string {
	return []string{"create_event", "update_event", "delete_event"}
}

func contactsReadToolNames() []string {
	return []string{"list_address_books", "search_contacts", "get_contact"}
}

func contactsWriteToolNames() []string {
	return []string{"create_contact", "update_contact", "delete_contact"}
}

func mailReadToolNames() []string {
	return []string{"list_mailboxes", "search_messages", "get_message"}
}

func mailMutationToolNames() []string {
	return []string{"set_message_flags", "move_message", "trash_message"}
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

// Register preserves the original Calendar registration entry point. Optional
// domains remain disabled, while icloud_capabilities is always added as part of
// the unified default surface.
func Register(s *server.MCPServer, deps Deps, readOnly bool) {
	RegisterUnified(s, deps, NewCapabilityPlan(readOnly, false, false, false, false))
}

// RegisterUnified composes all enabled domain registrations from one finalized
// plan. Disabled-domain handlers are never installed.
func RegisterUnified(s *server.MCPServer, deps Deps, plan CapabilityPlan) []string {
	if s == nil {
		panic("mcptools: cannot register tools on a nil MCP server")
	}
	if isNilDependency(deps.Service) {
		panic("mcptools: Calendar tools are enabled but Calendar service is nil")
	}
	if deps.Redactor == nil {
		panic("mcptools: enabled tools require a non-nil redactor")
	}
	if plan.ContactsEnabled() && isNilDependency(deps.ContactsService) {
		panic("mcptools: Contacts tools are enabled but Contacts service is nil")
	}
	if plan.MailEnabled() && isNilDependency(deps.MailService) {
		panic("mcptools: Mail tools are enabled but Mail service is nil")
	}
	mutationsEnabled := plan.CalendarWritesEnabled() || plan.ContactsWritesEnabled() ||
		plan.MailMutationsEnabled() || plan.MailSendEnabled()
	if mutationsEnabled && deps.Audit == nil {
		panic("mcptools: enabled mutation tools require a non-nil audit logger")
	}
	deps.ReadOnly = plan.ReadOnly()
	registered := registerCalendar(s, deps, plan.CalendarWritesEnabled())
	if plan.ContactsEnabled() {
		registered = append(registered, RegisterContacts(s, ContactsDeps{
			Service: deps.ContactsService, Audit: deps.Audit, Redactor: deps.Redactor,
		}, plan.ContactsWritesEnabled())...)
	}
	if plan.MailEnabled() {
		registered = append(registered, RegisterMail(s, MailDeps{
			Service: deps.MailService, Audit: deps.Audit, Redactor: deps.Redactor,
		}, plan.MailMutationsEnabled(), plan.MailSendEnabled())...)
	}
	s.AddTool(newICloudCapabilitiesTool(), icloudCapabilitiesHandler(deps, plan))
	registered = append(registered, "icloud_capabilities")
	sort.Strings(registered)

	expected := plan.RegisteredTools()
	if !slices.Equal(registered, expected) {
		panic(fmt.Sprintf("mcptools: registration manifest mismatch: got %v, want %v", registered, expected))
	}
	actual := make([]string, 0, len(s.ListTools()))
	for name := range s.ListTools() {
		actual = append(actual, name)
	}
	sort.Strings(actual)
	if !slices.Equal(actual, expected) {
		panic(fmt.Sprintf("mcptools: server tool inventory mismatch: got %v, want %v", actual, expected))
	}
	return actual
}

func isNilDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func registerCalendar(s *server.MCPServer, deps Deps, allowWrites bool) []string {
	s.AddTool(newListCalendarsTool(), listCalendarsHandler(deps))
	s.AddTool(newSearchEventsTool(deps.DefaultLocation), searchEventsHandler(deps))
	s.AddTool(newGetEventTool(), getEventHandler(deps))
	s.AddTool(newFindFreeSlotsTool(deps.DefaultLocation), findFreeSlotsHandler(deps))
	s.AddTool(newValidateEventTool(deps.DefaultLocation), validateEventHandler(deps))
	s.AddTool(newCalendarCapabilitiesTool(), calendarCapabilitiesHandler(deps))
	names := calendarReadToolNames()
	if !allowWrites {
		return names
	}
	s.AddTool(newCreateEventTool(deps.DefaultLocation), createEventHandler(deps))
	s.AddTool(newUpdateEventTool(deps.DefaultLocation), updateEventHandler(deps))
	s.AddTool(newDeleteEventTool(deps.DefaultLocation), deleteEventHandler(deps))
	return append(names, calendarWriteToolNames()...)
}
