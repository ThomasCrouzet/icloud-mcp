package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func newICloudCapabilitiesTool() mcp.Tool {
	return mcp.NewTool("icloud_capabilities",
		mcp.WithDescription("Returns enabled local iCloud domains and tools without network access or account data."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

type icloudDomains struct {
	Calendar bool `json:"calendar"`
	Contacts bool `json:"contacts"`
	Mail     bool `json:"mail"`
}

type icloudCapabilityGroups struct {
	CalendarRead   bool `json:"calendarRead"`
	CalendarWrite  bool `json:"calendarWrite"`
	CalendarDelete bool `json:"calendarDelete"`
	ContactsRead   bool `json:"contactsRead"`
	ContactsWrite  bool `json:"contactsWrite"`
	ContactsDelete bool `json:"contactsDelete"`
	MailRead       bool `json:"mailRead"`
	MailMutation   bool `json:"mailMutation"`
	MailTrash      bool `json:"mailTrash"`
	MailSend       bool `json:"mailSend"`
}

type icloudCapabilitiesResponse struct {
	Version           string                 `json:"version"`
	ReadOnly          bool                   `json:"readOnly"`
	HealthcheckActive bool                   `json:"healthcheckActive"`
	Domains           icloudDomains          `json:"domains"`
	CapabilityGroups  icloudCapabilityGroups `json:"capabilityGroups"`
	Tools             []string               `json:"tools"`
	ToolCount         int                    `json:"toolCount"`
}

func icloudCapabilitiesHandler(deps Deps, plan CapabilityPlan) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx
		_ = req
		version := deps.Version
		if version == "" {
			version = "dev"
		}
		tools := plan.RegisteredTools()
		return writeJSON(deps.Redactor, icloudCapabilitiesResponse{
			Version:           version,
			ReadOnly:          plan.ReadOnly(),
			HealthcheckActive: deps.HealthEnabled,
			Domains: icloudDomains{
				Calendar: plan.CalendarReadEnabled() || plan.CalendarWritesEnabled(),
				Contacts: plan.ContactsEnabled(),
				Mail:     plan.MailEnabled(),
			},
			CapabilityGroups: icloudCapabilityGroups{
				CalendarRead:   plan.CalendarReadEnabled(),
				CalendarWrite:  plan.CalendarCreateUpdateEnabled(),
				CalendarDelete: plan.CalendarDeleteEnabled(),
				ContactsRead:   plan.ContactsReadEnabled(),
				ContactsWrite:  plan.ContactsCreateUpdateEnabled(),
				ContactsDelete: plan.ContactsDeleteEnabled(),
				MailRead:       plan.MailReadEnabled(),
				MailMutation:   plan.MailMutationsEnabled(),
				MailTrash:      plan.MailTrashEnabled(),
				MailSend:       plan.MailSendEnabled(),
			},
			Tools:     tools,
			ToolCount: len(tools),
		}), nil
	}
}
