package icloud

import (
	"fmt"
	"strings"
	"time"
)

// Bornes de validation d'entrée, appliquées côté handler MCP (avant tout
// appel réseau) ET côté Client (défense en profondeur).
const (
	MaxTitleLen    = 500
	MaxLocationLen = 1000
	MaxNotesLen    = 4000
	MaxQueryLen    = 200
	MaxUIDLen      = 255
	MaxRangeDays   = 366 // borne la fenêtre search_events (et donc l'expansion)
	MaxResults     = 400 // borne dure de la spec
)

// ValidateCalendarPath vérifie qu'un path de calendrier est plausible :
// non vide, commence par '/', sans traversée de répertoire ni caractères de
// contrôle, taille bornée.
func ValidateCalendarPath(path string) error {
	if path == "" {
		return fmt.Errorf("le path du calendrier ne peut pas être vide")
	}
	if len(path) > 1024 {
		return fmt.Errorf("le path du calendrier est trop long (max 1024 caractères)")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("le path du calendrier doit commencer par '/'")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("le path du calendrier contient une séquence de traversée de répertoire ('..')")
	}
	if strings.ContainsAny(path, "\x00\n\r") {
		return fmt.Errorf("le path du calendrier contient des caractères invalides")
	}
	return nil
}

// ValidateUID vérifie qu'un UID d'événement est plausible.
func ValidateUID(uid string) error {
	if uid == "" {
		return fmt.Errorf("l'UID ne peut pas être vide")
	}
	if len(uid) > MaxUIDLen {
		return fmt.Errorf("l'UID est trop long (max %d caractères)", MaxUIDLen)
	}
	if strings.Contains(uid, "..") {
		return fmt.Errorf("l'UID contient une séquence de traversée de répertoire ('..')")
	}
	if strings.ContainsAny(uid, "\x00\n\r/%") {
		return fmt.Errorf("l'UID contient des caractères invalides")
	}
	return nil
}

// ValidateTextField vérifie la taille et l'absence de caractères NUL d'un
// champ texte libre (title/location/notes/query). Le saut de ligne est
// toléré (les notes peuvent être multi-lignes) ; go-ical échappe
// correctement \n, ;, , et \ à l'encodage TEXT (SetText), pas d'injection
// de propriété iCalendar possible via ces champs, donc pas de ré-échappement
// manuel ici.
func ValidateTextField(name, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("%s trop long (max %d caractères, reçu %d)", name, max, len(value))
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contient un caractère interdit (NUL)", name)
	}
	return nil
}

// ParseRFC3339 parse une date/heure au format RFC3339 avec un message
// d'erreur pédagogique (l'agent LLM appelant doit comprendre comment
// corriger son appel).
func ParseRFC3339(name, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s invalide (%q) : format attendu RFC3339, ex. 2026-07-01T00:00:00Z : %w", name, value, err)
	}
	return t, nil
}

// ValidateRange vérifie que end > start et que la plage ne dépasse pas
// MaxRangeDays jours (borne aussi indirectement l'expansion de récurrences).
func ValidateRange(start, end time.Time) error {
	if !end.After(start) {
		return fmt.Errorf("la date de fin (%s) doit être après la date de début (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	if end.Sub(start) > MaxRangeDays*24*time.Hour {
		return fmt.Errorf("la plage de dates dépasse %d jours (maximum autorisé)", MaxRangeDays)
	}
	return nil
}
