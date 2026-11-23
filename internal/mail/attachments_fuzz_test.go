package mail

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParsePartID checks that a part identifier is either rejected or maps to
// a bounded path of positive part numbers that formats back to the input.
func FuzzParsePartID(f *testing.F) {
	for _, seed := range []string{"1", "2", "1.2", "1.2.3", "0", "01", "1..2", ".1", "1.", "9999", "10000", "1.2.3.4.5.6.7.8.9.10.11.12.13.14.15.16.17.18.19.20.21", "-1", "1e3", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		path, err := parsePartID(value)
		if err != nil {
			return
		}
		if len(path) == 0 || len(path) > MaxMIMEDepth {
			t.Fatalf("path length %d for %q", len(path), value)
		}
		for _, number := range path {
			if number < 1 || number > MaxMIMEParts {
				t.Fatalf("part number %d for %q", number, value)
			}
		}
		if partID(path) != value {
			t.Fatalf("round trip %q -> %v -> %q", value, path, partID(path))
		}
	})
}

// FuzzDecodeTransferEncoding checks that untrusted MIME headers and bodies
// never panic and never decode beyond the limit.
func FuzzDecodeTransferEncoding(f *testing.F) {
	f.Add([]byte("Content-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n"), []byte("JVBERi0xLjQK\r\n"))
	f.Add([]byte("Content-Transfer-Encoding: quoted-printable\r\n"), []byte("caf=E9=\r\nau lait"))
	f.Add([]byte("Content-Transfer-Encoding: x-uuencode\r\n"), []byte("begin 644 a"))
	f.Add([]byte("Content-Type: text/plain; charset=\"iso-8859-1\"\r\n"), []byte("plain"))
	f.Add([]byte(""), []byte(""))
	f.Add([]byte("\x00: \r\n"), []byte("===="))
	f.Fuzz(func(t *testing.T, header, body []byte) {
		const limit = 64 * 1024
		decoded, _, err := decodeTransferEncoding(header, body, limit)
		if err != nil {
			return
		}
		if len(decoded) > limit {
			t.Fatalf("decoded %d bytes beyond the %d limit", len(decoded), limit)
		}
	})
}

// FuzzDocxText checks that arbitrary archive bytes and document XML never
// panic and that extracted text stays valid UTF-8 within the limit.
func FuzzDocxText(f *testing.F) {
	document := func(xml string) []byte {
		var buffer bytes.Buffer
		archive := zip.NewWriter(&buffer)
		file, _ := archive.Create("word/document.xml")
		_, _ = file.Write([]byte(xml))
		_ = archive.Close()
		return buffer.Bytes()
	}
	f.Add(document(`<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>Hello</w:t></w:r></w:p></w:body></w:document>`), 100)
	f.Add(document(`<w:document><w:p><w:t>a</w:t><w:tab/><w:t>b</w:t><w:br/></w:p>`), 3)
	f.Add(document(`<!DOCTYPE x [<!ENTITY a "aaaa">]><x>&a;</x>`), 50)
	f.Add([]byte("PK\x03\x04 not a zip"), 10)
	f.Add([]byte{}, 1)
	f.Fuzz(func(t *testing.T, data []byte, maxText int) {
		if maxText < 1 || maxText > MaxAttachmentTextBytes {
			return
		}
		text, _, err := docxText(data, maxText)
		if err != nil {
			return
		}
		if len(text) > maxText || !utf8.ValidString(text) {
			t.Fatalf("text of %d bytes, valid=%v, limit %d", len(text), utf8.ValidString(text), maxText)
		}
	})
}

// FuzzOutboundFilename checks that a received name always becomes a safe
// single path component.
func FuzzOutboundFilename(f *testing.F) {
	for _, seed := range []string{"report.pdf", "../../etc/passwd", "a\\b", "", ".", "..", " ", "a\x00b", "\xff\xfe", strings.Repeat("x", 400)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got := outboundFilename(name)
		if got == "" || got == "." || got == ".." || len(got) > MaxAttachmentFilename || !utf8.ValidString(got) ||
			strings.ContainsAny(got, "/\\") {
			t.Fatalf("unsafe filename %q from %q", got, name)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("control character in %q", got)
			}
		}
	})
}
