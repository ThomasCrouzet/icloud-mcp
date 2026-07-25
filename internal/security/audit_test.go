package security

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAuditLogger_LogDomainMutationWritesOpaqueUnifiedRecord(t *testing.T) {
	const resource = "/123/calendars/ABC/\x00uid-xyz"
	var buf bytes.Buffer
	NewAuditLogger(&buf).LogDomainMutation("delete_event", "calendar", "event", resource, "success")

	out := buf.String()
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatalf("audit line is not valid JSON: %v\n%s", err, out)
	}
	for _, want := range []string{"msg", "time", "level", "tool", "domain", "resourceType", "resourceToken", "status"} {
		if _, ok := entry[want]; !ok {
			t.Errorf("audit JSON missing key %q: %v", want, entry)
		}
	}
	if entry["msg"] != "audit" || entry["level"] != "INFO" {
		t.Errorf("audit metadata = msg %v, level %v", entry["msg"], entry["level"])
	}
	if entry["tool"] != "delete_event" || entry["domain"] != "calendar" || entry["resourceType"] != "event" {
		t.Errorf("resource metadata = %v/%v/%v", entry["tool"], entry["domain"], entry["resourceType"])
	}
	if entry["status"] != "success" {
		t.Errorf("status = %v, want success", entry["status"])
	}
	if token, ok := entry["resourceToken"].(string); !ok || len(token) != 64 {
		t.Errorf("resourceToken = %v, want 64-character HMAC-SHA256 hex", entry["resourceToken"])
	}
	for _, raw := range []string{resource, "/123/calendars/ABC/", "uid-xyz"} {
		if strings.Contains(out, raw) {
			t.Errorf("raw resource value %q leaked into audit: %q", raw, out)
		}
	}
	for _, forbidden := range []string{"calendar", "uid", "title", "location", "notes"} {
		if _, exists := entry[forbidden]; exists {
			t.Errorf("audit contains raw resource field %q: %v", forbidden, entry)
		}
	}
}

func TestAuditLogger_StatusValues(t *testing.T) {
	for _, status := range []string{"success", "error", "denied", "dry_run", "outcome_unknown"} {
		var buf bytes.Buffer
		NewAuditLogger(&buf).LogDomainMutation("update_event", "calendar", "event", "/cal/\x00uid", status)
		var entry map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
			t.Fatalf("status %q: audit line not JSON: %v", status, err)
		}
		if entry["status"] != status {
			t.Errorf("status %q: got %v", status, entry["status"])
		}
	}
}

func TestAuditLogger_LogDomainMutationUsesStableToken(t *testing.T) {
	const resource = "/private/address-books/family/contact-name-sentinel.vcf"
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)
	a.LogDomainMutation("update_contact", "contacts", "contact", resource, "success")
	a.LogDomainMutation("update_contact", "contacts", "contact", resource, "success")

	if strings.Contains(buf.String(), resource) || strings.Contains(buf.String(), "contact-name-sentinel") {
		t.Fatalf("raw resource leaked into audit: %q", buf.String())
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit line count = %d", len(lines))
	}
	entries := make([]map[string]any, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &entries[i]); err != nil {
			t.Fatalf("audit line %d is not JSON: %v", i, err)
		}
		for _, field := range []string{"msg", "tool", "domain", "resourceType", "resourceToken", "status"} {
			if _, ok := entries[i][field]; !ok {
				t.Errorf("audit JSON missing %q: %v", field, entries[i])
			}
		}
		if _, exists := entries[i]["calendar"]; exists {
			t.Errorf("unified audit contains raw calendar field")
		}
		if _, exists := entries[i]["uid"]; exists {
			t.Errorf("unified audit contains raw UID field")
		}
	}
	if entries[0]["resourceToken"] != entries[1]["resourceToken"] {
		t.Error("same process/resource did not produce a stable token")
	}
}

func TestAuditLogger_LogDomainMutationSeparatesResources(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)
	a.LogDomainMutation("move_message", "mail", "message", "mailbox-a/42", "outcome_unknown")
	a.LogDomainMutation("move_message", "mail", "message", "mailbox-a/43", "outcome_unknown")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first["resourceToken"] == second["resourceToken"] {
		t.Error("distinct resources produced the same token")
	}
	if first["status"] != "outcome_unknown" {
		t.Errorf("status = %v", first["status"])
	}
}

func TestAuditLogger_LogDomainMutationSanitizesMetadata(t *testing.T) {
	const sentinel = "private-address-sentinel@example.com"
	var buf bytes.Buffer
	NewAuditLogger(&buf).LogDomainMutation(sentinel, sentinel, sentinel, sentinel, sentinel)
	if strings.Contains(buf.String(), sentinel) {
		t.Fatalf("untrusted metadata leaked into audit: %q", buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["tool"] != "unknown" || entry["domain"] != "unknown" || entry["resourceType"] != "unknown" {
		t.Errorf("invalid audit labels were not sanitized: %v", entry)
	}
	if entry["status"] != "error" {
		t.Errorf("invalid status = %v, want error", entry["status"])
	}
}
