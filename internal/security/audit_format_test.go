package security

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAuditFormat(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    AuditFormat
		wantErr bool
	}{
		{"", AuditFormatJSON, false},
		{"json", AuditFormatJSON, false},
		{"JSON", AuditFormatJSON, false},
		{"text", AuditFormatText, false},
		{"TEXT", AuditFormatText, false},
		{"xml", "", true},
	} {
		got, err := ParseAuditFormat(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseAuditFormat(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAuditFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuditLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	NewAuditLoggerWithFormat(&buf, AuditFormatText).LogDomainMutation(
		"create_event", "calendar", "event", "/cal/\x00uid", "success",
	)
	out := buf.String()
	if !strings.Contains(out, "msg=audit") || !strings.Contains(out, "tool=create_event") {
		t.Fatalf("text audit = %q", out)
	}
	if strings.Contains(out, "/cal/") {
		t.Fatalf("raw path leaked: %q", out)
	}
	// Must not be JSON.
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &m) == nil {
		t.Fatalf("text format produced JSON: %q", out)
	}
}
