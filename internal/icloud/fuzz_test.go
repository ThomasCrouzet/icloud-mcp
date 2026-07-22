package icloud

import (
	"strings"
	"testing"
	"time"
)

func FuzzValidateCalendarPath(f *testing.F) {
	f.Add("/calendars/123/home/")
	f.Add("")
	f.Add("/../etc/passwd")
	f.Add("/cal/\x00evil")
	f.Add(strings.Repeat("a", 2000))
	f.Fuzz(func(t *testing.T, path string) {
		_ = ValidateCalendarPath(path)
	})
}

func FuzzValidateUID(f *testing.F) {
	f.Add("abc@icloud-mcp")
	f.Add("")
	f.Add("../x")
	f.Add("uid/with/slash")
	f.Add(strings.Repeat("u", 300))
	f.Fuzz(func(t *testing.T, uid string) {
		_ = ValidateUID(uid)
	})
}

func FuzzParseDateTime(f *testing.F) {
	f.Add("2026-07-01T14:00:00Z")
	f.Add("2026-07-01T14:00:00")
	f.Add("2026-07-01T14:00:00+02:00")
	f.Add("not-a-date")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseDateTime("start", s, time.UTC)
	})
}

func FuzzValidateRRULE(f *testing.F) {
	f.Add("FREQ=WEEKLY;COUNT=10")
	f.Add("FREQ=MINUTELY")
	f.Add("RRULE:FREQ=DAILY")
	f.Add("")
	f.Add(strings.Repeat("FREQ=DAILY;", 100))
	f.Fuzz(func(t *testing.T, rule string) {
		_ = ValidateRRULE(rule)
	})
}

func FuzzRedactLikeEventRoundTrip(f *testing.F) {
	// Build/parse-ish: Ensure buildEventCalendar + property access does not panic.
	f.Add("Title", "Place", "Notes", "FREQ=DAILY;COUNT=2")
	f.Add("🎉 Unicode", "Café", "line1\nline2", "")
	f.Fuzz(func(t *testing.T, title, loc, notes, rrule string) {
		if len(title) > MaxTitleLen {
			title = title[:MaxTitleLen]
		}
		if len(loc) > MaxLocationLen {
			loc = loc[:MaxLocationLen]
		}
		if len(notes) > MaxNotesLen {
			notes = notes[:MaxNotesLen]
		}
		start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		ne := &NewEvent{
			Title: title, Location: loc, Notes: notes,
			StartTime: start, EndTime: start.Add(time.Hour),
			Recurrence: rrule,
		}
		if rrule != "" {
			if err := ValidateRRULE(rrule); err != nil {
				return
			}
		}
		if title == "" {
			return
		}
		cal := buildEventCalendar("fuzz-uid@icloud-mcp", ne)
		if cal == nil {
			t.Fatal("nil calendar")
		}
		master, err := findMasterVEvent(cal)
		if err != nil {
			t.Fatal(err)
		}
		_ = master
	})
}

func FuzzExpandOccurrences(f *testing.F) {
	f.Add("FREQ=DAILY;COUNT=5")
	f.Add("FREQ=WEEKLY;BYDAY=MO,WE;COUNT=10")
	f.Add("FREQ=MINUTELY;COUNT=100")
	f.Fuzz(func(t *testing.T, rule string) {
		if err := ValidateRRULE(rule); err != nil {
			return
		}
		start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
		master := Event{
			UID: "fuzz", Title: "t", StartTime: start, EndTime: start.Add(time.Hour),
			Recurrence: rule,
		}
		rangeEnd := start.Add(30 * 24 * time.Hour)
		evs, truncated, err := ExpandOccurrences(master, nil, start, rangeEnd, 100)
		if err != nil {
			return
		}
		if len(evs) > 100 {
			t.Fatalf("expanded %d > cap", len(evs))
		}
		_ = truncated
	})
}

func FuzzIsICloudHostViaPath(f *testing.F) {
	// Fuzz hrefPath which feeds into path handling after REPORT.
	f.Add("/calendars/1/home/uid.ics")
	f.Add("https://p12-caldav.icloud.com/calendars/1/")
	f.Add("https://evil.example/path")
	f.Add("")
	f.Fuzz(func(t *testing.T, href string) {
		p := hrefPath(href)
		if strings.Contains(p, "\x00") {
			// just ensure no panic
		}
		_ = p
	})
}
