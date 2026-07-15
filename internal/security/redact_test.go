package security

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestRedactor_Redact(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		input   string
		want    string
	}{
		{
			name:    "simple secret present",
			secrets: []string{"SENTINEL-PW-abc123"},
			input:   "password: SENTINEL-PW-abc123 rejected", // gitleaks:allow, test sentinel, not a real secret
			want:    "password: [REDACTED] rejected",
		},
		{
			name:    "secret absent",
			secrets: []string{"SENTINEL-PW-abc123"},
			input:   "nothing to see here",
			want:    "nothing to see here",
		},
		{
			name:    "multiple secrets",
			secrets: []string{"pass1234", "user@example.com-secret"},
			input:   "pass1234 and user@example.com-secret in the same message",
			want:    "[REDACTED] and [REDACTED] in the same message",
		},
		{
			name:    "repeated secret",
			secrets: []string{"pass1234"},
			input:   "pass1234 pass1234 pass1234",
			want:    "[REDACTED] [REDACTED] [REDACTED]",
		},
		{
			name:    "too-short secret ignored",
			secrets: []string{"ab"},
			input:   "ab ab ab",
			want:    "ab ab ab",
		},
		{
			name:    "empty secret ignored",
			secrets: []string{""},
			input:   "normal text",
			want:    "normal text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRedactor(tt.secrets...)
			if got := r.Redact(tt.input); got != tt.want {
				t.Errorf("Redact() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedactingWriter_ThroughSlog(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	var buf bytes.Buffer
	r := NewRedactor(password)
	rw := NewRedactingWriter(&buf, r)

	logger := slog.New(slog.NewTextHandler(rw, nil))
	logger.Info("authentication failed", "err", "invalid password: "+password)

	out := buf.String()
	if strings.Contains(out, password) {
		t.Fatalf("the password was not redacted in the slog output: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected output containing [REDACTED], got: %q", out)
	}
}

func TestRedactingWriter_Base64AndURLEncodedForms(t *testing.T) {
	email := "user@example.com"
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	basicAuth := base64.StdEncoding.EncodeToString([]byte(email + ":" + password))
	urlEncoded := url.QueryEscape(password)

	r := NewRedactor(password, basicAuth, urlEncoded)
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, r)

	msg := "Authorization: Basic " + basicAuth + " ; redirected query with pw=" + urlEncoded
	if _, err := rw.Write([]byte(msg)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, password) {
		t.Errorf("raw password found in the output: %q", out)
	}
	if strings.Contains(out, basicAuth) {
		t.Errorf("base64 form found in the output: %q", out)
	}
	if strings.Contains(out, urlEncoded) && urlEncoded != password {
		t.Errorf("url-encoded form found in the output: %q", out)
	}
}

// TestRedactor_RedactsEmail: main.go now registers cfg.Email in the runtime
// Redactor alongside cfg.Password (neither the password nor the email may
// ever appear in any output). The generic multi-secret mechanism of Redactor
// already supported this (see "multiple secrets" above); this test isolates
// the email case specifically to document that intent.
func TestRedactor_RedactsEmail(t *testing.T) {
	email := "user@example.com"
	r := NewRedactor(email)
	out := r.Redact("authentication rejected for " + email)
	if strings.Contains(out, email) {
		t.Errorf("email not redacted in the output: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected output containing [REDACTED], got: %q", out)
	}
}

func TestRedactingWriter_ReturnsOriginalLength(t *testing.T) {
	r := NewRedactor("secretvalue")
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, r)

	p := []byte("contains secretvalue here")
	n, err := rw.Write(p)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p) {
		t.Errorf("Write() n = %d, want %d (original length)", n, len(p))
	}
}

// TestRedactingWriter_SecretSplitAcrossWrites: a secret fragmented across
// two Write calls must still be redacted (rolling buffer).
func TestRedactingWriter_SecretSplitAcrossWrites(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, NewRedactor(password))

	// Split in the middle of the password.
	mid := len(password) / 2
	part1 := "auth failed: " + password[:mid]
	part2 := password[mid:] + " retry\n"
	if _, err := rw.Write([]byte(part1)); err != nil {
		t.Fatalf("Write part1: %v", err)
	}
	// Before the second write the secret is incomplete: nothing should leak.
	if strings.Contains(buf.String(), password) {
		t.Fatalf("password leaked after partial write: %q", buf.String())
	}
	if _, err := rw.Write([]byte(part2)); err != nil {
		t.Fatalf("Write part2: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, password) {
		t.Fatalf("password not redacted across split writes: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output: %q", out)
	}
}

// TestRedactingWriter_SecretSplitAfterNewline: a logger that batches several
// records per Write can end a record with '\n' and then cut a secret mid-way.
// Emitting the whole buffer on any newline would reassemble the secret
// unredacted on the stream, so only complete lines may be emitted.
func TestRedactingWriter_SecretSplitAfterNewline(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, NewRedactor(password))

	mid := len(password) / 2
	if _, err := rw.Write([]byte("first record\nauth failed: " + password[:mid])); err != nil {
		t.Fatalf("Write part1: %v", err)
	}
	if !strings.Contains(buf.String(), "first record\n") {
		t.Errorf("complete line should be emitted immediately: %q", buf.String())
	}
	if _, err := rw.Write([]byte(password[mid:] + " retry\n")); err != nil {
		t.Fatalf("Write part2: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, password) {
		t.Fatalf("password reassembled unredacted across the newline flush: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output: %q", out)
	}
}

// TestRedactingWriter_NoNewlineDrainsAtCap: a stream that never emits a
// newline must not buffer without bound, yet must still keep enough of a tail
// to match a secret split across the drain.
func TestRedactingWriter_NoNewlineDrainsAtCap(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, NewRedactor(password))

	mid := len(password) / 2
	if _, err := rw.Write([]byte(strings.Repeat("x", maxRedactBuf) + password[:mid])); err != nil {
		t.Fatalf("Write filler: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("oversized buffer was not drained")
	}
	if _, err := rw.Write([]byte(password[mid:] + "\n")); err != nil {
		t.Fatalf("Write tail: %v", err)
	}
	if out := buf.String(); strings.Contains(out, password) {
		t.Errorf("password leaked across the oversize drain")
	}
}

func TestRedactingWriter_FlushEmitsBufferedTail(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, NewRedactor(password))
	// No newline and incomplete secret: stays buffered until Flush.
	if _, err := rw.Write([]byte("prefix " + password[:5])); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected buffer to hold incomplete data, got emitted %q", buf.String())
	}
	if err := rw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(buf.String(), "prefix") {
		t.Errorf("Flush did not emit buffered data: %q", buf.String())
	}
}
