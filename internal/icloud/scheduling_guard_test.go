package icloud

import (
	"context"
	"strings"
	"testing"
)

func schedulingFixture(uid string, scheduling string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//icloud-mcp//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\n" +
		"SUMMARY:Group event\r\n" +
		"DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\n" +
		"DTSTAMP:20260601T000000Z\r\n" +
		scheduling +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func newSchedulingObject(t *testing.T, uid, scheduling string) (*mockCalDAV, string) {
	t.Helper()
	m := newMockCalDAV(t)
	path := testHomeCalendar + uid + ".ics"
	m.objects[uid] = mockObject{path: path, ics: schedulingFixture(uid, scheduling)}
	m.etags[path] = `"v1"`
	return m, path
}

// TestGroupScheduledMutationDeniedByDefault covers organizer-only,
// attendee-only, and multiple attendees. No PUT or DELETE reaches the server.
func TestGroupScheduledMutationDeniedByDefault(t *testing.T) {
	for _, tt := range []struct {
		name       string
		scheduling string
	}{
		{name: "organizer only", scheduling: "ORGANIZER:mailto:owner@example.invalid\r\n"},
		{name: "attendee only", scheduling: "ATTENDEE:mailto:guest@example.invalid\r\n"},
		{name: "multiple attendees", scheduling: "ORGANIZER:mailto:owner@example.invalid\r\n" +
			"ATTENDEE:mailto:a@example.invalid\r\nATTENDEE:mailto:b@example.invalid\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			uid := "scheduled-" + strings.ReplaceAll(tt.name, " ", "-")
			m, _ := newSchedulingObject(t, uid, tt.scheduling)
			c := m.client()

			title := "Changed"
			err := c.UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
				Title:       &title,
				IfMatchETag: `"v1"`,
			})
			if err == nil {
				t.Fatal("unapproved update was accepted")
			}
			if ie := AsICloudError(err); ie == nil || ie.Code != CodeAuthorization {
				t.Fatalf("update error = %v, want authorization", err)
			}
			_, err = c.DeleteEvent(context.Background(), testHomeCalendar, uid, &DeleteOptions{
				IfMatchETag: `"v1"`,
			})
			if err == nil {
				t.Fatal("unapproved delete was accepted")
			}
			if ie := AsICloudError(err); ie == nil || ie.Code != CodeAuthorization {
				t.Fatalf("delete error = %v, want authorization", err)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(m.puts) != 0 || len(m.deletes) != 0 {
				t.Fatalf("mutation reached the server: puts=%d deletes=%d", len(m.puts), len(m.deletes))
			}
		})
	}
}

// TestGroupScheduledApprovalBoundToETag proves approval needs the reviewed
// strong ETag, and a changed object invalidates it.
func TestGroupScheduledApprovalBoundToETag(t *testing.T) {
	uid := "scheduled-approval"
	m, _ := newSchedulingObject(t, uid, "ORGANIZER:mailto:owner@example.invalid\r\n")
	c := m.client()
	title := "Changed"

	// Approved flag without an ETag is not enough.
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
		Title:               &title,
		AllowGroupScheduled: true,
	}); err == nil {
		t.Fatal("approval without an ETag was accepted")
	} else if ie := AsICloudError(err); ie == nil || ie.Code != CodeConcurrentModification {
		t.Fatalf("error = %v, want concurrent_modification", err)
	}

	// A stale reviewed ETag is rejected before dispatch.
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
		Title:               &title,
		IfMatchETag:         `"stale"`,
		AllowGroupScheduled: true,
	}); err == nil {
		t.Fatal("stale ETag approval was accepted")
	} else if ie := AsICloudError(err); ie == nil || ie.Code != CodeConcurrentModification {
		t.Fatalf("error = %v, want concurrent_modification", err)
	}
	m.mu.Lock()
	if len(m.puts) != 0 {
		m.mu.Unlock()
		t.Fatalf("stale approval sent %d PUT(s)", len(m.puts))
	}
	m.mu.Unlock()

	// The reviewed ETag and explicit approval allow the exact operation.
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
		Title:               &title,
		IfMatchETag:         `"v1"`,
		AllowGroupScheduled: true,
	}); err != nil {
		t.Fatalf("approved update: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(m.puts))
	}
}

// TestGroupScheduledDeleteDryRunReportsRisk proves a dry run neither mutates
// nor exposes raw addresses.
func TestGroupScheduledDeleteDryRunReportsRisk(t *testing.T) {
	uid := "scheduled-dry-run"
	m, _ := newSchedulingObject(t, uid, "ORGANIZER:mailto:owner@example.invalid\r\n"+
		"ATTENDEE:mailto:guest@example.invalid\r\n")
	c := m.client()
	res, err := c.DeleteEvent(context.Background(), testHomeCalendar, uid, &DeleteOptions{
		DryRun:      true,
		IfMatchETag: `"v1"`,
	})
	if err != nil {
		t.Fatalf("DeleteEvent dry run: %v", err)
	}
	if !res.DryRun || !res.IsGroupScheduled || !res.WouldMutate {
		t.Fatalf("dry-run result = %+v", res)
	}
	if res.NotificationRisk == "" || strings.Contains(res.NotificationRisk, "example.invalid") {
		t.Fatalf("risk warning = %q", res.NotificationRisk)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 0 || len(m.deletes) != 0 {
		t.Fatalf("dry run mutated: puts=%d deletes=%d", len(m.puts), len(m.deletes))
	}
}

// TestGroupScheduledDeleteApproved covers the approved delete path.
func TestGroupScheduledDeleteApproved(t *testing.T) {
	uid := "scheduled-delete"
	m, _ := newSchedulingObject(t, uid, "ATTENDEE:mailto:guest@example.invalid\r\n")
	res, err := m.client().DeleteEvent(context.Background(), testHomeCalendar, uid, &DeleteOptions{
		IfMatchETag:         `"v1"`,
		AllowGroupScheduled: true,
	})
	if err != nil {
		t.Fatalf("approved delete: %v", err)
	}
	if !res.IsGroupScheduled {
		t.Fatal("result lost the group-scheduled indicator")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deletes) != 1 {
		t.Fatalf("deletes = %d, want 1", len(m.deletes))
	}
}

// TestMalformedSchedulingMetadataFailsClosed covers an empty or URI-less
// cal-address. The object is rejected before any mutation.
func TestMalformedSchedulingMetadataFailsClosed(t *testing.T) {
	for _, value := range []string{"ORGANIZER:\r\n", "ATTENDEE:not-a-uri\r\n"} {
		uid := "scheduled-malformed"
		m, _ := newSchedulingObject(t, uid, value)
		err := m.client().UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
			Title:               ptrString("Changed"),
			IfMatchETag:         `"v1"`,
			AllowGroupScheduled: true,
		})
		if err == nil {
			t.Fatalf("malformed value %q was accepted", value)
		}
		if ie := AsICloudError(err); ie == nil || ie.Code != CodeProtocolError {
			t.Fatalf("error = %v, want protocol_error", err)
		}
		m.mu.Lock()
		if len(m.puts) != 0 {
			m.mu.Unlock()
			t.Fatalf("malformed object reached PUT")
		}
		m.mu.Unlock()
	}
}

// TestGetEventReportsGroupScheduledWithoutIdentity proves the read marker is
// non-sensitive.
func TestGetEventReportsGroupScheduledWithoutIdentity(t *testing.T) {
	uid := "scheduled-read"
	m, _ := newSchedulingObject(t, uid, "ORGANIZER:mailto:owner@example.invalid\r\n"+
		"ATTENDEE:mailto:guest@example.invalid\r\n")
	detail, err := m.client().GetEvent(context.Background(), testHomeCalendar, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.IsGroupScheduled {
		t.Fatal("IsGroupScheduled is false")
	}
	if strings.Contains(detail.Title, "example.invalid") {
		t.Fatal("identity leaked into a normal field")
	}
}

func ptrString(value string) *string { return &value }
