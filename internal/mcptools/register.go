package mcptools

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
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
	ReadOnly       bool
	HealthEnabled  bool
	ResourceScopes config.ResourceScopes

	// CalendarWriteExplicit and CalendarDeleteExplicit report an operator
	// capability grant from ICLOUD_MCP_PERMISSIONS. They gate the
	// group-scheduled approval path; a legacy switch is not sufficient.
	CalendarWriteExplicit    bool
	CalendarDeleteExplicit   bool
	CalendarScheduleExplicit bool

	// Idempotency scopes update claims and results to one registration. It is
	// initialized by RegisterUnified when nil.
	Idempotency *idempotencyStore
}

// CapabilityPlan is the immutable, effective registration plan. Its private
// fields prevent registration and capability reporting from being changed
// independently after construction.
type CapabilityPlan struct {
	readOnly bool

	calendarRead   bool
	calendarWrite  bool
	calendarDelete bool

	contactsRead   bool
	contactsWrite  bool
	contactsDelete bool

	mailRead   bool
	mailMutate bool
	mailTrash  bool
	mailSend   bool
	mailReply  bool
}

// NewCapabilityPlan applies the global read-only and domain enable gates to
// the legacy configured booleans.
func NewCapabilityPlan(readOnly, contactsEnabled, mailEnabled, mailMutations, mailSend bool) CapabilityPlan {
	return CapabilityPlan{
		readOnly:       readOnly,
		calendarRead:   true,
		calendarWrite:  !readOnly,
		calendarDelete: !readOnly,
		contactsRead:   contactsEnabled,
		contactsWrite:  contactsEnabled && !readOnly,
		contactsDelete: contactsEnabled && !readOnly,
		mailRead:       mailEnabled,
		mailMutate:     mailEnabled && mailMutations && !readOnly,
		mailTrash:      mailEnabled && mailMutations && !readOnly,
		mailSend:       mailEnabled && mailSend && !readOnly,
		mailReply:      mailEnabled && mailSend && !readOnly,
	}
}

// NewCapabilityPlanFromConfig builds the plan from an effective capability
// set. The set already includes the global read-only switch.
func NewCapabilityPlanFromConfig(readOnly bool, set config.CapabilitySet) CapabilityPlan {
	if readOnly {
		set.CalendarWrite, set.CalendarDelete = false, false
		set.ContactsWrite, set.ContactsDelete = false, false
		set.MailMutate, set.MailTrash, set.MailSend = false, false, false
	}
	return CapabilityPlan{
		readOnly:       readOnly,
		calendarRead:   set.CalendarRead,
		calendarWrite:  set.CalendarWrite,
		calendarDelete: set.CalendarDelete,
		contactsRead:   set.ContactsRead,
		contactsWrite:  set.ContactsWrite,
		contactsDelete: set.ContactsDelete,
		mailRead:       set.MailRead,
		mailMutate:     set.MailMutate,
		mailTrash:      set.MailTrash,
		mailSend:       set.MailSend,
		mailReply:      set.MailRead && set.MailSend,
	}
}

// ReadOnly reports whether the global mutation kill switch is active.
func (p CapabilityPlan) ReadOnly() bool { return p.readOnly }

// CalendarReadEnabled reports whether Calendar read tools are effective.
func (p CapabilityPlan) CalendarReadEnabled() bool { return p.calendarRead }

// CalendarCreateUpdateEnabled reports whether Calendar create and update tools
// are effective.
func (p CapabilityPlan) CalendarCreateUpdateEnabled() bool { return p.calendarWrite }

// CalendarDeleteEnabled reports whether the Calendar delete tool is effective.
func (p CapabilityPlan) CalendarDeleteEnabled() bool { return p.calendarDelete }

// CalendarWritesEnabled reports whether any Calendar mutation is effective.
func (p CapabilityPlan) CalendarWritesEnabled() bool {
	return p.calendarWrite || p.calendarDelete
}

// ContactsReadEnabled reports whether Contacts read tools are effective.
func (p CapabilityPlan) ContactsReadEnabled() bool { return p.contactsRead }

// ContactsEnabled reports whether the Contacts domain is configured for reads
// or mutations.
func (p CapabilityPlan) ContactsEnabled() bool {
	return p.contactsRead || p.contactsWrite || p.contactsDelete
}

// ContactsCreateUpdateEnabled reports whether Contacts create and update tools
// are effective.
func (p CapabilityPlan) ContactsCreateUpdateEnabled() bool { return p.contactsWrite }

// ContactsDeleteEnabled reports whether the Contacts delete tool is effective.
func (p CapabilityPlan) ContactsDeleteEnabled() bool { return p.contactsDelete }

// ContactsWritesEnabled reports whether any Contacts mutation is effective.
func (p CapabilityPlan) ContactsWritesEnabled() bool {
	return p.contactsWrite || p.contactsDelete
}

// MailReadEnabled reports whether Mail read tools are effective.
func (p CapabilityPlan) MailReadEnabled() bool { return p.mailRead }

// MailEnabled reports whether the Mail domain is configured for reads or
// mutations.
func (p CapabilityPlan) MailEnabled() bool {
	return p.mailRead || p.mailMutate || p.mailTrash || p.mailSend
}

// MailMutationsEnabled reports whether flag and move mutations are effective.
func (p CapabilityPlan) MailMutationsEnabled() bool { return p.mailMutate }

// MailTrashEnabled reports whether trash mutations are effective.
func (p CapabilityPlan) MailTrashEnabled() bool { return p.mailTrash }

// MailSendEnabled reports whether SMTP submission is effective.
func (p CapabilityPlan) MailSendEnabled() bool { return p.mailSend }

// MailReplyEnabled reports whether reply_message is effective. It requires
// both Mail read and Mail send.
func (p CapabilityPlan) MailReplyEnabled() bool { return p.mailReply }

// RegisteredTools returns a sorted copy of the exact registration manifest.
func (p CapabilityPlan) RegisteredTools() []string {
	var names []string
	if p.calendarRead {
		names = append(names, calendarReadToolNames()...)
	}
	if p.calendarWrite {
		names = append(names, "create_event", "update_event")
	}
	if p.calendarDelete {
		names = append(names, "delete_event")
	}
	if p.contactsRead {
		names = append(names, contactsReadToolNames()...)
	}
	if p.contactsWrite {
		names = append(names, "create_contact", "update_contact")
	}
	if p.contactsDelete {
		names = append(names, "delete_contact")
	}
	if p.mailRead {
		names = append(names, mailReadToolNames()...)
	}
	if p.mailMutate {
		names = append(names, "set_message_flags", "move_message")
	}
	if p.mailTrash {
		names = append(names, "trash_message")
	}
	if p.mailSend {
		names = append(names, "send_message")
	}
	if p.mailReply {
		names = append(names, "reply_message")
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

func mailReadToolNames() []string {
	return []string{"list_mailboxes", "search_messages", "get_message"}
}

// defaultLocationName returns the display name for tool descriptions and error
// messages. It returns loc.String() when loc is set and "UTC" otherwise. This
// matches the nil handling in icloud.ParseDateTime and keeps the schema
// consistent with parsing behavior.
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
	if isNilDependency(deps.Service) && (plan.CalendarReadEnabled() || plan.CalendarWritesEnabled()) {
		panic("mcptools: Calendar tools are enabled but Calendar service is nil")
	}
	if deps.Redactor == nil {
		panic("mcptools: enabled tools require a non-nil redactor")
	}
	if deps.Idempotency == nil {
		deps.Idempotency = newIdempotencyStore()
		// Preserve the process clock hook used by deterministic tests.
		if defaultIdempotency != nil {
			deps.Idempotency.now = defaultIdempotency.now
		}
	}
	if plan.ContactsEnabled() && isNilDependency(deps.ContactsService) {
		panic("mcptools: Contacts tools are enabled but Contacts service is nil")
	}
	if plan.MailEnabled() && isNilDependency(deps.MailService) {
		panic("mcptools: Mail tools are enabled but Mail service is nil")
	}
	mutationsEnabled := plan.CalendarWritesEnabled() || plan.ContactsWritesEnabled() ||
		plan.MailMutationsEnabled() || plan.MailTrashEnabled() || plan.MailSendEnabled()
	if mutationsEnabled && deps.Audit == nil {
		panic("mcptools: enabled mutation tools require a non-nil audit logger")
	}
	deps.ReadOnly = plan.ReadOnly()
	var registered []string
	if plan.CalendarReadEnabled() || plan.CalendarWritesEnabled() {
		registered = registerCalendar(s, deps, plan.CalendarReadEnabled(), plan.CalendarCreateUpdateEnabled(), plan.CalendarDeleteEnabled())
	}
	if plan.ContactsEnabled() {
		registered = append(registered, RegisterContacts(s, ContactsDeps{
			Service: deps.ContactsService, Audit: deps.Audit, Redactor: deps.Redactor, ResourceScopes: deps.ResourceScopes,
			Idempotency: deps.Idempotency,
		}, plan.ContactsReadEnabled(), plan.ContactsCreateUpdateEnabled(), plan.ContactsDeleteEnabled())...)
	}
	if plan.MailEnabled() {
		registered = append(registered, RegisterMail(s, MailDeps{
			Service: deps.MailService, Audit: deps.Audit, Redactor: deps.Redactor, ResourceScopes: deps.ResourceScopes,
		}, plan.MailReadEnabled(), plan.MailMutationsEnabled(), plan.MailTrashEnabled(), plan.MailSendEnabled(), plan.MailReplyEnabled())...)
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

func registerCalendar(s *server.MCPServer, deps Deps, allowRead, allowWrite, allowDelete bool) []string {
	var names []string
	if allowRead {
		s.AddTool(newListCalendarsTool(), listCalendarsHandler(deps))
		s.AddTool(newSearchEventsTool(deps.DefaultLocation), searchEventsHandler(deps))
		s.AddTool(newGetEventTool(), getEventHandler(deps))
		s.AddTool(newFindFreeSlotsTool(deps.DefaultLocation), findFreeSlotsHandler(deps))
		s.AddTool(newValidateEventTool(deps.DefaultLocation), validateEventHandler(deps))
		s.AddTool(newCalendarCapabilitiesTool(), calendarCapabilitiesHandler(deps))
		names = append(names, calendarReadToolNames()...)
	}
	if allowWrite {
		s.AddTool(newCreateEventTool(deps.DefaultLocation), createEventHandler(deps))
		s.AddTool(newUpdateEventTool(deps.DefaultLocation), updateEventHandler(deps))
		names = append(names, "create_event", "update_event")
	}
	if allowDelete {
		s.AddTool(newDeleteEventTool(deps.DefaultLocation), deleteEventHandler(deps))
		names = append(names, "delete_event")
	}
	return names
}
