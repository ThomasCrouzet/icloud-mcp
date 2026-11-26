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

// TestClientWritesRejectCRBeforeAnyRequest proves that Client.CreateEvent and
// Client.UpdateEvent apply the write text rule without the MCP handlers. A CR
// cannot be encoded, so it fails as validation before discovery. LF in notes
// passes the rule and reaches the first request.
func TestClientWritesRejectCRBeforeAnyRequest(t *testing.T) {
	writes := map[string]func(*Client, string, string) error{
		"create": func(c *Client, field, value string) error {
			ne := validNewEvent()
			switch field {
			case "title":
				ne.Title = value
			case "location":
				ne.Location = value
			case "notes":
				ne.Notes = value
			}
			_, err := c.CreateEvent(context.Background(), "/cal/home/", ne)
			return err
		},
		"update": func(c *Client, field, value string) error {
			up := &EventUpdate{}
			switch field {
			case "title":
				up.Title = &value
			case "location":
				up.Location = &value
			case "notes":
				up.Notes = &value
			}
			return c.UpdateEvent(context.Background(), "/cal/home/", "event-uid", up)
		},
	}
	for name, write := range writes {
		for _, field := range []string{"title", "location", "notes"} {
			t.Run(name+"/"+field+"/CR", func(t *testing.T) {
				client, doer := newRequestCountingClient()
				err := write(client, field, "first\rsecond")
				if typed := AsICloudError(err); typed == nil || typed.Code != CodeValidation {
					t.Fatalf("error = %v, want validation", err)
				}
				if !strings.Contains(err.Error(), field) || strings.Contains(err.Error(), "first") {
					t.Fatalf("error must name %s and repeat no value: %v", field, err)
				}
				if doer.requests != 0 {
					t.Fatalf("requests = %d, want 0", doer.requests)
				}
			})
		}
		t.Run(name+"/notes/LF", func(t *testing.T) {
			client, doer := newRequestCountingClient()
			err := write(client, "notes", "first\nsecond")
			if typed := AsICloudError(err); typed != nil && typed.Code == CodeValidation {
				t.Fatalf("LF in notes was rejected: %v", err)
			}
			if doer.requests == 0 {
				t.Fatal("LF in notes did not reach the first request")
			}
		})
	}
}
