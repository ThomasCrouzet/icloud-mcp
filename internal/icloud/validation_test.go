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
		{"valid", "/121234567/calendars/home/", false},
		{"empty", "", true},
		{"missing leading slash", "121234567/calendars/home/", true},
		{"directory traversal", "/121234567/../etc/passwd", true},
		{"NUL", "/cal\x00endar/", true},
		{"CRLF injection", "/calendar/\r\nDELETE-ALL", true},
		{"too long", "/" + strings.Repeat("a", 1025), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCalendarPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCalendarPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
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
		{"simple valid", "abcdef0123456789@icloud-mcp", false},
		{"empty", "", true},
		{"too long", strings.Repeat("a", 256), true},
		{"directory traversal", "../../etc/passwd", true},
		{"slash", "abc/def", true},
		{"percent (suspicious encoding)", "abc%2Fdef", true},
		{"NUL", "abc\x00def", true},
		{"carriage return", "abc\rdef", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUID(tt.uid)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUID(%q) error = %v, wantErr %v", tt.uid, err, tt.wantErr)
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
		{"empty", "", 10, false},
		{"within limit", "abcde", 10, false},
		{"exactly at limit", "abcdefghij", 10, false},
		{"too long", "abcdefghijk", 10, true},
		{"newline tolerated", "line1\nline2", 20, false},
		{"NUL rejected", "abc\x00def", 20, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTextField("field", tt.value, tt.max)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTextField(%q, max=%d) error = %v, wantErr %v", tt.value, tt.max, err, tt.wantErr)
			}
		})
	}
}

func TestParseDateTime_ExplicitOffset(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valid UTC", "2026-07-01T00:00:00Z", false},
		{"valid with offset", "2026-07-01T00:00:00+02:00", false},
		{"date-only format", "2026-07-01", true},
		{"empty", "", true},
		{"free text", "tomorrow morning", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDateTime("start", tt.value, time.UTC)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDateTime(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

// TestParseDateTime_ExplicitOffsetAlwaysHonoredLiterally locks in that an
// explicit offset is NEVER reinterpreted through defaultLoc, regardless of
// what defaultLoc is: it is a deliberate, self-declared choice by the
// caller (see ParseDateTime's doc comment).
func TestParseDateTime_ExplicitOffsetAlwaysHonoredLiterally(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("LoadLocation(Europe/Paris): %v", err)
	}
	got, err := ParseDateTime("start", "2026-07-12T10:00:00Z", paris)
	if err != nil {
		t.Fatalf("ParseDateTime: %v", err)
	}
	want := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (an explicit Z must mean UTC even with a non-UTC defaultLoc)", got, want)
	}
}

// TestParseDateTime_NaiveLocalUsesDefaultLocation is the regression lock for
// the 2026-07-12 "Grand ménage" incident: the user confirmed "10h à 14h"
// (Europe/Paris, CEST = UTC+2), but the calling agent sent the offset-bearing
// form "2026-07-12T10:00:00Z", which iCloud rendered as 12h, 2h late. The
// fix is the no-offset local-time form: given the SAME literal hour the
// agent should now send ("2026-07-12T10:00:00", no "Z"), the server must
// itself resolve it against defaultLoc (Europe/Paris) to the correct UTC
// instant (08:00 UTC), instead of requiring the agent to do that
// DST-arithmetic itself.
func TestParseDateTime_NaiveLocalUsesDefaultLocation(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("LoadLocation(Europe/Paris): %v", err)
	}

	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{
			name:  "CEST (summer, UTC+2) - the Grand menage incident",
			value: "2026-07-12T10:00:00",
			want:  time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC),
		},
		{
			name:  "CET (winter, UTC+1)",
			value: "2026-01-12T10:00:00",
			want:  time.Date(2026, 1, 12, 9, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDateTime("start", tt.value, paris)
			if err != nil {
				t.Fatalf("ParseDateTime(%q): %v", tt.value, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("ParseDateTime(%q) = %v (%v UTC), want %v UTC", tt.value, got, got.UTC(), tt.want)
			}
		})
	}
}

// TestParseDateTime_NaiveLocalDefaultsToUTCWhenLocationNil covers the
// defensive nil-safety of ParseDateTime: a caller that fails to wire the
// configured location must not panic or silently misbehave, it must fall
// back to the previous strict-UTC behavior.
func TestParseDateTime_NaiveLocalDefaultsToUTCWhenLocationNil(t *testing.T) {
	got, err := ParseDateTime("start", "2026-07-12T10:00:00", nil)
	if err != nil {
		t.Fatalf("ParseDateTime: %v", err)
	}
	want := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
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
		{"valid 1 week", base, base.AddDate(0, 0, 7), false},
		{"start == end", base, base, true},
		{"end before start", base, base.AddDate(0, 0, -1), true},
		{"exactly 366 days", base, base.AddDate(0, 0, 366), false},
		{"367 days (too many)", base, base.AddDate(0, 0, 367), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRange(tt.start, tt.end)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRange() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
