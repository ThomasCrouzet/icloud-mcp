package icloud

import (
	"context"
	"testing"
	"time"
)

// recurringExcludedFixture is a weekly UTC series with one EXDATE. It is the
// ordinary (non-scheduled) recurrence fixture of the #220 matrix.
const recurringExcludedFixture = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-recur-excluded\r\n" +
	"SUMMARY:Standup\r\n" +
	"DTSTART:20260706T090000Z\r\n" +
	"DTEND:20260706T091500Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=5\r\n" +
	"EXDATE:20260713T090000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// recurringExcludedScheduledFixture adds ORGANIZER and ATTENDEE metadata to
// the same shape, so the dry-run also proves no invitation path is taken.
const recurringExcludedScheduledFixture = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-recur-excluded-scheduled\r\n" +
	"SUMMARY:Group standup\r\n" +
	"DTSTART:20260706T090000Z\r\n" +
	"DTEND:20260706T091500Z\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=5\r\n" +
	"EXDATE:20260713T090000Z\r\n" +
	"ORGANIZER:mailto:owner@example.invalid\r\n" +
	"ATTENDEE:mailto:guest@example.invalid\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// excludedOccurrence is the EXDATE slot of both fixtures.
var excludedOccurrence = time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)

// deleteExcludedFixture registers one fixture and runs an occurrence dry-run.
func deleteExcludedFixture(t *testing.T, uid, fixture, etag string, opts *DeleteOptions) (*mockCalDAV, DeleteResult, error) {
	t.Helper()
	m := newMockCalDAV(t)
	path := testHomeCalendar + uid + ".ics"
	m.objects[uid] = mockObject{path: path, ics: fixture}
	m.etags[path] = etag
	recID := excludedOccurrence
	if opts == nil {
		opts = &DeleteOptions{}
	}
	opts.Scope = ScopeOccurrence
	opts.RecurrenceID = &recID
	opts.DryRun = true
	result, err := m.client().DeleteEvent(context.Background(), testHomeCalendar, uid, opts)
	return m, result, err
}

func requireNoMutation(t *testing.T, m *mockCalDAV) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 0 || len(m.deletes) != 0 {
		t.Fatalf("dry run mutated: puts=%d deletes=%d", len(m.puts), len(m.deletes))
	}
}

// TestDeleteEventAlreadyExcludedDryRunEnforcesCallerETag is the #220 defect:
// an already-excluded occurrence returned a success dry-run even when the
// caller ETag was stale.
func TestDeleteEventAlreadyExcludedDryRunEnforcesCallerETag(t *testing.T) {
	for _, test := range []struct {
		name    string
		uid     string
		fixture string
		group   bool
	}{
		{name: "ordinary", uid: "uid-recur-excluded", fixture: recurringExcludedFixture},
		{name: "scheduled", uid: "uid-recur-excluded-scheduled", fixture: recurringExcludedScheduledFixture, group: true},
	} {
		t.Run(test.name+"/stale etag", func(t *testing.T) {
			opts := &DeleteOptions{IfMatchETag: `"v1"`, AllowGroupScheduled: test.group}
			m, _, err := deleteExcludedFixture(t, test.uid, test.fixture, `"v2"`, opts)
			typed := AsICloudError(err)
			if typed == nil || typed.Code != CodeConcurrentModification {
				t.Fatalf("stale ETag on an excluded occurrence = %v, want concurrent_modification", err)
			}
			requireNoMutation(t, m)
		})

		t.Run(test.name+"/matching etag", func(t *testing.T) {
			opts := &DeleteOptions{IfMatchETag: `"v2"`, AllowGroupScheduled: test.group}
			m, result, err := deleteExcludedFixture(t, test.uid, test.fixture, `"v2"`, opts)
			if err != nil {
				t.Fatalf("matching ETag on an excluded occurrence = %v", err)
			}
			if !result.DryRun || !result.AlreadyExcluded || result.WouldMutate {
				t.Fatalf("matching dry run = %#v, want dryRun+alreadyExcluded and no mutation", result)
			}
			if test.group != result.IsGroupScheduled {
				t.Fatalf("isGroupScheduled = %v, want %v", result.IsGroupScheduled, test.group)
			}
			requireNoMutation(t, m)
		})

		t.Run(test.name+"/no caller etag keeps documented behavior", func(t *testing.T) {
			// Without a caller ETag the dry run stays a point-in-time
			// preflight: it reports the no-op instead of failing.
			opts := &DeleteOptions{AllowGroupScheduled: test.group}
			m, result, err := deleteExcludedFixture(t, test.uid, test.fixture, `"v2"`, opts)
			if err != nil {
				t.Fatalf("no caller ETag on an excluded occurrence = %v", err)
			}
			if !result.DryRun || !result.AlreadyExcluded || result.WouldMutate {
				t.Fatalf("no-ETag dry run = %#v", result)
			}
			requireNoMutation(t, m)
		})
	}
}

// TestDeleteEventAlreadyExcludedServerETagForms pins the safe handling of a
// weak, absent, or unquoted server ETag against a supplied strong caller ETag.
func TestDeleteEventAlreadyExcludedServerETagForms(t *testing.T) {
	for _, test := range []struct {
		name       string
		serverETag string
		wantCode   Code
	}{
		{name: "missing server etag", serverETag: "", wantCode: CodeConcurrentModification},
		{name: "different strong server etag", serverETag: `"v9"`, wantCode: CodeConcurrentModification},
		{name: "matching strong server etag", serverETag: `"v2"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := &DeleteOptions{IfMatchETag: `"v2"`}
			m, result, err := deleteExcludedFixture(t, "uid-recur-excluded", recurringExcludedFixture, test.serverETag, opts)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("matching server ETag = %v", err)
				}
				if !result.AlreadyExcluded || result.WouldMutate {
					t.Fatalf("result = %#v", result)
				}
			} else {
				typed := AsICloudError(err)
				if typed == nil || typed.Code != test.wantCode {
					t.Fatalf("error = %v, want %s", err, test.wantCode)
				}
			}
			requireNoMutation(t, m)
		})
	}
}

// TestDeleteEventDryRunRetainsActionablePaths keeps the #177 behavior: a
// series dry run and an actionable occurrence dry run still compare the ETag
// and stay mutation-free.
func TestDeleteEventDryRunRetainsActionablePaths(t *testing.T) {
	t.Run("series stale etag", func(t *testing.T) {
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-recur-excluded.ics"
		m.objects["uid-recur-excluded"] = mockObject{path: path, ics: recurringExcludedFixture}
		m.etags[path] = `"v2"`
		_, err := m.client().DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-excluded",
			&DeleteOptions{DryRun: true, IfMatchETag: `"v1"`})
		if typed := AsICloudError(err); typed == nil || typed.Code != CodeConcurrentModification {
			t.Fatalf("series dry run with a stale ETag = %v", err)
		}
		requireNoMutation(t, m)
	})

	t.Run("actionable occurrence matching etag", func(t *testing.T) {
		m := newMockCalDAV(t)
		path := testHomeCalendar + "uid-recur-excluded.ics"
		m.objects["uid-recur-excluded"] = mockObject{path: path, ics: recurringExcludedFixture}
		m.etags[path] = `"v2"`
		recID := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
		result, err := m.client().DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-excluded",
			&DeleteOptions{Scope: ScopeOccurrence, RecurrenceID: &recID, DryRun: true, IfMatchETag: `"v2"`})
		if err != nil {
			t.Fatalf("actionable occurrence dry run = %v", err)
		}
		if !result.DryRun || !result.WouldMutate || result.AlreadyExcluded {
			t.Fatalf("actionable dry run = %#v", result)
		}
		requireNoMutation(t, m)
	})
}

// TestDeleteEventExcludedOccurrenceWithoutDryRunStillNoOp keeps the existing
// non-dry-run no-op contract for an already-excluded occurrence.
func TestDeleteEventExcludedOccurrenceWithoutDryRunStillNoOp(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-recur-excluded.ics"
	m.objects["uid-recur-excluded"] = mockObject{path: path, ics: recurringExcludedFixture}
	m.etags[path] = `"v2"`
	recID := excludedOccurrence
	result, err := m.client().DeleteEvent(context.Background(), testHomeCalendar, "uid-recur-excluded",
		&DeleteOptions{Scope: ScopeOccurrence, RecurrenceID: &recID})
	if err != nil {
		t.Fatalf("excluded occurrence delete = %v", err)
	}
	if !result.AlreadyExcluded || result.WouldMutate || result.DryRun {
		t.Fatalf("no-op delete = %#v", result)
	}
	requireNoMutation(t, m)
}
