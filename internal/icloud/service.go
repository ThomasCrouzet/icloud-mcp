// Package icloud implements iCloud calendar access via CalDAV: shard
// discovery, calendar listing, event search/create/update/delete, and
// recurrence expansion.
package icloud

import (
	"context"
	"time"
)

// MutationScope selects whether an update/delete applies to the whole series
// or a single occurrence of a recurring event.
type MutationScope string

const (
	// ScopeSeries modifies/deletes the master VEVENT (default).
	ScopeSeries MutationScope = "series"
	// ScopeOccurrence modifies/deletes a single occurrence identified by
	// RecurrenceID (override VEVENT or EXDATE). An occurrence-scoped delete
	// must NEVER remove the whole series.
	ScopeOccurrence MutationScope = "occurrence"
)

// Calendar represents an iCloud calendar.
type Calendar struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"` // e.g. "#FF2968FF" (apple:calendar-color)
}

// Event represents a calendar event (or an expanded occurrence of a
// recurring event). Path is never serialized to MCP clients.
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
	Status     string    `json:"status,omitempty"`
	Transp     string    `json:"transparency,omitempty"`
	URL        string    `json:"url,omitempty"`
	ETag       string    `json:"etag,omitempty"` // concurrency token when known

	// recurrenceID identifies, for an override, the master occurrence it
	// replaces (RECURRENCE-ID property). Zero for a master or a simple
	// event. Internal field, never serialized.
	recurrenceID time.Time
	// exDates lists the excluded dates (EXDATE) of a recurring master.
	// Internal field, never serialized.
	exDates []time.Time
}

// EventDetail is a full read of a single event object (get_event), including
// alarms and concurrency metadata. Path is never exposed.
type EventDetail struct {
	Event
	Alarms []AlarmInfo `json:"alarms,omitempty"`
	// IsRecurring is true when the master has an RRULE.
	IsRecurring bool `json:"isRecurring,omitempty"`
	// OverrideCount is the number of RECURRENCE-ID overrides on the object.
	OverrideCount int `json:"overrideCount,omitempty"`
}

// AlarmInfo is a parsed VALARM for read paths.
type AlarmInfo struct {
	Action        string `json:"action,omitempty"`
	Trigger       string `json:"trigger,omitempty"`
	MinutesBefore int    `json:"minutesBefore,omitempty"`
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
	// out of scope for writes unless ScopeOccurrence is used on update.
	Recurrence string

	// Optional extended fields (V2).
	Timezone     string
	Status       string // TENTATIVE|CONFIRMED|CANCELLED
	Transparency string // OPAQUE|TRANSPARENT
	URL          string
	Alarms       []AlarmSpec
	ExDates      []time.Time
	// ClientUID, when set and valid, is used as the event UID (idempotent
	// create). If the object already exists, CreateEvent returns conflict
	// rather than overwriting.
	ClientUID string
}

// SearchOptions configures SearchEvents. A nil *SearchOptions means defaults
// (expand recurrences).
type SearchOptions struct {
	// ExpandRecurrence, when true (default), expands RRULE into occurrences
	// within the range. When false, only master VEVENTs overlapping the
	// server time-range are returned (with Recurrence still populated).
	ExpandRecurrence bool
}

// DefaultSearchOptions returns expand-on defaults.
func DefaultSearchOptions() SearchOptions {
	return SearchOptions{ExpandRecurrence: true}
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
	Status                 *string
	Transparency           *string
	URL                    *string

	// Scope defaults to series. ScopeOccurrence requires RecurrenceID and
	// never deletes or rewrites the whole series master RRULE away.
	Scope        MutationScope
	RecurrenceID *time.Time
	// IfMatchETag, when set, is sent as If-Match (overrides the ETag read
	// from GET when non-empty).
	IfMatchETag string
}

// DeleteOptions configures DeleteEvent.
type DeleteOptions struct {
	Scope        MutationScope
	RecurrenceID *time.Time
	IfMatchETag  string
	// DryRun performs lookup and validation but emits no PUT/DELETE.
	DryRun bool
}

// DeleteResult is returned by DeleteEvent.
type DeleteResult struct {
	Title       string `json:"deletedTitle,omitempty"`
	DryRun      bool   `json:"dryRun,omitempty"`
	UID         string `json:"uid"`
	Scope       string `json:"scope,omitempty"`
	WouldMutate bool   `json:"wouldMutate,omitempty"`
}

// Service is the interface consumed by the MCP tools (mockable for tests).
type Service interface {
	ListCalendars(ctx context.Context) ([]Calendar, error)
	// SearchEvents lists events overlapping [start, end]. opts may be nil
	// (expand recurrences). When opts.ExpandRecurrence is false, only master
	// objects are returned without occurrence expansion.
	SearchEvents(ctx context.Context, calendarPath string, start, end time.Time, opts *SearchOptions) (SearchResult, error)
	GetEvent(ctx context.Context, calendarPath, uid string) (*EventDetail, error)
	CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (uid string, err error)
	UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error
	// DeleteEvent deletes (or dry-runs) an event by UID. opts may be nil
	// (series delete, no If-Match, not dry-run).
	DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error)
}
