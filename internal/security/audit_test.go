package security

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAuditLogger_LogMutation(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)

	a.LogMutation("delete_event", "/123/calendars/ABC/", "uid-xyz", "success")

	out := buf.String()
	// One NDJSON line, parseable as JSON, with the required structured fields.
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatalf("audit line is not valid JSON: %v\n%s", err, out)
	}
	for _, want := range []string{"msg", "time", "level", "tool", "calendar", "uid", "status"} {
		if _, ok := entry[want]; !ok {
			t.Errorf("audit JSON missing key %q: %v", want, entry)
		}
	}
	if entry["msg"] != "audit" {
		t.Errorf("msg = %v, want %q", entry["msg"], "audit")
	}
	if entry["tool"] != "delete_event" {
		t.Errorf("tool = %v, want delete_event", entry["tool"])
	}
	if entry["calendar"] != "/123/calendars/ABC/" {
		t.Errorf("calendar = %v", entry["calendar"])
	}
	if entry["uid"] != "uid-xyz" {
		t.Errorf("uid = %v", entry["uid"])
	}
	if entry["status"] != "success" {
		t.Errorf("status = %v", entry["status"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
}

func TestAuditLogger_NeverLogsTitleEvenIfPassedByMistake(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)

	// LogMutation only accepts tool/calendar/uid/status: there is
	// structurally no parameter for a title. We verify that no value looking
	// like an event title leaks; if a future developer mistakenly passed a
	// UID containing a title, the field would still be named "uid" in the
	// output, which keeps the anomaly auditable.
	a.LogMutation("create_event", "/123/calendars/ABC/", "uid-1", "denied")

	out := buf.String()
	if strings.Contains(out, "title") || strings.Contains(out, "location") || strings.Contains(out, "notes") {
		t.Errorf("the audit line must never contain title/location/notes keys: %q", out)
	}
}

func TestAuditLogger_StatusValues(t *testing.T) {
	for _, status := range []string{"success", "error", "denied"} {
		var buf bytes.Buffer
		a := NewAuditLogger(&buf)
		a.LogMutation("update_event", "/cal/", "uid", status)
		var entry map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
			t.Fatalf("status %q: audit line not JSON: %v", status, err)
		}
		if entry["status"] != status {
			t.Errorf("status %q: got %v", status, entry["status"])
		}
	}
}
