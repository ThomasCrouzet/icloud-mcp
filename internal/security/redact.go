package security

import (
	"bytes"
	"io"
	"strings"
	"sync"
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

// maxSecretLen returns the length of the longest registered secret.
func (r *Redactor) maxSecretLen() int {
	max := 0
	for _, s := range r.secrets {
		if len(s) > max {
			max = len(s)
		}
	}
	return max
}

// RedactingWriter wraps an io.Writer (typically stderr) and redacts secrets
// before forwarding. Bytes are buffered across Write calls so a secret split
// mid-stream is still masked: only complete lines are emitted, and the
// trailing partial line stays buffered until its line terminator arrives.
type RedactingWriter struct {
	w   io.Writer
	r   *Redactor
	mu  sync.Mutex
	buf []byte
}

// maxRedactBuf caps the buffer so a writer that never emits a newline cannot
// grow it without bound.
const maxRedactBuf = 64 << 10 // 64 KiB

// NewRedactingWriter builds a RedactingWriter.
func NewRedactingWriter(w io.Writer, r *Redactor) *RedactingWriter {
	return &RedactingWriter{w: w, r: r}
}

// Write implements io.Writer. It returns len(p) on success (not the length
// of the redacted text, which may differ): callers (slog, log.Logger) expect
// Write to consume the entire original buffer without a short-write error.
func (rw *RedactingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	rw.buf = append(rw.buf, p...)
	if err := rw.emitLocked(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// emitLocked redacts and forwards the buffered bytes that are safe to emit.
//
// A secret never contains a newline, so it can never straddle a line
// terminator: everything up to and including the last '\n' is safe to emit,
// while the trailing partial line must stay buffered, since the next Write
// may complete a secret started at its end. Without a newline the buffer is
// only drained once it exceeds maxRedactBuf, and even then the last
// maxSecretLen-1 bytes are retained so a split secret still matches.
//
// force drains everything, including the trailing partial line.
func (rw *RedactingWriter) emitLocked(force bool) error {
	if len(rw.buf) == 0 {
		return nil
	}

	cut := bytes.LastIndexByte(rw.buf, '\n') + 1 // 0 when there is no newline
	switch {
	case force:
		cut = len(rw.buf)
	case cut == 0 && len(rw.buf) >= maxRedactBuf:
		keep := rw.r.maxSecretLen() - 1
		if keep < 0 {
			keep = 0
		}
		if keep < len(rw.buf) {
			cut = len(rw.buf) - keep
		}
	}
	if cut == 0 {
		return nil
	}

	redacted := rw.r.Redact(string(rw.buf[:cut]))
	rw.buf = append(rw.buf[:0], rw.buf[cut:]...)
	_, err := rw.w.Write([]byte(redacted))
	return err
}

// Flush writes any buffered bytes (redacted) to the underlying writer,
// including a trailing line with no terminator.
func (rw *RedactingWriter) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.emitLocked(true)
}
