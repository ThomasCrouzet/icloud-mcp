package security

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// CredentialPair identifies one protocol username/password pair whose raw and
// encoded forms must be redacted.
type CredentialPair struct {
	Username string
	Password string
}

// RedactionVariants builds a deduplicated set of raw, JSON-escaped, URL-escaped,
// Basic-auth, and SASL PLAIN variants for multiple independent credential pairs.
// The result is intended to be passed directly to NewRedactor.
func RedactionVariants(credentials ...CredentialPair) []string {
	seen := make(map[string]struct{})
	variants := make([]string, 0, len(credentials)*20)
	addRaw := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		variants = append(variants, value)
	}
	add := func(value string) {
		addRaw(value)
		encoded, err := json.Marshal(value)
		if err == nil && len(encoded) >= 2 {
			addRaw(string(encoded[1 : len(encoded)-1]))
		}
	}
	addEncodings := func(value string) {
		data := []byte(value)
		add(base64.StdEncoding.EncodeToString(data))
		add(base64.RawStdEncoding.EncodeToString(data))
		add(base64.URLEncoding.EncodeToString(data))
		add(base64.RawURLEncoding.EncodeToString(data))
	}

	for _, credential := range credentials {
		add(credential.Username)
		add(credential.Password)
		add(url.QueryEscape(credential.Username))
		add(url.PathEscape(credential.Username))
		add(url.QueryEscape(credential.Password))
		add(url.PathEscape(credential.Password))
		addEncodings(credential.Username)
		addEncodings(credential.Password)

		basic := credential.Username + ":" + credential.Password
		add(basic)
		addEncodings(basic)

		plain := "\x00" + credential.Username + "\x00" + credential.Password
		add(plain)
		addEncodings(plain)

		plainWithAuthzID := credential.Username + "\x00" + credential.Username + "\x00" + credential.Password
		add(plainWithAuthzID)
		addEncodings(plainWithAuthzID)
	}
	return variants
}

// Redactor replaces every registered secret with "[REDACTED]" in a string.
type Redactor struct {
	secrets []string
}

// NewRedactor builds a Redactor from the secrets to mask. Empty or too-short
// strings (fewer than 4 characters) are ignored: replacing them everywhere
// would produce unusable noise (replacing "" or "ab" would mask passages
// unrelated to any secret). Secrets containing newlines are also ignored:
// RedactingWriter emits complete lines and cannot mask a secret that spans
// line boundaries (app-specific passwords and emails never contain newlines;
// file:// secrets are TrimSpace'd at load). Accepted secrets are deduplicated
// and sorted by descending byte length so overlaps redact the complete value.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	seen := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		if len(s) < 4 {
			continue
		}
		if strings.ContainsAny(s, "\n\r") {
			continue
		}
		if _, duplicate := seen[s]; duplicate {
			continue
		}
		seen[s] = struct{}{}
		r.secrets = append(r.secrets, s)
	}
	sort.SliceStable(r.secrets, func(i, j int) bool {
		return len(r.secrets[i]) > len(r.secrets[j])
	})
	return r
}

// redactToken is the standard replacement for masked secrets. A shorter
// fallback is used when a secret itself occurs in the standard token.
const redactToken = "[REDACTED]"

const redactFallbackToken = "***"

// Redact replaces every registered secret with "[REDACTED]" in s.
// Replacement is applied to a fixed point (bounded iterations) so a secret
// that is re-formed across a previous replacement boundary (e.g. secret
// "]aaa" against text "]aaaaaa" producing "[REDACTED]aaa") is still masked.
func (r *Redactor) Redact(s string) string {
	out := s
	for i := 0; i < 32; i++ {
		prev := out
		for _, secret := range r.secrets {
			if secret == "" {
				continue
			}
			replacement := redactToken
			if strings.Contains(redactToken, secret) {
				replacement = redactFallbackToken
			}
			out = strings.ReplaceAll(out, secret, replacement)
		}
		if out == prev {
			break
		}
	}
	return out
}

// RedactingWriter wraps an io.Writer (typically stderr) and redacts secrets
// before forwarding. Bytes are buffered across Write calls so a secret split
// mid-stream is still masked: only complete lines are emitted, and the
// trailing partial line stays buffered until its line terminator arrives.
type RedactingWriter struct {
	w          io.Writer
	r          *Redactor
	mu         sync.Mutex
	buf        []byte
	discarding bool
}

// maxRedactBuf caps the buffer so a writer that never emits a newline cannot
// grow it without bound.
const maxRedactBuf = 64 << 10 // 64 KiB

const oversizedRedactMarker = "[REDACTED: oversized record truncated]\n"

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

	originalLen := len(p)
	for len(p) > 0 {
		if rw.discarding {
			newline := bytes.IndexByte(p, '\n')
			if newline < 0 {
				return originalLen, nil
			}
			rw.discarding = false
			p = p[newline+1:]
			continue
		}

		newline := bytes.IndexByte(p, '\n')
		if newline >= 0 {
			recordLen := len(rw.buf) + newline + 1
			if recordLen > maxRedactBuf {
				rw.buf = rw.buf[:0]
				if err := rw.emitOversizedMarkerLocked(); err != nil {
					return 0, err
				}
				p = p[newline+1:]
				continue
			}
			rw.buf = append(rw.buf, p[:newline+1]...)
			p = p[newline+1:]
			if err := rw.emitLocked(false); err != nil {
				return 0, err
			}
			continue
		}

		if len(p) >= maxRedactBuf-len(rw.buf) {
			rw.buf = rw.buf[:0]
			rw.discarding = true
			if err := rw.emitOversizedMarkerLocked(); err != nil {
				return 0, err
			}
			return originalLen, nil
		}
		rw.buf = append(rw.buf, p...)
		return originalLen, nil
	}
	return originalLen, nil
}

// emitLocked redacts and forwards the buffered bytes that are safe to emit.
//
// A secret never contains a newline, so it can never straddle a line
// terminator: everything up to and including the last '\n' is safe to emit,
// while the trailing partial line must stay buffered, since the next Write
// may complete a secret started at its end. Write handles oversized records
// separately and never lets this buffer reach maxRedactBuf without a newline.
//
// force drains everything, including the trailing partial line.
func (rw *RedactingWriter) emitLocked(force bool) error {
	if len(rw.buf) == 0 {
		return nil
	}

	cut := bytes.LastIndexByte(rw.buf, '\n') + 1 // 0 when there is no newline
	if force {
		cut = len(rw.buf)
	}
	if cut == 0 {
		return nil
	}

	redacted := rw.r.Redact(string(rw.buf[:cut]))
	rw.buf = append(rw.buf[:0], rw.buf[cut:]...)
	return rw.writeLocked([]byte(redacted))
}

func (rw *RedactingWriter) emitOversizedMarkerLocked() error {
	return rw.writeLocked([]byte(rw.r.Redact(oversizedRedactMarker)))
}

func (rw *RedactingWriter) writeLocked(data []byte) error {
	n, err := rw.w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// Flush writes any buffered bytes (redacted) to the underlying writer,
// including a trailing line with no terminator.
func (rw *RedactingWriter) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.discarding = false
	return rw.emitLocked(true)
}
