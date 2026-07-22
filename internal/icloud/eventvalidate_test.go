package icloud

import (
	"testing"
	"time"
)

func TestValidateEventInput_OK(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	res := ValidateEventInput(&EventInput{
		Title: "Meet", StartTime: start, EndTime: end, Status: "CONFIRMED",
	}, time.UTC)
	if !res.OK {
		t.Fatalf("errors: %v", res.Errors)
	}
	if res.Normalized == nil || res.Normalized.Title != "Meet" {
		t.Fatalf("normalized: %+v", res.Normalized)
	}
}

func TestValidateEventInput_RejectsBadStatusAndURL(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	res := ValidateEventInput(&EventInput{
		Title: "x", StartTime: start, EndTime: start.Add(time.Hour),
		Status: "NOPE", URL: "ftp://evil",
	}, time.UTC)
	if res.OK {
		t.Fatal("expected validation failure")
	}
	if len(res.Errors) < 2 {
		t.Errorf("want >=2 errors, got %v", res.Errors)
	}
}

func TestValidateEventInput_StructuredRecurrence(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	res := ValidateEventInput(&EventInput{
		Title: "Weekly", StartTime: start, EndTime: start.Add(time.Hour),
		Structured: &StructuredRecurrence{Frequency: "weekly", Interval: 1, Count: 5, ByDay: []string{"MO"}},
	}, time.UTC)
	if !res.OK {
		t.Fatalf("errors: %v", res.Errors)
	}
	if res.Normalized.Recurrence == "" || res.Normalized.Recurrence[:4] != "FREQ" {
		t.Errorf("rrule = %q", res.Normalized.Recurrence)
	}
}

func TestValidateEventInput_ClientUID(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	res := ValidateEventInput(&EventInput{
		Title: "x", StartTime: start, EndTime: start.Add(time.Hour),
		ClientUID: "../evil",
	}, time.UTC)
	if res.OK {
		t.Fatal("expected client uid rejection")
	}
}

func TestValidateEventUpdateFields_EmptyClearsAndValid(t *testing.T) {
	empty := ""
	confirmed := "confirmed" // case-insensitive
	opaque := "OPAQUE"
	goodURL := "https://example.com/meet"
	up := &EventUpdate{
		Status:       &empty,
		Transparency: &opaque,
		URL:          &goodURL,
	}
	if err := ValidateEventUpdateFields(up); err != nil {
		t.Fatalf("valid clear/set: %v", err)
	}
	up.Status = &confirmed
	if err := ValidateEventUpdateFields(up); err != nil {
		t.Fatalf("case-insensitive status: %v", err)
	}
	NormalizeEventUpdateFields(up)
	if *up.Status != "CONFIRMED" {
		t.Errorf("normalized status = %q", *up.Status)
	}
	// Empty URL clears without validation error.
	up.URL = &empty
	if err := ValidateEventUpdateFields(up); err != nil {
		t.Fatalf("empty url clear: %v", err)
	}
}

func TestValidateEventUpdateFields_RejectsInvalid(t *testing.T) {
	badStatus := "MAYBE"
	badTransp := "BUSY"
	ftp := "ftp://evil.example/x"
	js := "javascript:alert(1)"
	noHost := "https://"
	for _, tc := range []struct {
		name string
		up   EventUpdate
	}{
		{"status", EventUpdate{Status: &badStatus}},
		{"transparency", EventUpdate{Transparency: &badTransp}},
		{"ftp", EventUpdate{URL: &ftp}},
		{"javascript", EventUpdate{URL: &js}},
		{"no-host", EventUpdate{URL: &noHost}},
	} {
		if err := ValidateEventUpdateFields(&tc.up); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
	if err := ValidateEventUpdateFields(nil); err == nil {
		t.Error("nil update should error")
	}
}
