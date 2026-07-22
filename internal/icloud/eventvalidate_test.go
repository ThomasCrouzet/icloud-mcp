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
