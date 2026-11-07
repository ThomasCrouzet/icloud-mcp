package mail

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIssue77RejectMalformedDiagnosticMetadata(t *testing.T) {
	for _, tc := range []struct{ stage, reason string }{
		{"private-stage", "eof"},
		{"tls", "private-reason"},
		{"authentication", "certificate_verification"},
		{"connect", "rejected"},
		{"tls", "dns"},
		{"", "eof"},
		{"tls", ""},
		{strings.Repeat("private-stage", 10000), strings.Repeat("private-reason", 10000)},
	} {
		err := &Error{Code: CodeUnavailable, Message: "mail service is temporarily unavailable", diagnosticStage: tc.stage, diagnosticReason: tc.reason}
		if stage, reason := err.Diagnostic(); stage != "" || reason != "" {
			t.Fatal("invalid diagnostic pair was not discarded")
		}
		data, marshalErr := json.Marshal(err)
		if marshalErr != nil || strings.Contains(string(data), "private-") || strings.Contains(string(data), "stage") || strings.Contains(string(data), "reason") {
			t.Fatal("private diagnostic metadata reached Mail JSON")
		}
	}
	var err Error
	if e := json.Unmarshal([]byte(`{"code":"unavailable","stage":"tls","reason":"private-reason"}`), &err); e != nil {
		t.Fatal(e)
	}
	if stage, reason := err.Diagnostic(); stage != "" || reason != "" {
		t.Fatal("input JSON injected diagnostic provenance")
	}
	var absent *Error
	if stage, reason := absent.Diagnostic(); stage != "" || reason != "" {
		t.Fatal("nil error reported a diagnostic")
	}
}
