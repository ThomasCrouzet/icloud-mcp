package mcptools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func scopeContains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func authorizeCalendarRead(scopes config.ResourceScopes, path string) error {
	return authorizeCalendar(scopes, config.ResourceCalendarRead, path)
}

func authorizeCalendarWrite(scopes config.ResourceScopes, path string) error {
	return authorizeCalendar(scopes, config.ResourceCalendarWrite, path)
}

func authorizeCalendarDelete(scopes config.ResourceScopes, path string) error {
	return authorizeCalendar(scopes, config.ResourceCalendarDelete, path)
}

func filterCalendarReads(calendars []icloud.Calendar, scopes config.ResourceScopes) []icloud.Calendar {
	return filterCalendars(calendars, scopes, config.ResourceCalendarRead)
}

func calendarReadScope(scopes config.ResourceScopes) ([]string, bool) {
	return scopes.Calendar(config.ResourceCalendarRead)
}

func authorizeCalendar(scopes config.ResourceScopes, action, path string) error {
	values, restricted := scopes.Calendar(action)
	if !restricted || scopeContains(values, path) {
		return nil
	}
	return icloud.NewError(icloud.CodeAuthorization, 0, "calendar is outside the configured resource scope", nil)
}

func filterCalendars(calendars []icloud.Calendar, scopes config.ResourceScopes, action string) []icloud.Calendar {
	values, restricted := scopes.Calendar(action)
	if !restricted {
		return calendars
	}
	out := make([]icloud.Calendar, 0, len(calendars))
	for _, calendar := range calendars {
		if scopeContains(values, calendar.Path) {
			out = append(out, calendar)
		}
	}
	return out
}

func contactScopeAllows(ctx context.Context, scopes config.ResourceScopes, service contacts.Service, action, addressBook string) (bool, error) {
	allowed, restricted, err := allowedContactBooks(ctx, scopes, service, action)
	if err != nil {
		return false, err
	}
	if !restricted {
		return true, nil
	}
	return allowed[addressBook], nil
}

func authorizeContactBook(ctx context.Context, scopes config.ResourceScopes, service contacts.Service, action, addressBook string) error {
	allowed, err := contactScopeAllows(ctx, scopes, service, action, addressBook)
	if err != nil {
		return err
	}
	if !allowed {
		return &contacts.Error{Code: contacts.CodeAuthorization, Message: "address book is outside the configured resource scope"}
	}
	return nil
}

func allowedContactBooks(ctx context.Context, scopes config.ResourceScopes, service contacts.Service, action string) (map[string]bool, bool, error) {
	values, restricted := scopes.Contacts(action)
	if !restricted {
		return nil, false, nil
	}
	books, err := service.ListAddressBooks(ctx)
	if err != nil {
		return nil, true, err
	}
	allowed := make(map[string]bool)
	for _, value := range values {
		matched := false
		for _, book := range books {
			if book.Identifier == value {
				allowed[book.Identifier] = true
				matched = true
			}
		}
		if matched {
			continue
		}
		var byName []string
		for _, book := range books {
			if book.Name == value {
				byName = append(byName, book.Identifier)
			}
		}
		switch len(byName) {
		case 0:
			return nil, true, fmt.Errorf("configured address book scope does not match a discovered address book")
		case 1:
			allowed[byName[0]] = true
		default:
			return nil, true, fmt.Errorf("configured address book scope is ambiguous")
		}
	}
	return allowed, true, nil
}

func filterAddressBooks(ctx context.Context, books []contacts.AddressBook, scopes config.ResourceScopes, service contacts.Service) ([]contacts.AddressBook, error) {
	allowed, restricted, err := allowedContactBooks(ctx, scopes, service, config.ResourceContactsRead)
	if err != nil || !restricted {
		return books, err
	}
	out := make([]contacts.AddressBook, 0, len(books))
	for _, book := range books {
		if allowed[book.Identifier] {
			out = append(out, book)
		}
	}
	return out, nil
}

func filterContactsByBook(contactsList []contacts.ContactSummary, addressBook string) []contacts.ContactSummary {
	out := make([]contacts.ContactSummary, 0, len(contactsList))
	for _, contact := range contactsList {
		if contact.AddressBook == addressBook {
			out = append(out, contact)
		}
	}
	return out
}

func sortContactSummaries(result *contacts.SearchResult) {
	sort.SliceStable(result.Contacts, func(i, j int) bool {
		left := strings.ToLower(result.Contacts[i].DisplayName)
		right := strings.ToLower(result.Contacts[j].DisplayName)
		if left != right {
			return left < right
		}
		if result.Contacts[i].UID != result.Contacts[j].UID {
			return result.Contacts[i].UID < result.Contacts[j].UID
		}
		return result.Contacts[i].AddressBook < result.Contacts[j].AddressBook
	})
}

func searchScopedContacts(ctx context.Context, scopes config.ResourceScopes, service contacts.Service, opts contacts.SearchOptions) (contacts.SearchResult, error) {
	if opts.AddressBook != "" {
		if err := authorizeContactBook(ctx, scopes, service, config.ResourceContactsRead, opts.AddressBook); err != nil {
			return contacts.SearchResult{}, err
		}
		result, err := service.SearchContacts(ctx, opts)
		if err != nil {
			return contacts.SearchResult{}, err
		}
		result.Contacts = filterContactsByBook(result.Contacts, opts.AddressBook)
		return result, nil
	}
	allowed, restricted, err := allowedContactBooks(ctx, scopes, service, config.ResourceContactsRead)
	if err != nil {
		return contacts.SearchResult{}, err
	}
	if !restricted {
		return service.SearchContacts(ctx, opts)
	}
	ids := make([]string, 0, len(allowed))
	for id := range allowed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var combined contacts.SearchResult
	for _, id := range ids {
		bookOpts := opts
		bookOpts.AddressBook = id
		part, err := service.SearchContacts(ctx, bookOpts)
		if err != nil {
			return contacts.SearchResult{}, err
		}
		combined.Contacts = append(combined.Contacts, filterContactsByBook(part.Contacts, id)...)
		combined.Truncated = combined.Truncated || part.Truncated
		combined.ScanLimitReached = combined.ScanLimitReached || part.ScanLimitReached
	}
	sortContactSummaries(&combined)
	if opts.Limit > 0 && len(combined.Contacts) > opts.Limit {
		combined.Contacts = combined.Contacts[:opts.Limit]
		combined.Truncated = true
	}
	return combined, nil
}

func authorizeMailbox(scopes config.ResourceScopes, action, mailbox string) error {
	values, restricted := scopes.Mail(action)
	if !restricted || scopeContains(values, mailbox) {
		return nil
	}
	return &maildomain.Error{Code: maildomain.CodeAuthorization, Message: "mailbox is outside the configured resource scope"}
}

func filterMailboxes(mailboxes []maildomain.Mailbox, scopes config.ResourceScopes) []maildomain.Mailbox {
	readValues, readRestricted := scopes.Mail(config.ResourceMailRead)
	if !readRestricted {
		return mailboxes
	}
	allowed := make(map[string]bool)
	for _, value := range readValues {
		allowed[value] = true
	}
	for _, action := range []string{config.ResourceMailMutate, config.ResourceMailArchive} {
		values, restricted := scopes.Mail(action)
		if !restricted {
			return mailboxes
		}
		for _, value := range values {
			allowed[value] = true
		}
	}
	out := make([]maildomain.Mailbox, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		if allowed[mailbox.Name] {
			out = append(out, mailbox)
		}
	}
	return out
}

func uniqueTrashMailbox(mailboxes []maildomain.Mailbox) (string, error) {
	var trash []string
	for _, mailbox := range mailboxes {
		selectable := true
		for _, attribute := range mailbox.Attributes {
			if strings.EqualFold(attribute, "\\Noselect") || strings.EqualFold(attribute, "\\NonExistent") {
				selectable = false
			}
		}
		if !selectable {
			continue
		}
		for _, attribute := range mailbox.Attributes {
			if strings.EqualFold(attribute, "\\Trash") {
				trash = append(trash, mailbox.Name)
				break
			}
		}
	}
	if len(trash) != 1 {
		return "", &maildomain.Error{Code: maildomain.CodeProtocolError, Message: "exactly one selectable SPECIAL-USE Trash mailbox is required"}
	}
	return trash[0], nil
}
