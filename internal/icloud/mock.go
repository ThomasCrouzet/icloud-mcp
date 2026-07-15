package icloud

import (
	"context"
	"sync"
	"time"
)

// MockService implements Service for the MCP tools tests (no network).
//
// The configuration fields (Calendars, Events, *Err, CreatedUID,
// DeletedTitle) are expected to be set by the test BEFORE any concurrent
// call and are only read afterwards; no protection is needed for them. The
// counters and Last* fields, however, are mutated on EVERY call:
// GuardedService may invoke these methods from concurrent goroutines
// (retry/rate-limit), so those writes must be protected by mu (defense
// against a future race, even if no current test exercises them in
// parallel).
type MockService struct {
	Calendars []Calendar
	Events    []Event

	// EventsByPath, when non-nil, overrides Events for SearchEvents on that
	// calendar path (used to test multi-calendar query + early-stop).
	EventsByPath map[string][]Event

	ListErr   error
	SearchErr error
	CreateErr error
	UpdateErr error
	DeleteErr error

	CreatedUID   string // UID returned by CreateEvent ("mock-uid" by default)
	DeletedTitle string // title returned by DeleteEvent

	// SearchTruncated is returned as SearchResult.TruncatedByExpansion.
	SearchTruncated bool

	mu sync.Mutex

	LastCreated    *NewEvent
	LastUpdateUID  string
	LastUpdate     *EventUpdate
	LastDeleteUID  string
	LastSearchPath string
	LastSearchFrom time.Time
	LastSearchTo   time.Time

	// SearchPaths records each calendar path SearchEvents was called with
	// (order preserved), for multi-calendar early-stop assertions.
	SearchPaths []string

	ListCallCount   int
	SearchCallCount int
	CreateCallCount int
	UpdateCallCount int
	DeleteCallCount int
}

var _ Service = (*MockService)(nil)

// ListCalendars returns m.Calendars (or m.ListErr).
func (m *MockService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	m.mu.Lock()
	m.ListCallCount++
	m.mu.Unlock()
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	return m.Calendars, nil
}

// SearchEvents returns m.Events (or m.SearchErr). When EventsByPath has an
// entry for calendarPath, that slice is used instead of m.Events.
func (m *MockService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) (SearchResult, error) {
	m.mu.Lock()
	m.SearchCallCount++
	m.LastSearchPath = calendarPath
	m.LastSearchFrom = start
	m.LastSearchTo = end
	m.SearchPaths = append(m.SearchPaths, calendarPath)
	truncated := m.SearchTruncated
	events := m.Events
	if m.EventsByPath != nil {
		if byPath, ok := m.EventsByPath[calendarPath]; ok {
			events = byPath
		}
	}
	m.mu.Unlock()
	if m.SearchErr != nil {
		return SearchResult{}, m.SearchErr
	}
	return SearchResult{Events: events, TruncatedByExpansion: truncated}, nil
}

// CreateEvent returns m.CreatedUID (or "mock-uid" by default, or m.CreateErr).
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

// UpdateEvent records the call (or returns m.UpdateErr).
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

// DeleteEvent returns m.DeletedTitle (or m.DeleteErr).
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
