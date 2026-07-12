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

func TestParseRFC3339(t *testing.T) {
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
			_, err := ParseRFC3339("start", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRFC3339(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
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
