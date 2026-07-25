package icloud

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestIncrementSequenceRejectsOverflow(t *testing.T) {
	event := ical.NewEvent()
	sequence := ical.NewProp(ical.PropSequence)
	sequence.Value = "2147483647"
	event.Props.Set(sequence)

	err := incrementSequence(event)
	if typed := AsICloudError(err); typed == nil || typed.Code != CodeProtocolError {
		t.Fatalf("error = %v, want protocol_error", err)
	}
	if got := event.Props.Get(ical.PropSequence).Value; got != "2147483647" {
		t.Fatalf("sequence changed to %q after rejected increment", got)
	}
}

func TestRetryDelayRejectsDeltaSecondsOverflow(t *testing.T) {
	response := &http.Response{Header: make(http.Header)}
	response.Header.Set("Retry-After", "9223372036854775807")
	if got := retryDelay(response, 0, time.Second, 10*time.Second, time.Now, func() float64 { return 0 }); got != 10*time.Second {
		t.Fatalf("retry delay = %v, want 10s cap", got)
	}
}

func TestICalDurationArithmeticIsChecked(t *testing.T) {
	valid, err := parseICalDuration("P1DT2H3M4S")
	if err != nil || valid != 26*time.Hour+3*time.Minute+4*time.Second {
		t.Fatalf("valid duration = %v, %v", valid, err)
	}
	for _, value := range []string{
		"P999999999999999999D",
		"PT999999999999999999H",
		"P106751DT23H47M17S",
	} {
		if _, err := parseICalDuration(value); err == nil {
			t.Errorf("parseICalDuration(%q) accepted overflow", value)
		}
	}
	if got := parseTriggerMinutesBefore("-PT999999999999999999H"); got != 0 {
		t.Fatalf("overflowing alarm trigger parsed as %d minutes", got)
	}
}

func TestRuntimeLimitsCountUTF8Bytes(t *testing.T) {
	exactTitle := strings.Repeat("\u00e9", MaxTitleLen/2)
	if len(exactTitle) != MaxTitleLen {
		t.Fatal("test title does not reach the byte boundary")
	}
	if err := ValidateTextField("title", exactTitle, MaxTitleLen); err != nil {
		t.Fatalf("exact byte boundary rejected: %v", err)
	}
	if err := ValidateTextField("title", exactTitle+"a", MaxTitleLen); err == nil {
		t.Fatal("multibyte title above byte boundary was accepted")
	}

	exactUID := strings.Repeat("\u00e9", 127) + "a"
	if len(exactUID) != MaxUIDLen {
		t.Fatal("test UID does not reach the byte boundary")
	}
	if err := ValidateUID(exactUID); err != nil {
		t.Fatalf("exact UID byte boundary rejected: %v", err)
	}
	if err := ValidateUID(exactUID + "b"); err == nil {
		t.Fatal("multibyte UID above byte boundary was accepted")
	}
}
