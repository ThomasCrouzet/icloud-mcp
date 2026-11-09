package mcptools

import (
	"context"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
)

type budgetContactsService struct {
	books     []contacts.AddressBook
	cards     int
	bytes     int64
	seenCards []int
	seenBytes []int64
}

func (s *budgetContactsService) ListAddressBooks(context.Context) ([]contacts.AddressBook, error) {
	return s.books, nil
}

func (s *budgetContactsService) SearchContacts(_ context.Context, opts contacts.SearchOptions) (contacts.SearchResult, error) {
	s.seenCards = append(s.seenCards, opts.RemainingCards)
	s.seenBytes = append(s.seenBytes, opts.RemainingBytes)
	usedCards := s.cards
	if usedCards > opts.RemainingCards {
		usedCards = opts.RemainingCards
	}
	usedBytes := s.bytes
	if usedBytes > opts.RemainingBytes {
		usedBytes = opts.RemainingBytes
	}
	return contacts.SearchResult{
		Contacts:         []contacts.ContactSummary{{AddressBook: opts.AddressBook, UID: "uid", DisplayName: "Name"}},
		ScannedCards:     usedCards,
		UsedBytes:        usedBytes,
		ScanLimitReached: opts.RemainingCards <= s.cards,
	}, nil
}

func (s *budgetContactsService) GetContact(context.Context, string, string) (*contacts.Contact, error) {
	return nil, nil
}
func (s *budgetContactsService) CreateContact(context.Context, *contacts.CreateContactInput) (contacts.CreateResult, error) {
	return contacts.CreateResult{}, nil
}
func (s *budgetContactsService) UpdateContact(context.Context, *contacts.UpdateContactInput) (contacts.UpdateResult, error) {
	return contacts.UpdateResult{}, nil
}
func (s *budgetContactsService) DeleteContact(context.Context, *contacts.DeleteContactInput) (contacts.DeleteResult, error) {
	return contacts.DeleteResult{}, nil
}

func TestSearchScopedContactsSharesOneScanBudget(t *testing.T) {
	books := []contacts.AddressBook{
		{Identifier: "book-AAAAAAAAAAAAAAAAAAAAAA"},
		{Identifier: "book-BBBBBBBBBBBBBBBBBBBBBB"},
		{Identifier: "book-CCCCCCCCCCCCCCCCCCCCCC"},
	}
	service := &budgetContactsService{books: books, cards: 1000, bytes: 16 << 20}
	scopes := mustScopes(t, "contacts.read="+books[0].Identifier+","+books[1].Identifier+","+books[2].Identifier)
	result, err := searchScopedContacts(context.Background(), scopes, service, contacts.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("searchScopedContacts() error = %v", err)
	}
	if len(service.seenCards) != 2 {
		t.Fatalf("books queried = %d, want 2 before the budget was exhausted", len(service.seenCards))
	}
	if service.seenCards[0] != contacts.MaxCardsScanned || service.seenCards[1] != contacts.MaxCardsScanned-1000 {
		t.Fatalf("per-book card budgets = %v", service.seenCards)
	}
	if service.seenBytes[1] != int64(contacts.MaxReportBytes)-(16<<20) {
		t.Fatalf("per-book byte budgets = %v", service.seenBytes)
	}
	if !result.ScanLimitReached {
		t.Fatal("aggregate budget exhaustion was not reported")
	}
	if result.ScannedCards != 2000 || result.UsedBytes != 32<<20 {
		t.Fatalf("aggregate usage = %d cards, %d bytes", result.ScannedCards, result.UsedBytes)
	}
}
