package icloud

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// requestCountingDoer counts each request and sends none. A write that
// validates its input first must leave the count at zero.
type requestCountingDoer struct{ requests int }

func (d *requestCountingDoer) Do(*http.Request) (*http.Response, error) {
	d.requests++
	return nil, errors.New("requestCountingDoer: unexpected request")
}

func newRequestCountingClient() (*Client, *requestCountingDoer) {
	doer := &requestCountingDoer{}
	return NewClient(doer, "https://caldav.icloud.com", func(string) bool { return true }), doer
}

func validNewEvent() *NewEvent {
	return &NewEvent{
		Title:     "Synthetic",
		StartTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}
}

// TestClientCreateEventValidationDoesNotRepeatInput covers the Client checks
// that the MCP input schema hides: an MCP enum refuses these values first.
func TestClientCreateEventValidationDoesNotRepeatInput(t *testing.T) {
	const marker = "ZQXMARKER"
	for _, test := range []struct {
		name, field string
		set         func(*NewEvent)
	}{
		{"status", "status", func(ne *NewEvent) { ne.Status = marker }},
		{"transparency", "transparency", func(ne *NewEvent) { ne.Transparency = marker }},
		{"timezone", "timezone", func(ne *NewEvent) { ne.Timezone = marker + "/Nowhere" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, doer := newRequestCountingClient()
			ne := validNewEvent()
			test.set(ne)
			_, err := client.CreateEvent(context.Background(), "/cal/home/", ne)
			if err == nil {
				t.Fatal("invalid value was accepted")
			}
			if typed := AsICloudError(err); typed == nil || typed.Code != CodeValidation {
				t.Fatalf("error = %v, want validation", err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error repeats the value: %v", err)
			}
			if !strings.Contains(err.Error(), test.field) {
				t.Fatalf("error does not name %s: %v", test.field, err)
			}
			if doer.requests != 0 {
				t.Fatalf("requests = %d, want 0", doer.requests)
			}
		})
	}
}
