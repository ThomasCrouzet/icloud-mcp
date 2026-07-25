package mcptools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type noCallContactsService struct{ contacts.Service }
type noCallMailService struct{ maildomain.Service }

func TestICloudCapabilitiesMatchesPlanAndPerformsNoNetwork(t *testing.T) {
	calendarService := &icloud.MockService{}
	plan := NewCapabilityPlan(false, true, true, true, true)
	deps := Deps{
		Service:         calendarService,
		ContactsService: noCallContactsService{},
		MailService:     noCallMailService{},
		Redactor:        security.NewRedactor("capability-secret-sentinel"),
		Version:         "1.2.3",
		HealthEnabled:   true,
	}

	result, err := icloudCapabilitiesHandler(deps, plan)(context.Background(), mcp.CallToolRequest{})
	if err != nil || result.IsError {
		t.Fatalf("icloud_capabilities returned err=%v result=%#v", err, result)
	}
	var response icloudCapabilitiesResponse
	if err := json.Unmarshal([]byte(resultText(t, result)), &response); err != nil {
		t.Fatalf("invalid capability JSON: %v", err)
	}
	if response.Version != "1.2.3" || response.ReadOnly || !response.HealthcheckActive {
		t.Errorf("top-level capability metadata = %#v", response)
	}
	if response.Domains != (icloudDomains{Calendar: true, Contacts: true, Mail: true}) {
		t.Errorf("domains = %#v", response.Domains)
	}
	wantGroups := icloudCapabilityGroups{
		CalendarRead: true, CalendarWrite: true,
		ContactsRead: true, ContactsWrite: true,
		MailRead: true, MailMutation: true, MailSend: true,
	}
	if response.CapabilityGroups != wantGroups {
		t.Errorf("capability groups = %#v, want %#v", response.CapabilityGroups, wantGroups)
	}
	if response.ToolCount != plan.ToolCount() || !slices.Equal(response.Tools, plan.RegisteredTools()) || !slices.IsSorted(response.Tools) {
		t.Errorf("reported tools are inconsistent: count=%d tools=%v plan=%v",
			response.ToolCount, response.Tools, plan.RegisteredTools())
	}
	if calendarService.ListCallCount+calendarService.SearchCallCount+calendarService.GetCallCount+
		calendarService.CreateCallCount+calendarService.UpdateCallCount+calendarService.DeleteCallCount != 0 {
		t.Fatal("icloud_capabilities called the Calendar service")
	}

	text := strings.ToLower(resultText(t, result))
	for _, forbidden := range []string{"password", "file://", "caldav.icloud", "contacts.icloud", "imap.mail", "smtp.mail", "recipient"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("capability result contains forbidden data marker %q: %s", forbidden, text)
		}
	}
}

func TestICloudCapabilitiesReadOnlyEffectiveGroups(t *testing.T) {
	plan := NewCapabilityPlan(true, true, true, true, true)
	result, err := icloudCapabilitiesHandler(Deps{
		Redactor: security.NewRedactor("unused-secret"),
	}, plan)(context.Background(), mcp.CallToolRequest{})
	if err != nil || result.IsError {
		t.Fatalf("icloud_capabilities returned err=%v result=%#v", err, result)
	}
	var response icloudCapabilitiesResponse
	if err := json.Unmarshal([]byte(resultText(t, result)), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ReadOnly || response.CapabilityGroups.CalendarWrite || response.CapabilityGroups.ContactsWrite ||
		response.CapabilityGroups.MailMutation || response.CapabilityGroups.MailSend {
		t.Errorf("read-only mutations are not suppressed: %#v", response.CapabilityGroups)
	}
	if !response.CapabilityGroups.CalendarRead || !response.CapabilityGroups.ContactsRead || !response.CapabilityGroups.MailRead {
		t.Errorf("read-only domain reads are not reported: %#v", response.CapabilityGroups)
	}
	if response.ToolCount != 13 {
		t.Fatalf("all-domain read-only count = %d, want 13", response.ToolCount)
	}
}

func TestICloudCapabilitiesVersionDefaultsToDev(t *testing.T) {
	result, err := icloudCapabilitiesHandler(Deps{
		Redactor: security.NewRedactor("unused-secret"),
	}, NewCapabilityPlan(false, false, false, false, false))(context.Background(), mcp.CallToolRequest{})
	if err != nil || result.IsError {
		t.Fatalf("icloud_capabilities returned err=%v result=%#v", err, result)
	}
	var response icloudCapabilitiesResponse
	if err := json.Unmarshal([]byte(resultText(t, result)), &response); err != nil {
		t.Fatal(err)
	}
	if response.Version != "dev" {
		t.Fatalf("version = %q, want dev", response.Version)
	}
}
