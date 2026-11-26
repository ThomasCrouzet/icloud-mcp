package mail

import (
	"archive/zip"
	"bytes"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

// These protocol tests send hostile attachments through the production IMAP
// adapter and the in-memory server. Each decode limit must stop the work with
// payload_too_large and keep the allocation of the call bounded.

// allocatedDuring returns the bytes that the process allocated while fn ran.
// The total includes garbage, so it is an upper bound of the peak increase.
func allocatedDuring(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// fixtureDocxXML deflates the bytes from write as word/document.xml.
func fixtureDocxXML(t *testing.T, write func(io.Writer) error) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if err := write(file); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// getHostileAttachment stages one part and reads it with get_attachment. It
// returns the result, the bytes allocated by the call, and the error.
func getHostileAttachment(t *testing.T, name, contentType string, data []byte) (AttachmentResult, uint64, error) {
	t.Helper()
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, extraParts: []fixturePart{{filename: name, contentType: contentType, data: data}}})
	ref := f.ref(f.partFor(t, name))
	var result AttachmentResult
	var err error
	allocated := allocatedDuring(func() {
		result, err = f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: ref})
	})
	t.Logf("%s: %d bytes on the wire, %d KiB allocated, error %v", name, len(data), allocated>>10, err)
	return result, allocated, err
}

const hostileAllocationBudget = 48 << 20

func TestProtocolDocxNestingBombIsBounded(t *testing.T) {
	// 16 MiB of nested elements deflate to about 17 KB on the wire.
	bomb := fixtureDocxXML(t, func(w io.Writer) error {
		chunk := []byte(strings.Repeat("<a>", 1<<16))
		for written := 0; written < 16<<20-len(chunk); written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	})
	_, allocated, err := getHostileAttachment(t, "bomb.docx", docxMediaType, bomb)
	wantCode(t, err, CodePayloadTooLarge)
	if allocated > hostileAllocationBudget {
		t.Fatalf("docx bomb of %d bytes allocated %d MiB, want at most %d MiB", len(bomb), allocated>>20, hostileAllocationBudget>>20)
	}
}

func TestProtocolDocxDepthLimit(t *testing.T) {
	// Random element names keep the compression ratio and the size inside
	// the other limits, so only the depth limit can stop this document.
	random := rand.New(rand.NewSource(11))
	nested := fixtureDocxXML(t, func(w io.Writer) error {
		var builder strings.Builder
		builder.WriteString(`<w:document xmlns:w="w"><w:body>`)
		name := make([]byte, 8)
		for i := 0; i < 150_000; i++ {
			for j := range name {
				name[j] = byte('a' + random.Intn(26))
			}
			builder.WriteString("<" + string(name) + ">")
		}
		_, err := io.WriteString(w, builder.String())
		return err
	})
	_, allocated, err := getHostileAttachment(t, "nested.docx", docxMediaType, nested)
	wantCode(t, err, CodePayloadTooLarge)
	if allocated > hostileAllocationBudget {
		t.Fatalf("nested docx allocated %d MiB, want at most %d MiB", allocated>>20, hostileAllocationBudget>>20)
	}
}

func TestProtocolDocxCompressionRatioLimit(t *testing.T) {
	// One long run of a single letter is valid text with a ratio near 1000.
	flat := fixtureDocxXML(t, func(w io.Writer) error {
		_, err := io.WriteString(w, `<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>`+strings.Repeat("A", 3<<20)+`</w:t></w:r></w:p></w:body></w:document>`)
		return err
	})
	_, _, err := getHostileAttachment(t, "flat.docx", docxMediaType, flat)
	wantCode(t, err, CodePayloadTooLarge)
}

func TestProtocolDocxDocumentSizeLimit(t *testing.T) {
	// Words from a small set give a ratio near 5, so only the size of the
	// document XML exceeds a limit.
	random := rand.New(rand.NewSource(5))
	words := []string{"offerte ", "levering ", "klant ", "prijs ", "datum ", "regel ", "totaal ", "bedrag "}
	large := fixtureDocxXML(t, func(w io.Writer) error {
		var builder strings.Builder
		builder.WriteString(`<w:document xmlns:w="w"><w:body><w:p><w:r><w:t>`)
		for builder.Len() < 5<<20 {
			builder.WriteString(words[random.Intn(len(words))])
		}
		builder.WriteString(`</w:t></w:r></w:p></w:body></w:document>`)
		_, err := io.WriteString(w, builder.String())
		return err
	})
	if len(large) > MaxAttachmentWireBytes*3/4 {
		t.Fatalf("fixture docx is %d bytes, want it inside the wire limit", len(large))
	}
	_, _, err := getHostileAttachment(t, "large.docx", docxMediaType, large)
	wantCode(t, err, CodePayloadTooLarge)
}
