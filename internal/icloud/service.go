// Package icloud implements iCloud calendar access via CalDAV: shard
// discovery, calendar listing, event search/create/update/delete, and
// recurrence expansion.
package icloud

import (
	"context"
	"time"
)

// Calendar represents an iCloud calendar.
type Calendar struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"` // e.g. "#FF2968FF" (apple:calendar-color)
}

// Event represents a calendar event (or an expanded occurrence of a
// recurring event).
type Event struct {
	UID        string    `json:"uid"`
	Path       string    `json:"-"` // server path, internal, never exposed to the MCP client
	Title      string    `json:"title"`
	Location   string    `json:"location,omitempty"`
	Notes      string    `json:"notes,omitempty"` // ical DESCRIPTION
	StartTime  time.Time `json:"start"`
	EndTime    time.Time `json:"end"`
	AllDay     bool      `json:"allDay,omitempty"`
	Recurrence string    `json:"recurrence,omitempty"` // raw RRULE, informational
	Timezone   string    `json:"timezone,omitempty"`

	// recurrenceID identifies, for an override, the master occurrence it
	// replaces (RECURRENCE-ID property). Zero for a master or a simple
	// event. Internal field, never serialized.
	recurrenceID time.Time
	// exDates lists the excluded dates (EXDATE) of a recurring master.
	// Internal field, never serialized.
	exDates []time.Time
}

// NewEvent groups the data needed to create an event (create_event).
type NewEvent struct {
	Title, Location, Notes string
	StartTime, EndTime     time.Time
	AlarmMinutesBefore     int // 0 = no alarm; >0 produces VALARM DISPLAY TRIGGER:-PT<n>M

	// AllDay writes DTSTART/DTEND as VALUE=DATE (end exclusive). When true,
	// StartTime/EndTime are interpreted as calendar dates (only the date
	// components matter).
	AllDay bool

	// Recurrence is an optional RRULE value (without the "RRULE:" prefix),
	// e.g. "FREQ=WEEKLY;COUNT=10". Empty = non-recurring. Validated before PUT.
	// The rule applies to the master VEVENT; overrides (RECURRENCE-ID) are
	// out of scope for writes.
	Recurrence string
}

// SearchResult is returned by Service.SearchEvents.
type SearchResult struct {
	Events []Event
	// TruncatedByExpansion is true when at least one recurring series hit
	// maxOccurrencesPerSeries; some occurrences may be missing from Events.
	TruncatedByExpansion bool
}

// EventUpdate groups the editable fields of an existing event
// (update_event). A nil pointer = field unchanged; a pointer to an empty
// string = clear the field (Title/Location/Notes only).
type EventUpdate struct {
	Title, Location, Notes *string
	StartTime, EndTime     *time.Time
}

// Service is the interface consumed by the MCP tools (mockable for tests).
type Service interface {
	ListCalendars(ctx context.Context) ([]Calendar, error)
	SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) (SearchResult, error)
	CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (uid string, err error)
	UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error
	// DeleteEvent returns the title of the deleted event (echo required by
	// the spec so the agent can confirm to the human what it is deleting).
	DeleteEvent(ctx context.Context, calendarPath, uid string) (deletedTitle string, err error)
}
