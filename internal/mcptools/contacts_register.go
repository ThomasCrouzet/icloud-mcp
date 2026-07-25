package mcptools

import (
	"github.com/mark3labs/mcp-go/server"
)

// RegisterContacts registers the Contacts read tools and, when allowWrites is
// true, the Contacts mutation tools. It returns the names actually registered.
func RegisterContacts(s *server.MCPServer, deps ContactsDeps, allowWrites bool) []string {
	if deps.Redactor == nil {
		// An empty redactor redacts nothing useful; production wiring must pass
		// a secret-aware redactor (RegisterUnified already panics on nil).
		panic("mcptools: Contacts tools require a non-nil redactor")
	}
	s.AddTool(newListAddressBooksTool(), listAddressBooksHandler(deps))
	s.AddTool(newSearchContactsTool(), searchContactsHandler(deps))
	s.AddTool(newGetContactTool(), getContactHandler(deps))
	names := []string{"list_address_books", "search_contacts", "get_contact"}
	if !allowWrites {
		return names
	}
	s.AddTool(newCreateContactTool(), createContactHandler(deps))
	s.AddTool(newUpdateContactTool(), updateContactHandler(deps))
	s.AddTool(newDeleteContactTool(), deleteContactHandler(deps))
	return append(names, "create_contact", "update_contact", "delete_contact")
}
