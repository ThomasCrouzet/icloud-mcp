package security

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzRedactor(f *testing.F) {
	f.Add("hello SECRET-PASSWORD-xyz world", "SECRET-PASSWORD-xyz")
	f.Add("no secret here", "zzzz")
	f.Add("aaaaaaaa", "aaaa")
	f.Fuzz(func(t *testing.T, text, secret string) {
		if len(secret) < 4 {
			return
		}
		// Secrets that are substrings of the redaction token cannot be
		// fully eliminated without looping forever (e.g. "REDA").
		if strings.Contains(redactToken, secret) {
			return
		}
		r := NewRedactor(secret)
		out := r.Redact(text)
		if strings.Contains(out, secret) {
			t.Fatalf("secret leaked in output")
		}
	})
}

func FuzzRedactingWriter(f *testing.F) {
	f.Add([]byte("line with SECRET99\n"), "SECRET99")
	f.Add([]byte("SECR"), "SECRET99")
	f.Add(bytes.Repeat([]byte{'x'}, maxRedactBuf+1), "SECRET99")
	f.Fuzz(func(t *testing.T, chunk []byte, secret string) {
		if len(secret) < 4 {
			return
		}
		if strings.Contains(redactToken, secret) {
			return
		}
		if strings.ContainsAny(secret, "\n\r") {
			return
		}
		var buf bytes.Buffer
		r := NewRedactor(secret)
		if len(r.secrets) == 0 {
			return
		}
		w := NewRedactingWriter(&buf, r)
		_, _ = w.Write(chunk)
		if len(w.buf) >= maxRedactBuf {
			t.Fatalf("writer retained %d bytes, limit is %d", len(w.buf), maxRedactBuf)
		}
		_ = w.Flush()
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("secret leaked via writer")
		}
	})
}

func FuzzIsICloudHost(f *testing.F) {
	f.Add("caldav.icloud.com")
	f.Add("p12-caldav.icloud.com")
	f.Add("evil.com")
	f.Add("p12-caldav.icloud.com.evil.com")
	f.Add("CALDAV.ICLOUD.COM")
	f.Add("p1234-caldav.icloud.com")
	f.Fuzz(func(t *testing.T, host string) {
		_ = IsICloudHost(host)
	})
}
