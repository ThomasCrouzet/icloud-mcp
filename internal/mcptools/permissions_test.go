package mcptools

import (
	"context"
	"slices"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func mixedProfileServer(t *testing.T, readOnly bool, set config.CapabilitySet) (*server.MCPServer, *client.Client) {
	t.Helper()
	plan := NewCapabilityPlanFromConfig(readOnly, set)
	s := server.NewMCPServer("mixed-profile", "test", server.WithToolCapabilities(false))
	RegisterUnified(s, Deps{
		Service:         &icloud.MockService{},
		ContactsService: &fakeContactsService{},
		MailService:     &mailToolsFakeService{},
		Audit:           security.NewAuditLogger(&discardWriter{}),
		Redactor:        security.NewRedactor("unused-secret"),
	}, plan)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "mixed-profile", Version: "0.0.0"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return s, c
}

func listedToolNames(t *testing.T, c *client.Client) []string {
	t.Helper()
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		names[i] = tool.Name
	}
	slices.Sort(names)
	return names
}

func assertToolAbsent(t *testing.T, c *client.Client, name string) {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = map[string]any{}
	res, err := c.CallTool(context.Background(), request)
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("disabled tool %q was callable: result=%#v", name, res)
	}
}

func TestMixedPermissionProfiles(t *testing.T) {
	t.Run("calendar read plus mail send", func(t *testing.T) {
		_, c := mixedProfileServer(t, false, config.CapabilitySet{CalendarRead: true, MailRead: true, MailSend: true})
		names := listedToolNames(t, c)
		for _, want := range []string{"list_calendars", "search_events", "get_message", "send_message", "icloud_capabilities"} {
			if !slices.Contains(names, want) {
				t.Errorf("missing %q in %v", want, names)
			}
		}
		for _, denied := range []string{"create_event", "update_event", "delete_event", "create_contact", "set_message_flags", "trash_message"} {
			if slices.Contains(names, denied) {
				t.Errorf("disabled tool %q appeared in %v", denied, names)
			}
			assertToolAbsent(t, c, denied)
		}
	})

	t.Run("contacts read without writes", func(t *testing.T) {
		_, c := mixedProfileServer(t, false, config.CapabilitySet{CalendarRead: true, ContactsRead: true})
		names := listedToolNames(t, c)
		for _, want := range []string{"list_address_books", "search_contacts", "get_contact"} {
			if !slices.Contains(names, want) {
				t.Errorf("missing %q in %v", want, names)
			}
		}
		for _, denied := range []string{"create_contact", "update_contact", "delete_contact"} {
			if slices.Contains(names, denied) {
				t.Errorf("disabled tool %q appeared", denied)
			}
			assertToolAbsent(t, c, denied)
		}
	})

	t.Run("create update without delete", func(t *testing.T) {
		_, c := mixedProfileServer(t, false, config.CapabilitySet{CalendarRead: true, CalendarWrite: true, ContactsRead: true, ContactsWrite: true})
		names := listedToolNames(t, c)
		for _, want := range []string{"create_event", "update_event", "create_contact", "update_contact"} {
			if !slices.Contains(names, want) {
				t.Errorf("missing %q in %v", want, names)
			}
		}
		for _, denied := range []string{"delete_event", "delete_contact"} {
			if slices.Contains(names, denied) {
				t.Errorf("disabled tool %q appeared", denied)
			}
			assertToolAbsent(t, c, denied)
		}
	})

	t.Run("global read-only overrides explicit writes and send", func(t *testing.T) {
		_, c := mixedProfileServer(t, true, config.CapabilitySet{
			CalendarRead: true, CalendarWrite: true, CalendarDelete: true,
			ContactsRead: true, ContactsWrite: true, ContactsDelete: true,
			MailRead: true, MailMutate: true, MailTrash: true, MailSend: true,
		})
		names := listedToolNames(t, c)
		for _, denied := range []string{
			"create_event", "update_event", "delete_event",
			"create_contact", "update_contact", "delete_contact",
			"set_message_flags", "move_message", "trash_message", "send_message",
		} {
			if slices.Contains(names, denied) {
				t.Errorf("mutation %q survived global read-only", denied)
			}
			assertToolAbsent(t, c, denied)
		}
	})
}
