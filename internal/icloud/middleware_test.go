package icloud

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingService est un Service instrumenté pour les tests de
// GuardedService : compte les appels, peut simuler N échecs avant succès.
type countingService struct {
	mu sync.Mutex

	listCalls int
	listFailN int

	searchCalls int

	createCalls int
	createErr   error

	updateCalls int
	updateErr   error

	deleteCalls int
	deleteFailN int
}

var _ Service = (*countingService)(nil)

func (s *countingService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	s.mu.Lock()
	s.listCalls++
	calls := s.listCalls
	s.mu.Unlock()
	if calls <= s.listFailN {
		return nil, fmt.Errorf("échec simulé %d", calls)
	}
	return []Calendar{{Path: "/cal/"}}, nil
}

func (s *countingService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	s.mu.Lock()
	s.searchCalls++
	s.mu.Unlock()
	return nil, nil
}

func (s *countingService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	s.mu.Lock()
	s.createCalls++
	s.mu.Unlock()
	if s.createErr != nil {
		return "", s.createErr
	}
	return "uid", nil
}

func (s *countingService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	s.mu.Lock()
	s.updateCalls++
	s.mu.Unlock()
	return s.updateErr
}

func (s *countingService) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	s.mu.Lock()
	s.deleteCalls++
	calls := s.deleteCalls
	s.mu.Unlock()
	if calls <= s.deleteFailN {
		return "", fmt.Errorf("échec simulé %d", calls)
	}
	return "titre", nil
}

func TestGuardedService_ReadRateLimitBlocksAfterBurst(t *testing.T) {
	inner := &countingService{}
	g := NewGuardedService(inner, 0, time.Millisecond)

	// Épuise le burst de lecture (10).
	for i := 0; i < 10; i++ {
		if _, err := g.ListCalendars(context.Background()); err != nil {
			t.Fatalf("appel %d : erreur inattendue : %v", i, err)
		}
	}

	// Le 11e appel doit attendre ~1s pour un nouveau jeton (60/min) ; un
	// contexte à échéance très courte doit donc échouer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := g.ListCalendars(ctx); err == nil {
		t.Fatal("attendu : erreur de limite de débit (burst de lecture épuisé)")
	}
}

func TestGuardedService_WriteBudgetIndependentFromRead(t *testing.T) {
	inner := &countingService{}
	g := NewGuardedService(inner, 0, time.Millisecond)

	for i := 0; i < 10; i++ {
		if _, err := g.ListCalendars(context.Background()); err != nil {
			t.Fatalf("lecture %d : erreur inattendue : %v", i, err)
		}
	}

	// Budget écriture indépendant : doit réussir immédiatement malgré le
	// burst de lecture épuisé.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := g.CreateEvent(ctx, "/cal/", &NewEvent{Title: "x"}); err != nil {
		t.Fatalf("CreateEvent erreur inattendue : %v", err)
	}
}

func TestGuardedService_RetrySucceedsAfterFailures(t *testing.T) {
	inner := &countingService{listFailN: 2}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if _, err := g.ListCalendars(context.Background()); err != nil {
		t.Fatalf("erreur inattendue après retries : %v", err)
	}
	if inner.listCalls != 3 {
		t.Errorf("listCalls = %d, want 3 (1 tentative + 2 retries)", inner.listCalls)
	}
}

func TestGuardedService_RetryExhausted(t *testing.T) {
	inner := &countingService{listFailN: 10}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if _, err := g.ListCalendars(context.Background()); err == nil {
		t.Fatal("attendu : erreur après épuisement des tentatives")
	}
	if inner.listCalls != 3 {
		t.Errorf("listCalls = %d, want 3 (1 tentative + 2 retries, tous en échec)", inner.listCalls)
	}
}

func TestGuardedService_CreateEventNeverRetried(t *testing.T) {
	inner := &countingService{createErr: fmt.Errorf("échec")}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if _, err := g.CreateEvent(context.Background(), "/cal/", &NewEvent{Title: "x"}); err == nil {
		t.Fatal("attendu : erreur")
	}
	if inner.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (CreateEvent n'est jamais retryé, non idempotent)", inner.createCalls)
	}
}

func TestGuardedService_UpdateEventNeverRetried(t *testing.T) {
	inner := &countingService{updateErr: fmt.Errorf("échec")}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if err := g.UpdateEvent(context.Background(), "/cal/", "uid", &EventUpdate{}); err == nil {
		t.Fatal("attendu : erreur")
	}
	if inner.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (UpdateEvent n'est jamais retryé, non idempotent)", inner.updateCalls)
	}
}

func TestGuardedService_DeleteEventRetried(t *testing.T) {
	inner := &countingService{deleteFailN: 1}
	g := NewGuardedService(inner, 2, time.Millisecond)

	title, err := g.DeleteEvent(context.Background(), "/cal/", "uid")
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if title != "titre" {
		t.Errorf("title = %q, want %q", title, "titre")
	}
	if inner.deleteCalls != 2 {
		t.Errorf("deleteCalls = %d, want 2 (1 échec + 1 retry réussi ; DeleteEvent est idempotent)", inner.deleteCalls)
	}
}
