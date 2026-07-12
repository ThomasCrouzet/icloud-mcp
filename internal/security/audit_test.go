package security

import (
	"bytes"
	"strings"
	"testing"
)

func TestAuditLogger_LogMutation(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)

	a.LogMutation("delete_event", "/123/calendars/ABC/", "uid-xyz", "success")

	out := buf.String()
	for _, want := range []string{
		"msg=audit",
		"tool=delete_event",
		"calendar=/123/calendars/ABC/",
		"uid=uid-xyz",
		"status=success",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected audit line containing %q, got: %q", want, out)
		}
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
	if strings.Contains(out, "title=") || strings.Contains(out, "location=") || strings.Contains(out, "notes=") {
		t.Errorf("the audit line must never contain title=/location=/notes=: %q", out)
	}
}

func TestAuditLogger_StatusValues(t *testing.T) {
	for _, status := range []string{"success", "error", "denied"} {
		var buf bytes.Buffer
		a := NewAuditLogger(&buf)
		a.LogMutation("update_event", "/cal/", "uid", status)
		if !strings.Contains(buf.String(), "status="+status) {
			t.Errorf("status %q missing from the audit line: %q", status, buf.String())
		}
	}
}
