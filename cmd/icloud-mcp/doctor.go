package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

const doctorProbeTimeout = 10 * time.Second

type doctorDomain struct {
	Domain         string   `json:"domain"`
	Status         string   `json:"status"`
	CheckedAt      string   `json:"checkedAt,omitempty"`
	ErrorCode      string   `json:"errorCode,omitempty"`
	Mailboxes      int      `json:"mailboxes,omitempty"`
	CondStore      bool     `json:"condStore,omitempty"`
	Move           bool     `json:"move,omitempty"`
	UIDPlus        bool     `json:"uidPlus,omitempty"`
	SpecialUse     bool     `json:"specialUse,omitempty"`
	AddressBooks   int      `json:"addressBooks,omitempty"`
	SupportedVCard []string `json:"supportedVCard,omitempty"`
}

type doctorReport struct {
	Version      string          `json:"version"`
	ReadOnly     bool            `json:"readOnly"`
	Capabilities map[string]bool `json:"capabilities"`
	Domains      []doctorDomain  `json:"domains"`
}

func runDoctor(cfg *config.Config, format string, stdout io.Writer) int {
	if format != "text" && format != "json" {
		fmt.Fprintln(stdout, "doctor: format must be text or json")
		return 2
	}
	report := doctorReport{
		Version: version, ReadOnly: cfg.ReadOnly,
		Capabilities: configuredCapabilities(cfg),
	}
	status := 0
	for _, name := range []string{"calendar", "contacts", "mail"} {
		domain := doctorProbe(cfg, name)
		if domain.Status != "disabled" && domain.Status != "ready" {
			status = 1
		}
		report.Domains = append(report.Domains, domain)
	}
	if format == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return 2
		}
	} else {
		fmt.Fprintf(stdout, "version: %s\nread_only: %t\n", report.Version, report.ReadOnly)
		for _, domain := range report.Domains {
			line := fmt.Sprintf("%s: %s", domain.Domain, domain.Status)
			if domain.ErrorCode != "" {
				line += " (" + domain.ErrorCode + ")"
			}
			if domain.Mailboxes > 0 {
				line += fmt.Sprintf(" mailboxes=%d condstore=%t move=%t uidplus=%t special_use=%t", domain.Mailboxes, domain.CondStore, domain.Move, domain.UIDPlus, domain.SpecialUse)
			}
			if domain.AddressBooks > 0 {
				line += fmt.Sprintf(" address_books=%d", domain.AddressBooks)
			}
			fmt.Fprintln(stdout, line)
		}
	}
	return status
}

func configuredCapabilities(cfg *config.Config) map[string]bool {
	set := cfg.Capabilities()
	return map[string]bool{
		"calendarRead": set.CalendarRead, "calendarWrite": set.CalendarWrite, "calendarDelete": set.CalendarDelete,
		"contactsRead": set.ContactsRead, "contactsWrite": set.ContactsWrite, "contactsDelete": set.ContactsDelete,
		"mailRead": set.MailRead, "mailMutate": set.MailMutate, "mailTrash": set.MailTrash, "mailSend": set.MailSend,
	}
}

func doctorProbe(cfg *config.Config, name string) doctorDomain {
	set := cfg.Capabilities()
	enabled := false
	switch name {
	case "calendar":
		enabled = set.CalendarRead || set.CalendarWrite || set.CalendarDelete
	case "contacts":
		enabled = set.ContactsRead || set.ContactsWrite || set.ContactsDelete
	case "mail":
		enabled = set.MailRead || set.MailMutate || set.MailTrash || set.MailSend
	}
	if !enabled {
		return doctorDomain{Domain: name, Status: "disabled"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
	defer cancel()
	domain := doctorDomain{Domain: name, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	var err error
	switch name {
	case "calendar":
		_, err = newCalendarClient(cfg).ListCalendars(ctx)
	case "contacts":
		contactsService, _, serviceErr := newOptionalServices(cfg)
		if serviceErr != nil {
			err = serviceErr
		} else if contactsService == nil {
			err = fmt.Errorf("contacts client is missing")
		} else {
			var books []contacts.AddressBook
			books, err = contactsService.ListAddressBooks(ctx)
			domain.AddressBooks = len(books)
			for _, book := range books {
				domain.SupportedVCard = append(domain.SupportedVCard, book.SupportedVersions...)
			}
		}
	case "mail":
		_, mailService, serviceErr := newOptionalServices(cfg)
		if serviceErr != nil {
			err = serviceErr
		} else if mailService == nil {
			err = fmt.Errorf("mail client is missing")
		} else if probe, ok := mailService.(interface {
			Probe(context.Context) (maildomain.ProbeResult, error)
		}); ok {
			var result maildomain.ProbeResult
			result, err = probe.Probe(ctx)
			domain.Mailboxes = result.Mailboxes
			domain.CondStore = result.CondStore
			domain.Move = result.Move
			domain.UIDPlus = result.UIDPlus
			domain.SpecialUse = result.SpecialUse
		} else {
			err = fmt.Errorf("mail probe is unavailable")
		}
	}
	if err == nil {
		domain.Status = "ready"
		return domain
	}
	domain.Status = "unavailable"
	if icloudError := icloud.AsICloudError(err); icloudError != nil {
		if icloudError.Code == icloud.CodeAuthentication || icloudError.Code == icloud.CodeAuthenticationRefused || icloudError.Code == icloud.CodeAuthorization {
			domain.Status = "authentication_failed"
		}
		domain.ErrorCode = string(icloudError.Code)
	}
	if contactsError := contacts.AsError(err); contactsError != nil {
		if contactsError.Code == contacts.CodeAuthentication || contactsError.Code == contacts.CodeAuthorization {
			domain.Status = "authentication_failed"
		}
		domain.ErrorCode = string(contactsError.Code)
	}
	if mailError := maildomain.AsError(err); mailError != nil {
		if mailError.Code == maildomain.CodeAuthentication || mailError.Code == maildomain.CodeAuthorization {
			domain.Status = "authentication_failed"
		}
		domain.ErrorCode = string(mailError.Code)
	}
	if domain.ErrorCode == "" {
		domain.ErrorCode = "probe_failed"
	}
	return domain
}
