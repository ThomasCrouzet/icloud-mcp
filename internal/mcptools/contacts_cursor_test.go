package mcptools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestSearchContactsCursorPagesLargeResult(t *testing.T) {
	summaries := make([]contacts.ContactSummary, 150)
	for i := range summaries {
		summaries[i] = contacts.ContactSummary{
			AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA",
			UID:         fmt.Sprintf("uid-%03d", i),
			DisplayName: fmt.Sprintf("Person %03d", i),
		}
	}
	service := &fakeContactsService{searchResult: contacts.SearchResult{Contacts: summaries}}
	handler := searchContactsHandler(ContactsDeps{Service: service, Redactor: security.NewRedactor("unused")})

	first, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 100}))
	if err != nil || first.IsError {
		t.Fatalf("first page: err=%v result=%s", err, resultText(t, first))
	}
	var firstPage contacts.SearchResult
	decodeResult(t, first, &firstPage)
	if len(firstPage.Contacts) != 100 || firstPage.NextCursor == "" || !firstPage.Truncated {
		t.Fatalf("first page = %d rows, cursor=%v truncated=%v", len(firstPage.Contacts), firstPage.NextCursor != "", firstPage.Truncated)
	}
	if service.lastSearch.PageAll != true {
		t.Fatal("cursor page did not request the complete bounded match set")
	}

	second, err := handler(context.Background(), newReq(map[string]any{"cursor": firstPage.NextCursor, "limit": 100}))
	if err != nil || second.IsError {
		t.Fatalf("second page: err=%v result=%s", err, resultText(t, second))
	}
	var secondPage contacts.SearchResult
	decodeResult(t, second, &secondPage)
	if len(secondPage.Contacts) != 50 || secondPage.NextCursor != "" {
		t.Fatalf("second page = %d rows, cursor=%v", len(secondPage.Contacts), secondPage.NextCursor != "")
	}
	if secondPage.Contacts[0].DisplayName != "Person 100" {
		t.Fatalf("second page starts at %q", secondPage.Contacts[0].DisplayName)
	}

	forged, err := handler(context.Background(), newReq(map[string]any{"cursor": "c1.invalid.invalid", "limit": 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !forged.IsError {
		t.Fatal("forged cursor was accepted")
	}

	mismatch, err := handler(context.Background(), newReq(map[string]any{"cursor": firstPage.NextCursor, "limit": 10, "query": "Different"}))
	if err != nil {
		t.Fatal(err)
	}
	if !mismatch.IsError {
		t.Fatal("cursor reused with different filters was accepted")
	}
}

func TestContactsCursorTokenCarriesNoContactPII(t *testing.T) {
	summaries := []contacts.ContactSummary{
		{AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA", UID: "private-uid", DisplayName: "Alice Private"},
		{AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA", UID: "private-uid-2", DisplayName: "Bob Private"},
	}
	service := &fakeContactsService{searchResult: contacts.SearchResult{Contacts: summaries}}
	handler := searchContactsHandler(ContactsDeps{Service: service, Redactor: security.NewRedactor("unused")})
	res, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 1}))
	if err != nil || res.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, res))
	}
	var page contacts.SearchResult
	decodeResult(t, res, &page)
	if page.NextCursor == "" {
		t.Fatal("no cursor")
	}
	parts := strings.Split(page.NextCursor, ".")
	if len(parts) != 3 {
		t.Fatalf("token shape = %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)
	for _, secret := range []string{"Alice", "private-uid", "book-AAAA", "Private"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("cursor payload leaked %q: %s", secret, payload)
		}
	}
}

func TestContactsCursorRejectsChangedReadScope(t *testing.T) {
	summaries := []contacts.ContactSummary{
		{AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA", UID: "a", DisplayName: "Alice"},
		{AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA", UID: "b", DisplayName: "Bob"},
	}
	service := &fakeContactsService{
		books:        []contacts.AddressBook{{Identifier: "book-AAAAAAAAAAAAAAAAAAAAAA"}},
		searchResult: contacts.SearchResult{Contacts: summaries},
	}
	codec := newContactsCursorCodec()
	openDeps := ContactsDeps{Service: service, Redactor: security.NewRedactor("unused"), ContactsCursor: codec}
	openDeps.ResourceScopes = mustScopes(t, "contacts.read=*")
	first, err := searchContactsHandler(openDeps)(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 1}))
	if err != nil || first.IsError {
		t.Fatalf("open first page: %v %s", err, resultText(t, first))
	}
	var page contacts.SearchResult
	decodeResult(t, first, &page)

	scopedDeps := openDeps
	scopedDeps.ResourceScopes = mustScopes(t, "contacts.read=book-AAAAAAAAAAAAAAAAAAAAAA")
	second, err := searchContactsHandler(scopedDeps)(context.Background(), newReq(map[string]any{"cursor": page.NextCursor, "limit": 1}))
	if err != nil {
		t.Fatal(err)
	}
	if !second.IsError {
		t.Fatal("cursor from a different read scope was accepted")
	}
}

func TestContactsSortKeyTrimsLikeServiceSort(t *testing.T) {
	if contacts.NormalizeSortName(" Alice\t") != "alice" {
		t.Fatal("canonical sort key does not trim")
	}
	position := contactsSortKey{DisplayName: "alice", UID: "a", AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA"}
	if !contactsSortKeyAfter(" Alice", "b", "book-AAAAAAAAAAAAAAAAAAAAAA", position) {
		t.Fatal("whitespace-normalized continuation skipped the next row")
	}
}

func TestContactsCursorReachesEveryDensePage(t *testing.T) {
	summaries := make([]contacts.ContactSummary, 200)
	for i := range summaries {
		email := strings.Repeat("x", 300) + fmt.Sprintf("%03d@example.test", i)
		summaries[i] = contacts.ContactSummary{
			AddressBook: "book-AAAAAAAAAAAAAAAAAAAAAA",
			UID:         fmt.Sprintf("uid-%03d", i),
			DisplayName: fmt.Sprintf("Dense %03d", i),
			Emails:      []contacts.TypedValue{{Value: email}},
		}
	}
	service := &fakeContactsService{searchResult: contacts.SearchResult{Contacts: summaries}}
	handler := searchContactsHandler(ContactsDeps{Service: service, Redactor: security.NewRedactor("unused")})
	cursor := "start"
	seen := make(map[string]bool)
	pages := 0
	for {
		res, err := handler(context.Background(), newReq(map[string]any{"cursor": cursor, "limit": 100}))
		if err != nil || res.IsError {
			t.Fatalf("page %d: %v %s", pages, err, resultText(t, res))
		}
		var page contacts.SearchResult
		decodeResult(t, res, &page)
		for _, summary := range page.Contacts {
			if seen[summary.UID] {
				t.Fatalf("duplicate UID %s on page %d", summary.UID, pages)
			}
			seen[summary.UID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 200 || pages < 2 {
		t.Fatalf("reached %d contacts over %d pages", len(seen), pages)
	}
}
