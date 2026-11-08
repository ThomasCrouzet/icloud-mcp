package mcptools

import (
	"context"
	"sync"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
)

// This observer forwards to the real service. It makes singleton-book selector
// and filter acceptance observable without replacing business logic or results.
type contractContactsTrace struct {
	contacts.Service
	mu       sync.Mutex
	searches []contacts.SearchOptions
}

func (s *contractContactsTrace) SearchContacts(ctx context.Context, opts contacts.SearchOptions) (contacts.SearchResult, error) {
	s.mu.Lock()
	s.searches = append(s.searches, opts)
	s.mu.Unlock()
	return s.Service.SearchContacts(ctx, opts)
}

func (s *contractContactsTrace) lastSearch() (contacts.SearchOptions, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.searches) == 0 {
		return contacts.SearchOptions{}, false
	}
	return s.searches[len(s.searches)-1], true
}
