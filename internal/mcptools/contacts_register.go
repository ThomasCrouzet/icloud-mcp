package mcptools

import (
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// RegisterContacts registers the Contacts read tools and, when allowWrites is
// true, the Contacts mutation tools. It returns the names actually registered.
func RegisterContacts(s *server.MCPServer, deps ContactsDeps, allowWrites bool) []string {
	if deps.Redactor == nil {
		deps.Redactor = security.NewRedactor()
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
