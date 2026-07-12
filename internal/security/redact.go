package security

import (
	"io"
	"strings"
)

// Redactor replaces every registered secret with "[REDACTED]" in a string.
type Redactor struct {
	secrets []string
}

// NewRedactor builds a Redactor from the secrets to mask. Empty or too-short
// strings (fewer than 4 characters) are ignored: replacing them everywhere
// would produce unusable noise (replacing "" or "ab" would mask passages
// unrelated to any secret).
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) < 4 {
			continue
		}
		r.secrets = append(r.secrets, s)
	}
	return r
}

// Redact replaces every registered secret with "[REDACTED]" in s.
func (r *Redactor) Redact(s string) string {
	out := s
	for _, secret := range r.secrets {
		out = strings.ReplaceAll(out, secret, "[REDACTED]")
	}
	return out
}

// RedactingWriter wraps an io.Writer (typically stderr) and redacts every
// Write before forwarding it. Accepted limitation: a secret split across two
// Write calls would not be masked; in practice slog and the stdio
// transport's error logger emit each record in a single Write, so the case
// does not arise for our usages.
type RedactingWriter struct {
	w io.Writer
	r *Redactor
}

// NewRedactingWriter builds a RedactingWriter.
func NewRedactingWriter(w io.Writer, r *Redactor) *RedactingWriter {
	return &RedactingWriter{w: w, r: r}
}

// Write implements io.Writer. It returns len(p) on success (not the length
// of the redacted text, which may differ): callers (slog, log.Logger) expect
// Write to consume the entire original buffer without a short-write error.
func (rw *RedactingWriter) Write(p []byte) (int, error) {
	redacted := rw.r.Redact(string(p))
	if _, err := rw.w.Write([]byte(redacted)); err != nil {
		return 0, err
	}
	return len(p), nil
}
