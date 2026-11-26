package mail

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"
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

// fixtureRawPNG writes a PNG without filters. The first noiseRows rows are
// random and the rest are zero, so the file size stays controllable while the
// declared dimensions stay large. plte is the palette for color type 3.
func fixtureRawPNG(t *testing.T, width, height int, colorType, bitDepth byte, plte []byte, noiseRows int) []byte {
	t.Helper()
	channels := map[byte]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[colorType]
	rowBytes := (width*channels*int(bitDepth) + 7) / 8
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(data)))
		out.Write(length[:])
		sum := crc32.NewIEEE()
		_, _ = io.WriteString(sum, kind)
		_, _ = sum.Write(data)
		out.WriteString(kind)
		out.Write(data)
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], sum.Sum32())
		out.Write(crc[:])
	}
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:], uint32(width))
	binary.BigEndian.PutUint32(header[4:], uint32(height))
	header[8], header[9] = bitDepth, colorType
	chunk("IHDR", header)
	if plte != nil {
		chunk("PLTE", plte)
	}
	var compressed bytes.Buffer
	writer, err := zlib.NewWriterLevel(&compressed, zlib.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(3))
	row := make([]byte, rowBytes+1)
	for y := 0; y < height; y++ {
		clear(row)
		if y < noiseRows {
			_, _ = random.Read(row[1:])
		}
		if _, err := writer.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	chunk("IDAT", compressed.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

func inResizeWindow(t *testing.T, data []byte) {
	t.Helper()
	if len(data) <= MaxAttachmentInlineBytes || len(data) > MaxAttachmentWireBytes*3/4 {
		t.Fatalf("fixture image is %d bytes, want it between the inline and the wire limits", len(data))
	}
}

func TestProtocolImageDecodeBudget(t *testing.T) {
	// 4000 x 3500 pixels at 16 bits per channel pass the edge and pixel
	// limits but decode into about 107 MiB.
	deep := fixtureRawPNG(t, 4000, 3500, 6, 16, nil, 30)
	inResizeWindow(t, deep)
	_, allocated, err := getHostileAttachment(t, "deep.png", "image/png", deep)
	wantCode(t, err, CodePayloadTooLarge)
	if allocated > hostileAllocationBudget {
		t.Fatalf("16-bit PNG allocated %d MiB, want at most %d MiB", allocated>>20, hostileAllocationBudget>>20)
	}
}

// fixtureJPEGHeader writes a JFIF marker, a frame header, and comment
// padding, which is enough for DecodeConfig. marker selects baseline (0xc0)
// or progressive (0xc2).
func fixtureJPEGHeader(width, height int, marker byte, size int) []byte {
	out := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0,
		0xff, marker, 0, 17, 8, byte(height >> 8), byte(height), byte(width >> 8), byte(width), 3,
		1, 0x11, 0, 2, 0x11, 0, 3, 0x11, 0}
	for len(out) < size {
		out = append(out, 0xff, 0xfe, 0xff, 0xff)
		out = append(out, make([]byte, 0xfffd)...)
	}
	return out
}

func TestProtocolImageDecodeBudgetCountsProgressiveJPEG(t *testing.T) {
	// A progressive decode keeps one coefficient for each sample, so the
	// same dimensions cost more than a baseline decode.
	progressive := fixtureJPEGHeader(3000, 2500, 0xc2, 700<<10)
	inResizeWindow(t, progressive)
	_, _, err := getHostileAttachment(t, "progressive.jpg", "image/jpeg", progressive)
	wantCode(t, err, CodePayloadTooLarge)
	// The baseline control passes the budget and fails only in the decoder.
	baseline := fixtureJPEGHeader(3000, 2500, 0xc0, 700<<10)
	_, _, err = getHostileAttachment(t, "baseline.jpg", "image/jpeg", baseline)
	wantCode(t, err, CodeProtocolError)
}

func TestProtocolImageDimensionLimits(t *testing.T) {
	// An edge above 12,000 pixels, with few decoded bytes.
	wide := fixtureRawPNG(t, 13_000, 64, 0, 8, nil, 64)
	inResizeWindow(t, wide)
	_, _, err := getHostileAttachment(t, "wide.png", "image/png", wide)
	wantCode(t, err, CodePayloadTooLarge)
	// More than 25 megapixels at one bit per pixel, inside the byte budget.
	many := fixtureRawPNG(t, 5_100, 5_000, 3, 1, []byte{0, 0, 0, 255, 255, 255}, 1_000)
	inResizeWindow(t, many)
	_, _, err = getHostileAttachment(t, "many.png", "image/png", many)
	wantCode(t, err, CodePayloadTooLarge)
}

func TestProtocolImageDecodeIsSerialized(t *testing.T) {
	large := fixturePNG(t, 700, 500, true)
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, extraParts: []fixturePart{{filename: "noise.png", contentType: "image/png", data: large}}})
	ref := f.ref(f.partFor(t, "noise.png"))
	// Another call holds the one decode slot, so this call must wait and
	// reach its deadline instead of decoding in parallel.
	imageDecodeSlot <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	_, err := f.client.GetAttachment(ctx, GetAttachmentInput{AttachmentRef: ref})
	cancel()
	<-imageDecodeSlot
	wantCode(t, err, CodeTimeout)
	resized, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: ref})
	if err != nil || resized.Delivery != DeliveryImageResized {
		t.Fatalf("resize after the slot is free = %s, %v", resized.Delivery, err)
	}
}
