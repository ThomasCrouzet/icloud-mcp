package mcptools

import (
	"github.com/mark3labs/mcp-go/server"
)

// RegisterContacts registers the Contacts read tools and the independently
// gated write and delete tools. It returns the names actually registered.
func RegisterContacts(s *server.MCPServer, deps ContactsDeps, allowRead, allowWrite, allowDelete bool) []string {
	if deps.Redactor == nil {
		// An empty redactor redacts nothing useful; production wiring must pass
		// a secret-aware redactor (RegisterUnified already panics on nil).
		panic("mcptools: Contacts tools require a non-nil redactor")
	}
	var names []string
	if allowRead {
		s.AddTool(newListAddressBooksTool(), listAddressBooksHandler(deps))
		s.AddTool(newSearchContactsTool(), searchContactsHandler(deps))
		s.AddTool(newGetContactTool(), getContactHandler(deps))
		names = append(names, "list_address_books", "search_contacts", "get_contact")
	}
	if allowWrite {
		s.AddTool(newCreateContactTool(), createContactHandler(deps))
		s.AddTool(newUpdateContactTool(), updateContactHandler(deps))
		names = append(names, "create_contact", "update_contact")
	}
	if allowDelete {
		s.AddTool(newDeleteContactTool(), deleteContactHandler(deps))
		names = append(names, "delete_contact")
	}
	return names
}
