package icloud

import (
	"context"
	"sync"
	"time"
)

// MockService implémente Service pour les tests des tools MCP (sans réseau).
//
// Les champs de configuration (Calendars, Events, *Err, CreatedUID,
// DeletedTitle) sont attendus positionnés par le test AVANT tout appel
// concurrent et ne sont ensuite que lus, pas de protection nécessaire pour
// eux. En revanche les compteurs et champs Last* sont mutés à CHAQUE appel :
// GuardedService peut invoquer ces méthodes depuis des goroutines
// concurrentes (retry/rate-limit), donc ces écritures doivent être protégées
// par mu (défense contre une race future, même si aucun test actuel ne les
// exerce en parallèle).
type MockService struct {
	Calendars []Calendar
	Events    []Event

	ListErr   error
	SearchErr error
	CreateErr error
	UpdateErr error
	DeleteErr error

	CreatedUID   string // UID retourné par CreateEvent ("mock-uid" par défaut)
	DeletedTitle string // titre retourné par DeleteEvent

	mu sync.Mutex

	LastCreated    *NewEvent
	LastUpdateUID  string
	LastUpdate     *EventUpdate
	LastDeleteUID  string
	LastSearchPath string
	LastSearchFrom time.Time
	LastSearchTo   time.Time

	ListCallCount   int
	SearchCallCount int
	CreateCallCount int
	UpdateCallCount int
	DeleteCallCount int
}

var _ Service = (*MockService)(nil)

// ListCalendars retourne m.Calendars (ou m.ListErr).
func (m *MockService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	m.mu.Lock()
	m.ListCallCount++
	m.mu.Unlock()
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	return m.Calendars, nil
}

// SearchEvents retourne m.Events (ou m.SearchErr).
func (m *MockService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	m.mu.Lock()
	m.SearchCallCount++
	m.LastSearchPath = calendarPath
	m.LastSearchFrom = start
	m.LastSearchTo = end
	m.mu.Unlock()
	if m.SearchErr != nil {
		return nil, m.SearchErr
	}
	return m.Events, nil
}

// CreateEvent retourne m.CreatedUID (ou "mock-uid" par défaut, ou m.CreateErr).
func (m *MockService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	m.mu.Lock()
	m.CreateCallCount++
	m.LastCreated = ev
	m.mu.Unlock()
	if m.CreateErr != nil {
		return "", m.CreateErr
	}
	uid := m.CreatedUID
	if uid == "" {
		uid = "mock-uid"
	}
	return uid, nil
}

// UpdateEvent enregistre l'appel (ou retourne m.UpdateErr).
func (m *MockService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	m.mu.Lock()
	m.UpdateCallCount++
	m.LastUpdateUID = uid
	m.LastUpdate = up
	m.mu.Unlock()
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	return nil
}

// DeleteEvent retourne m.DeletedTitle (ou m.DeleteErr).
func (m *MockService) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	m.mu.Lock()
	m.DeleteCallCount++
	m.LastDeleteUID = uid
	m.mu.Unlock()
	if m.DeleteErr != nil {
		return "", m.DeleteErr
	}
	return m.DeletedTitle, nil
}
