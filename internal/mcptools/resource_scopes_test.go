package mcptools

import (
	"context"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func mustScopes(t *testing.T, value string) config.ResourceScopes {
	t.Helper()
	scopes, err := config.ParseResourceScopes(value)
	if err != nil {
		t.Fatal(err)
	}
	return scopes
}

func TestCalendarResourceScopes(t *testing.T) {
	scopes := mustScopes(t, "calendar.read=/cal/a/,/cal/b/;calendar.write=/cal/a/;calendar.delete=/cal/b/")
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}, {Path: "/cal/secret/"}},
		Events:    []icloud.Event{{UID: "u1", Title: "x", StartTime: mustParseTime("2026-07-01T09:00:00Z"), EndTime: mustParseTime("2026-07-01T10:00:00Z")}},
	}
	deps := testDeps(svc)
	deps.ResourceScopes = scopes

	list, err := listCalendarsHandler(deps)(context.Background(), newReq(map[string]any{}))
	if err != nil || list.IsError {
		t.Fatalf("list calendars: %v %s", err, resultText(t, list))
	}
	var listed calendarsResponse
	decodeResult(t, list, &listed)
	if listed.Count != 2 || listed.Calendars[0].Path != "/cal/a/" || listed.Calendars[1].Path != "/cal/b/" {
		t.Fatalf("filtered calendars = %+v", listed)
	}

	search, err := searchEventsHandler(deps)(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-08T00:00:00Z", "calendar": "/cal/secret/",
	}))
	if err != nil || !search.IsError {
		t.Fatalf("explicit unauthorized search = %v %s", err, resultText(t, search))
	}

	get, err := getEventHandler(deps)(context.Background(), newReq(map[string]any{"calendar": "/cal/secret/", "uid": "u1"}))
	if err != nil || !get.IsError {
		t.Fatalf("unauthorized get = %v %s", err, resultText(t, get))
	}

	create, err := createEventHandler(deps)(context.Background(), newReq(map[string]any{
		"title": "new", "start": "2026-07-01T09:00:00Z", "end": "2026-07-01T10:00:00Z", "calendar": "/cal/b/",
	}))
	if err != nil || !create.IsError || svc.CreateCallCount != 0 {
		t.Fatalf("unauthorized create = %v %s calls=%d", err, resultText(t, create), svc.CreateCallCount)
	}

	del, err := deleteEventHandler(deps)(context.Background(), newReq(map[string]any{"calendar": "/cal/a/", "uid": "u1"}))
	if err != nil || !del.IsError || svc.DeleteCallCount != 0 {
		t.Fatalf("unauthorized delete = %v %s calls=%d", err, resultText(t, del), svc.DeleteCallCount)
	}
}

func TestContactResourceScopes(t *testing.T) {
	scopes := mustScopes(t, "contacts.read=Book A;contacts.write=Book A")
	bookA := contacts.AddressBook{Identifier: "book-AAAAAAAAAAAAAAAAAAAAAA", Name: "Book A"}
	bookB := contacts.AddressBook{Identifier: "book-BBBBBBBBBBBBBBBBBBBBBB", Name: "Book B"}
	service := &fakeContactsService{
		books: []contacts.AddressBook{bookA, bookB},
		searchResult: contacts.SearchResult{Contacts: []contacts.ContactSummary{
			{AddressBook: bookA.Identifier, UID: "a", DisplayName: "Alpha"},
			{AddressBook: bookB.Identifier, UID: "b", DisplayName: "Beta"},
		}},
		contact: &contacts.Contact{ContactSummary: contacts.ContactSummary{AddressBook: bookA.Identifier, UID: "a"}},
	}
	deps := contactsTestDeps(service, nil)
	deps.ResourceScopes = scopes

	list, err := listAddressBooksHandler(deps)(context.Background(), newReq(map[string]any{}))
	if err != nil || list.IsError {
		t.Fatalf("list books: %v %s", err, resultText(t, list))
	}
	var listed contactsAddressBooksResponse
	decodeResult(t, list, &listed)
	if listed.Count != 1 || listed.AddressBooks[0].Identifier != bookA.Identifier {
		t.Fatalf("filtered books = %+v", listed)
	}

	search, err := searchContactsHandler(deps)(context.Background(), newReq(map[string]any{}))
	if err != nil || search.IsError {
		t.Fatalf("scoped search: %v %s", err, resultText(t, search))
	}
	var searched contacts.SearchResult
	decodeResult(t, search, &searched)
	if len(searched.Contacts) != 1 || searched.Contacts[0].AddressBook != bookA.Identifier {
		t.Fatalf("scoped search contacts = %+v", searched.Contacts)
	}

	denied, err := getContactHandler(deps)(context.Background(), newReq(map[string]any{
		"address_book": bookB.Identifier, "uid": "b",
	}))
	if err != nil || !denied.IsError || service.getCalls != 0 {
		t.Fatalf("unauthorized get contact = %v %s calls=%d", err, resultText(t, denied), service.getCalls)
	}

	create, err := createContactHandler(deps)(context.Background(), newReq(map[string]any{
		"address_book": bookB.Identifier, "display_name": "Denied",
	}))
	if err != nil || !create.IsError || service.createCalls != 0 {
		t.Fatalf("unauthorized create contact = %v %s calls=%d", err, resultText(t, create), service.createCalls)
	}
}

func TestContactScopeAmbiguousNameFailsClosed(t *testing.T) {
	scopes := mustScopes(t, "contacts.read=Duplicate")
	service := &fakeContactsService{books: []contacts.AddressBook{
		{Identifier: "book-AAAAAAAAAAAAAAAAAAAAAA", Name: "Duplicate"},
		{Identifier: "book-BBBBBBBBBBBBBBBBBBBBBB", Name: "Duplicate"},
	}}
	deps := contactsTestDeps(service, nil)
	deps.ResourceScopes = scopes
	result, err := listAddressBooksHandler(deps)(context.Background(), newReq(map[string]any{}))
	if err != nil || !result.IsError {
		t.Fatalf("ambiguous scope did not fail: %v %s", err, resultText(t, result))
	}
}

func TestMailResourceScopes(t *testing.T) {
	scopes := mustScopes(t, "mail.read=INBOX;mail.mutate=INBOX")
	service := &mailToolsFakeService{
		listResult: maildomain.ListMailboxesResult{Mailboxes: []maildomain.Mailbox{
			{Name: "INBOX"}, {Name: "Archive"}, {Name: "Trash", Attributes: []string{"\\Trash"}},
		}},
		searchResult: maildomain.SearchResult{Messages: []maildomain.MessageSummary{{Mailbox: "INBOX", UIDValidity: 1, UID: 1}}},
	}
	deps := mailToolsDeps(service)
	deps.ResourceScopes = scopes

	list, err := listMailboxesHandler(deps)(context.Background(), newReq(map[string]any{}))
	if err != nil || list.IsError {
		t.Fatalf("list mailboxes: %v %s", err, resultText(t, list))
	}
	var listed maildomain.ListMailboxesResult
	decodeResult(t, list, &listed)
	if len(listed.Mailboxes) != 1 || listed.Mailboxes[0].Name != "INBOX" || !listed.Truncated {
		t.Fatalf("filtered mailboxes = %+v", listed)
	}

	search, err := searchMessagesHandler(deps)(context.Background(), newReq(map[string]any{"mailbox": "Archive"}))
	if err != nil || !search.IsError || service.searchCalls != 0 {
		t.Fatalf("unauthorized search = %v %s calls=%d", err, resultText(t, search), service.searchCalls)
	}

	move, err := moveMessageHandler(deps)(context.Background(), newReq(map[string]any{
		"mailbox": "INBOX", "uid_validity": float64(1), "uid": float64(1), "destination": "Archive",
	}))
	if err != nil || !move.IsError || service.moveCalls != 0 {
		t.Fatalf("unauthorized move destination = %v %s calls=%d", err, resultText(t, move), service.moveCalls)
	}

	trash, err := trashMessageHandler(deps)(context.Background(), newReq(map[string]any{
		"mailbox": "INBOX", "uid_validity": float64(1), "uid": float64(1),
	}))
	if err != nil || !trash.IsError || service.trashCalls != 0 {
		t.Fatalf("unauthorized trash destination = %v %s calls=%d", err, resultText(t, trash), service.trashCalls)
	}
}

func mustParseTime(value string) (parsed time.Time) {
	parsed, _ = time.Parse(time.RFC3339, value)
	return parsed
}
