package icloud

import (
	"strings"
	"testing"
	"time"
)

func TestValidateCalendarPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valide", "/121234567/calendars/home/", false},
		{"vide", "", true},
		{"sans slash initial", "121234567/calendars/home/", true},
		{"traversée de répertoire", "/121234567/../etc/passwd", true},
		{"NUL", "/cal\x00endar/", true},
		{"CRLF injection", "/calendar/\r\nDELETE-ALL", true},
		{"trop long", "/" + strings.Repeat("a", 1025), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCalendarPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCalendarPath(%q) erreur = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestValidateUID(t *testing.T) {
	tests := []struct {
		name    string
		uid     string
		wantErr bool
	}{
		{"valide simple", "abcdef0123456789@icloud-mcp", false},
		{"vide", "", true},
		{"trop long", strings.Repeat("a", 256), true},
		{"traversée de répertoire", "../../etc/passwd", true},
		{"slash", "abc/def", true},
		{"pourcentage (encodage suspicious)", "abc%2Fdef", true},
		{"NUL", "abc\x00def", true},
		{"retour chariot", "abc\rdef", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUID(tt.uid)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUID(%q) erreur = %v, wantErr %v", tt.uid, err, tt.wantErr)
			}
		})
	}
}

func TestValidateTextField(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		max     int
		wantErr bool
	}{
		{"vide", "", 10, false},
		{"dans la limite", "abcde", 10, false},
		{"exactement la limite", "abcdefghij", 10, false},
		{"trop long", "abcdefghijk", 10, true},
		{"saut de ligne toléré", "ligne1\nligne2", 20, false},
		{"NUL refusé", "abc\x00def", 20, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTextField("champ", tt.value, tt.max)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTextField(%q, max=%d) erreur = %v, wantErr %v", tt.value, tt.max, err, tt.wantErr)
			}
		})
	}
}

func TestParseRFC3339(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valide UTC", "2026-07-01T00:00:00Z", false},
		{"valide avec offset", "2026-07-01T00:00:00+02:00", false},
		{"format date seule", "2026-07-01", true},
		{"vide", "", true},
		{"texte libre", "demain matin", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRFC3339("start", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRFC3339(%q) erreur = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateRange(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		start   time.Time
		end     time.Time
		wantErr bool
	}{
		{"valide 1 semaine", base, base.AddDate(0, 0, 7), false},
		{"start == end", base, base, true},
		{"end avant start", base, base.AddDate(0, 0, -1), true},
		{"exactement 366 jours", base, base.AddDate(0, 0, 366), false},
		{"367 jours (trop)", base, base.AddDate(0, 0, 367), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRange(tt.start, tt.end)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRange() erreur = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
