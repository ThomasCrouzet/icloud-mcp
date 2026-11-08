package icloud

import (
	"strings"
	"testing"
	"time"

	extcaldav "github.com/emersion/go-webdav/caldav"
)

// TestExDateValueFormMustMatchDTSTART pins the RFC 5545 section 3.8.5.1 rule
// that EXDATE uses the DTSTART value type. A mismatch must fail closed before
// expansion; otherwise the exclusion silently has no effect.
func TestExDateValueFormMustMatchDTSTART(t *testing.T) {
	tests := []struct {
		name    string
		start   string
		end     string
		exdate  string
		wantErr bool
	}{
		{
			name:   "date with date",
			start:  "DTSTART;VALUE=DATE:20260328",
			end:    "DTEND;VALUE=DATE:20260329",
			exdate: "EXDATE;VALUE=DATE:20260329",
		},
		{
			name:   "utc date-time with utc date-time",
			start:  "DTSTART:20260701T100000Z",
			end:    "DTEND:20260701T110000Z",
			exdate: "EXDATE:20260702T100000Z",
		},
		{
			name:   "tzid date-time with same tzid",
			start:  "DTSTART;TZID=Europe/Paris:20260701T100000",
			end:    "DTEND;TZID=Europe/Paris:20260701T110000",
			exdate: "EXDATE;TZID=Europe/Paris:20260702T100000",
		},
		{
			name:   "floating date-time with floating date-time",
			start:  "DTSTART:20260701T100000",
			end:    "DTEND:20260701T110000",
			exdate: "EXDATE:20260702T100000",
		},
		{
			name:   "utc start with equivalent tzid exclusion",
			start:  "DTSTART:20260701T100000Z",
			end:    "DTEND:20260701T110000Z",
			exdate: "EXDATE;TZID=Europe/Paris:20260702T120000",
		},
		{
			name:   "tzid start with equivalent utc exclusion",
			start:  "DTSTART;TZID=Europe/Paris:20260701T100000",
			end:    "DTEND;TZID=Europe/Paris:20260701T110000",
			exdate: "EXDATE:20260702T080000Z",
		},
		{
			name:    "date start with date-time exclusion",
			start:   "DTSTART;VALUE=DATE:20260328",
			exdate:  "EXDATE:20260329T000000Z",
			wantErr: true,
		},
		{
			name:    "date-time start with date exclusion",
			start:   "DTSTART:20260701T100000Z",
			exdate:  "EXDATE;VALUE=DATE:20260702",
			wantErr: true,
		},
		{
			name:    "floating start with absolute exclusion",
			start:   "DTSTART:20260701T100000",
			exdate:  "EXDATE:20260702T100000Z",
			wantErr: true,
		},
		{
			name:    "absolute start with floating exclusion",
			start:   "DTSTART;TZID=Europe/Paris:20260701T100000",
			exdate:  "EXDATE:20260702T100000",
			wantErr: true,
		},
		{
			name:    "multi-value list mixes forms",
			start:   "DTSTART:20260701T100000Z",
			exdate:  "EXDATE:20260702T100000Z,20260703T100000",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			end := tt.end
			if end == "" {
				end = "DTEND:20260701T110000Z"
			}
			body := researchCalendarHead +
				"BEGIN:VEVENT\r\nUID:exdate-form\r\n" +
				tt.start + "\r\n" + end + "\r\n" +
				"RRULE:FREQ=DAILY;COUNT=4\r\n" +
				tt.exdate + "\r\n" +
				"END:VEVENT\r\nEND:VCALENDAR\r\n"

			cal, err := decodeRemoteCalendar(strings.NewReader(body))
			if err != nil {
				t.Fatalf("decodeRemoteCalendar: %v", err)
			}
			_, _, err = parseCalendarObject(&extcaldav.CalendarObject{Data: cal})
			if tt.wantErr {
				if err == nil {
					t.Fatal("mismatched EXDATE was accepted")
				}
				ie := AsICloudError(err)
				if ie == nil || ie.Code != CodeProtocolError {
					t.Fatalf("error = %v, want typed %s", err, CodeProtocolError)
				}
				return
			}
			if err != nil {
				t.Fatalf("compatible EXDATE was rejected: %v", err)
			}
		})
	}
}

// TestExDateFormKeepsMatchingOccurrences proves a compatible exclusion still
// removes exactly its own occurrence for UTC, TZID, and floating forms.
func TestExDateFormKeepsMatchingOccurrences(t *testing.T) {
	tests := []struct {
		name       string
		start      string
		end        string
		exdate     string
		excludedAt time.Time
	}{
		{
			name:       "utc",
			start:      "DTSTART:20260701T100000Z",
			end:        "DTEND:20260701T110000Z",
			exdate:     "EXDATE:20260702T100000Z",
			excludedAt: time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC),
		},
		{
			name:       "tzid",
			start:      "DTSTART;TZID=Europe/Paris:20260701T100000",
			end:        "DTEND;TZID=Europe/Paris:20260701T110000",
			exdate:     "EXDATE;TZID=Europe/Paris:20260702T100000",
			excludedAt: time.Date(2026, time.July, 2, 8, 0, 0, 0, time.UTC),
		},
		{
			name:       "floating",
			start:      "DTSTART:20260701T100000",
			end:        "DTEND:20260701T110000",
			exdate:     "EXDATE:20260702T100000",
			excludedAt: time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC),
		},
		{
			name:       "multi-value tzid",
			start:      "DTSTART;TZID=Europe/Paris:20260701T100000",
			end:        "DTEND;TZID=Europe/Paris:20260701T110000",
			exdate:     "EXDATE;TZID=Europe/Paris:20260702T100000,20260703T100000",
			excludedAt: time.Date(2026, time.July, 2, 8, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := researchCalendarHead +
				"BEGIN:VEVENT\r\nUID:exdate-match\r\n" +
				tt.start + "\r\n" + tt.end + "\r\n" +
				"RRULE:FREQ=DAILY;COUNT=4\r\n" +
				tt.exdate + "\r\n" +
				"END:VEVENT\r\nEND:VCALENDAR\r\n"

			events := researchExpand(t, body,
				time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC))
			for _, event := range events {
				if event.StartTime.Equal(tt.excludedAt) {
					t.Fatalf("excluded occurrence %v is present", tt.excludedAt)
				}
			}
		})
	}
}

// TestExDateFormMismatchBlocksAvailability proves a malformed exclusion cannot
// yield an apparently complete search result.
func TestExDateFormMismatchBlocksAvailability(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:exdate-availability\r\n" +
		"DTSTART;VALUE=DATE:20260328\r\n" +
		"DTEND;VALUE=DATE:20260329\r\n" +
		"RRULE:FREQ=DAILY;COUNT=4\r\n" +
		"EXDATE:20260329T000000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	cal, err := decodeRemoteCalendar(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decodeRemoteCalendar: %v", err)
	}
	if _, _, err := parseCalendarObject(&extcaldav.CalendarObject{Data: cal}); err == nil {
		t.Fatal("malformed exclusion produced a parseable event")
	}
}

// TestExDateFormIsValidatedPerComponent covers an override that carries a
// malformed exclusion. It must not be ignored because the master is valid.
func TestExDateFormIsValidatedPerComponent(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:exdate-override\r\n" +
		"DTSTART;VALUE=DATE:20260701\r\nDTEND;VALUE=DATE:20260702\r\n" +
		"RRULE:FREQ=DAILY;COUNT=3\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:exdate-override\r\n" +
		"RECURRENCE-ID;VALUE=DATE:20260702\r\n" +
		"DTSTART;VALUE=DATE:20260702\r\nDTEND;VALUE=DATE:20260703\r\n" +
		"EXDATE:20260703T000000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	cal, err := decodeRemoteCalendar(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decodeRemoteCalendar: %v", err)
	}
	if _, _, err := parseCalendarObject(&extcaldav.CalendarObject{Data: cal}); err == nil {
		t.Fatal("mismatched EXDATE on an override was accepted")
	}
}
