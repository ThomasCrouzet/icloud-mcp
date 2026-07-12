// Package icloud implémente l'accès au calendrier iCloud via CalDAV :
// découverte du shard, liste des calendriers, recherche/création/mise à
// jour/suppression d'événements, expansion des récurrences.
package icloud

import (
	"context"
	"time"
)

// Calendar représente un calendrier iCloud.
type Calendar struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"` // ex. "#FF2968FF" (apple:calendar-color)
}

// Event représente un événement de calendrier (ou une occurrence développée
// d'un événement récurrent).
type Event struct {
	UID        string    `json:"uid"`
	Path       string    `json:"-"` // path serveur, interne, jamais exposé au client MCP
	Title      string    `json:"title"`
	Location   string    `json:"location,omitempty"`
	Notes      string    `json:"notes,omitempty"` // DESCRIPTION ical
	StartTime  time.Time `json:"start"`
	EndTime    time.Time `json:"end"`
	AllDay     bool      `json:"allDay,omitempty"`
	Recurrence string    `json:"recurrence,omitempty"` // RRULE brute, informatif
	Timezone   string    `json:"timezone,omitempty"`

	// recurrenceID identifie, pour un override, l'occurrence du master qu'il
	// remplace (propriété RECURRENCE-ID). Zéro pour un master ou un
	// événement simple. Champ interne, jamais sérialisé.
	recurrenceID time.Time
	// exDates liste les dates exclues (EXDATE) d'un master récurrent.
	// Champ interne, jamais sérialisé.
	exDates []time.Time
}

// NewEvent regroupe les données de création d'un événement (create_event).
type NewEvent struct {
	Title, Location, Notes string
	StartTime, EndTime     time.Time
	AlarmMinutesBefore     int // 0 = pas d'alarme ; >0 → VALARM DISPLAY TRIGGER:-PT<n>M
}

// EventUpdate regroupe les champs modifiables d'un événement existant
// (update_event). Un pointeur nil = champ inchangé ; un pointeur vers une
// chaîne vide = effacement du champ (Title/Location/Notes uniquement).
type EventUpdate struct {
	Title, Location, Notes *string
	StartTime, EndTime     *time.Time
}

// Service est l'interface consommée par les tools MCP (mockable pour les tests).
type Service interface {
	ListCalendars(ctx context.Context) ([]Calendar, error)
	SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error)
	CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (uid string, err error)
	UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error
	// DeleteEvent retourne le titre de l'événement supprimé (écho exigé par
	// la spec pour que l'agent puisse confirmer à l'humain ce qu'il supprime).
	DeleteEvent(ctx context.Context, calendarPath, uid string) (deletedTitle string, err error)
}
