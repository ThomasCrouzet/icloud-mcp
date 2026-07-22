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
	Detail    *EventDetail

	// EventsByPath, when non-nil, overrides Events for SearchEvents on that
	// calendar path (used to test multi-calendar query + early-stop).
	EventsByPath map[string][]Event

	ListErr   error
	SearchErr error
	GetErr    error
	CreateErr error
	UpdateErr error
	DeleteErr error

	CreatedUID   string // UID returned by CreateEvent ("mock-uid" by default)
	DeletedTitle string // title returned by DeleteEvent

	// SearchTruncated is returned as SearchResult.TruncatedByExpansion.
	SearchTruncated bool

	// ExistingUIDs, when set, makes CreateEvent return conflict if ClientUID
	// is already present (idempotency / no silent overwrite).
	ExistingUIDs map[string]bool

	// RecordedMutations records PUT/DELETE-like mutations for dry-run proofs.
	// Append-only under mu.
	RecordedMutations []string

	mu sync.Mutex

	LastCreated    *NewEvent
	LastUpdateUID  string
	LastUpdate     *EventUpdate
	LastDeleteUID  string
	LastDeleteOpts *DeleteOptions
	LastGetUID     string
	LastSearchPath string
	LastSearchFrom time.Time
	LastSearchTo   time.Time

	// SearchPaths records each calendar path SearchEvents was called with
	// (order preserved), for multi-calendar early-stop assertions.
	SearchPaths []string

	ListCallCount   int
	SearchCallCount int
	GetCallCount    int
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

// GetEvent returns m.Detail or synthesizes one from Events.
func (m *MockService) GetEvent(ctx context.Context, calendarPath, uid string) (*EventDetail, error) {
	m.mu.Lock()
	m.GetCallCount++
	m.LastGetUID = uid
	m.mu.Unlock()
	if m.GetErr != nil {
		return nil, m.GetErr
	}
	if m.Detail != nil && m.Detail.UID == uid {
		d := *m.Detail
		return &d, nil
	}
	for _, e := range m.Events {
		if e.UID == uid {
			return &EventDetail{Event: e}, nil
		}
	}
	return nil, NewError(CodeNotFound, 404, "event not found (uid="+uid+")", nil)
}

// CreateEvent returns m.CreatedUID (or "mock-uid" by default, or m.CreateErr).
func (m *MockService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	m.mu.Lock()
	m.CreateCallCount++
	m.LastCreated = ev
	m.RecordedMutations = append(m.RecordedMutations, "PUT")
	m.mu.Unlock()
	if m.CreateErr != nil {
		return "", m.CreateErr
	}
	uid := m.CreatedUID
	if uid == "" && ev != nil && ev.ClientUID != "" {
		uid = ev.ClientUID
	}
	if uid == "" {
		uid = "mock-uid"
	}
	if m.ExistingUIDs != nil && m.ExistingUIDs[uid] {
		return "", NewError(CodeConflict, 409, "event already exists (uid="+uid+")", nil)
	}
	return uid, nil
}

// UpdateEvent records the call (or returns m.UpdateErr).
func (m *MockService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	m.mu.Lock()
	m.UpdateCallCount++
	m.LastUpdateUID = uid
	m.LastUpdate = up
	m.RecordedMutations = append(m.RecordedMutations, "PUT")
	m.mu.Unlock()
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	return nil
}

// DeleteEvent returns a DeleteResult (or m.DeleteErr). Dry-run records no mutation.
func (m *MockService) DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error) {
	m.mu.Lock()
	m.DeleteCallCount++
	m.LastDeleteUID = uid
	m.LastDeleteOpts = opts
	dry := opts != nil && opts.DryRun
	if !dry {
		m.RecordedMutations = append(m.RecordedMutations, "DELETE")
	}
	m.mu.Unlock()
	if m.DeleteErr != nil {
		return DeleteResult{}, m.DeleteErr
	}
	scope := string(ScopeSeries)
	if opts != nil && opts.Scope != "" {
		scope = string(opts.Scope)
	}
	return DeleteResult{
		Title:       m.DeletedTitle,
		DryRun:      dry,
		UID:         uid,
		Scope:       scope,
		WouldMutate: true,
	}, nil
}

// MutationCount returns how many mutating HTTP-like operations were recorded.
func (m *MockService) MutationCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.RecordedMutations)
}
