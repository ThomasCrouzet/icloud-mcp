package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type countingFailDoer struct{ calls atomic.Int32 }

func (d *countingFailDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, errors.New("calendar endpoint unavailable")
}

func TestRunServerStartsWhenCalendarEndpointIsUnavailable(t *testing.T) {
	permissions, err := config.ParsePermissions("calendar.read,mail.read")
	if err != nil {
		t.Fatal(err)
	}
	inertDial := func(context.Context) (net.Conn, error) {
		return nil, errors.New("test dialer must not run")
	}
	mail, err := maildomain.NewService(maildomain.Config{
		Address: "mailbox@example.com", Password: "mail-app-password",
	}, inertDial, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Email: "calendar@example.com", Password: "calendar-app-password",
		EnableMail: true, MailAddress: "mailbox@example.com", MailPassword: "mail-app-password",
		Permissions: permissions, Timeout: time.Second, DefaultLocation: time.UTC,
	}
	doer := &countingFailDoer{}
	calendar := icloud.NewClient(doer, security.ICloudBaseURL, security.IsICloudHost)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"lazy-startup","version":"test"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := runServer(cfg, security.AuditFormatJSON, "", calendar, nil, mail, strings.NewReader(input), &output); err != nil {
		t.Fatalf("runServer with unavailable Calendar: %v", err)
	}
	if calls := doer.calls.Load(); calls != 0 {
		t.Fatalf("boot made %d Calendar requests, want lazy zero", calls)
	}
	body := output.String()
	if !strings.Contains(body, "list_calendars") || !strings.Contains(body, "list_mailboxes") {
		t.Fatalf("tools/list output is missing an independent domain:\n%s", body)
	}
	if strings.Contains(body, "send_message") {
		t.Fatalf("disabled Mail send appeared in tools/list:\n%s", body)
	}
}

func TestRunServerSupportsMailOnlyWithoutCalendarClient(t *testing.T) {
	permissions, err := config.ParsePermissions("mail.read")
	if err != nil {
		t.Fatal(err)
	}
	mail, err := maildomain.NewService(maildomain.Config{
		Address: "mailbox@example.com", Password: "mail-app-password",
	}, func(context.Context) (net.Conn, error) {
		return nil, errors.New("test dialer must not run")
	}, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		EnableMail: true, MailAddress: "mailbox@example.com", MailPassword: "mail-app-password",
		Permissions: permissions, Timeout: time.Second, DefaultLocation: time.UTC,
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"mail-only","version":"test"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := runServer(cfg, security.AuditFormatJSON, "", nil, nil, mail, strings.NewReader(input), &output); err != nil {
		t.Fatalf("Mail-only runServer: %v", err)
	}
	body := output.String()
	if !strings.Contains(body, "list_mailboxes") || !strings.Contains(body, "get_message") {
		t.Fatalf("Mail-only tools/list is incomplete:\n%s", body)
	}
	if strings.Contains(body, "list_calendars") || strings.Contains(body, "search_events") {
		t.Fatalf("Calendar tools appeared without a Calendar client:\n%s", body)
	}
}

func TestRunServerSupportsContactsOnlyWithoutCalendarClient(t *testing.T) {
	permissions, err := config.ParsePermissions("contacts.read")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Email: "contacts@example.com", Password: "contacts-app-password",
		EnableContacts: true, Permissions: permissions, Timeout: time.Second, DefaultLocation: time.UTC,
	}
	doer := &countingFailDoer{}
	addressBooks := contacts.NewClient(doer, security.ContactsBaseURL, security.IsContactsHost)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"contacts-only","version":"test"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := runServer(cfg, security.AuditFormatJSON, "", nil, addressBooks, nil, strings.NewReader(input), &output); err != nil {
		t.Fatalf("Contacts-only runServer: %v", err)
	}
	if calls := doer.calls.Load(); calls != 0 {
		t.Fatalf("boot made %d Contacts requests, want lazy zero", calls)
	}
	body := output.String()
	if !strings.Contains(body, "list_address_books") || !strings.Contains(body, "search_contacts") {
		t.Fatalf("Contacts-only tools/list is incomplete:\n%s", body)
	}
	if strings.Contains(body, "list_calendars") || strings.Contains(body, "create_contact") {
		t.Fatalf("unexpected Calendar or write tool:\n%s", body)
	}
}
