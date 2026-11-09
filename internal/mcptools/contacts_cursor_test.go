package mcptools

import (
	"context"
	"fmt"
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
