package security

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

func TestNewRedactorDeduplicatesAndSortsLongestFirst(t *testing.T) {
	r := NewRedactor("shared-secret", "shared-secret-long", "shared-secret", "other")
	want := []string{"shared-secret-long", "shared-secret", "other"}
	if len(r.secrets) != len(want) {
		t.Fatalf("registered secrets = %q, want %q", r.secrets, want)
	}
	for i := range want {
		if r.secrets[i] != want[i] {
			t.Fatalf("registered secrets = %q, want %q", r.secrets, want)
		}
	}
	if got := r.Redact("value=shared-secret-long"); got != "value=[REDACTED]" {
		t.Fatalf("overlapping secret redaction = %q", got)
	}
}

func TestRedactorRetainsFixedPointReplacement(t *testing.T) {
	const secret = "]aaa"
	got := NewRedactor(secret).Redact("]aaaaaa")
	if strings.Contains(got, secret) {
		t.Fatalf("fixed-point redaction left reconstructed secret in %q", got)
	}
}

func TestRedactorMasksSecretsContainedInMarker(t *testing.T) {
	for _, secret := range []string{"REDACTED", "CTED", "[RED"} {
		output := NewRedactor(secret).Redact("credential=" + secret)
		if strings.Contains(output, secret) {
			t.Fatalf("marker-colliding secret %q remained in %q", secret, output)
		}
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
	basic := []byte(email + ":" + password)
	std := base64.StdEncoding.EncodeToString(basic)
	rawStd := base64.RawStdEncoding.EncodeToString(basic)
	urlB64 := base64.URLEncoding.EncodeToString(basic)
	rawURL := base64.RawURLEncoding.EncodeToString(basic)
	queryEscaped := url.QueryEscape(password)

	r := NewRedactor(password, std, rawStd, urlB64, rawURL, queryEscaped)
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, r)

	msg := "Authorization: Basic " + std +
		" raw=" + rawStd +
		" urlb64=" + urlB64 +
		" rawurl=" + rawURL +
		" ; redirected query with pw=" + queryEscaped
	if _, err := rw.Write([]byte(msg)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := buf.String()
	for _, leak := range []string{password, std, rawStd, urlB64, rawURL} {
		if strings.Contains(out, leak) {
			t.Errorf("secret form %q found in the output: %q", leak, out)
		}
	}
	if strings.Contains(out, queryEscaped) && queryEscaped != password {
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

func TestRedactionVariants_MultipleCredentialEncodings(t *testing.T) {
	pairs := []CredentialPair{
		{Username: "calendar@example.com", Password: "calendar-secret-sentinel"},
		{Username: "mailbox@example.com", Password: "mail-secret-sentinel"},
	}
	variants := RedactionVariants(pairs...)
	redactor := NewRedactor(variants...)

	var expected []string
	for _, pair := range pairs {
		basic := pair.Username + ":" + pair.Password
		plain := "\x00" + pair.Username + "\x00" + pair.Password
		plainWithAuthzID := pair.Username + "\x00" + pair.Username + "\x00" + pair.Password
		expected = append(expected,
			pair.Username,
			pair.Password,
			base64.StdEncoding.EncodeToString([]byte(pair.Username)),
			base64.StdEncoding.EncodeToString([]byte(pair.Password)),
			base64.StdEncoding.EncodeToString([]byte(basic)),
			base64.RawStdEncoding.EncodeToString([]byte(basic)),
			base64.URLEncoding.EncodeToString([]byte(basic)),
			base64.RawURLEncoding.EncodeToString([]byte(basic)),
			plain,
			base64.StdEncoding.EncodeToString([]byte(plain)),
			plainWithAuthzID,
			base64.StdEncoding.EncodeToString([]byte(plainWithAuthzID)),
		)
	}
	for _, encoded := range expected {
		if out := redactor.Redact("prefix:" + encoded + ":suffix"); strings.Contains(out, encoded) {
			t.Errorf("credential variant was not redacted: %q", encoded)
		}
	}
}

func TestRedactionVariants_MasksJSONEscapedCredentials(t *testing.T) {
	password := `app-"password\\with<chars>`
	variants := RedactionVariants(CredentialPair{Username: "user@example.com", Password: password})
	redactor := NewRedactor(variants...)
	encoded, err := json.Marshal(map[string]string{"error": password})
	if err != nil {
		t.Fatal(err)
	}
	output := redactor.Redact(string(encoded))
	if strings.Contains(output, `app-\"password`) || strings.Contains(output, `password\\with`) {
		t.Fatalf("JSON-escaped credential remained in output: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %s", output)
	}
}

func TestRedactionVariants_DeduplicatesSharedCredentials(t *testing.T) {
	variants := RedactionVariants(
		CredentialPair{Username: "same@example.com", Password: "same-secret"},
		CredentialPair{Username: "same@example.com", Password: "same-secret"},
	)
	seen := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		if _, duplicate := seen[variant]; duplicate {
			t.Fatalf("duplicate redaction variant %q", variant)
		}
		seen[variant] = struct{}{}
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

func TestRedactingWriter_SecretStraddlingExactCapIsDiscarded(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, test sentinel, not a real secret
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, NewRedactor(password))

	mid := len(password) / 2
	first := []byte(strings.Repeat("x", maxRedactBuf-mid) + password[:mid])
	if len(first) != maxRedactBuf {
		t.Fatalf("first write length = %d, want %d", len(first), maxRedactBuf)
	}
	n, err := rw.Write(first)
	if err != nil || n != len(first) {
		t.Fatalf("Write first = %d, %v", n, err)
	}
	second := []byte(password[mid:] + "\nnext record\n")
	n, err = rw.Write(second)
	if err != nil || n != len(second) {
		t.Fatalf("Write tail: %v", err)
	}
	if got, want := buf.String(), oversizedRedactMarker+"next record\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if len(rw.buf) >= maxRedactBuf {
		t.Fatalf("internal buffer length = %d, want less than %d", len(rw.buf), maxRedactBuf)
	}
}

func TestRedactingWriter_OversizedWriteHasBoundedStateAndOutput(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, NewRedactor("secretvalue"))
	p := []byte(strings.Repeat("x", maxRedactBuf*4) + "\nkept\n")
	n, err := rw.Write(p)
	if err != nil || n != len(p) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got, want := buf.String(), oversizedRedactMarker+"kept\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if len(rw.buf) >= maxRedactBuf {
		t.Fatalf("internal buffer length = %d, want less than %d", len(rw.buf), maxRedactBuf)
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
