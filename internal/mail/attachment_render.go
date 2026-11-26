package mail

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"image"
	"image/color"
	_ "image/gif" // register the GIF decoder for DecodeConfig and Decode
	"image/jpeg"
	_ "image/png" // register the PNG decoder for DecodeConfig and Decode
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"
)

// Image and document limits for rendering. They bound decode work on
// untrusted attachments.
const (
	maxImagePixels       = 25_000_000
	maxImageEdge         = 12_000
	maxImageDecodeBytes  = 96 * 1024 * 1024
	maxDocxEntries       = 5_000
	maxDocxDocumentBytes = 4 * 1024 * 1024
	maxDocxTokens        = 1_000_000
	maxDocxDepth         = 128
	maxDocxAttributes    = 256
	maxDocxRatio         = 100
	docxMediaType        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// resizeEdges are the long-edge targets tried in order for an oversized image.
var resizeEdges = []int{1568, 1024, 768, 512}

// renderSlot lets one image decode and resize, or one Word text extraction,
// run at a time in the process, so concurrent tool calls cannot add their
// decode buffers.
var renderSlot = make(chan struct{}, 1)

var extensionTypes = map[string]string{
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".heic": "image/heic", ".txt": "text/plain",
	".csv": "text/csv", ".md": "text/markdown", ".html": "text/html", ".htm": "text/html",
	".ics": "text/calendar", ".vcf": "text/vcard", ".json": "application/json", ".xml": "application/xml",
	".docx": docxMediaType, ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".zip":  "application/zip", ".doc": "application/msword",
}

func parseContentType(value string) (string, map[string]string, bool) {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(mediaType, "/") {
		return "", nil, false
	}
	return strings.ToLower(mediaType), params, true
}

// effectiveContentType replaces a generic type with the type implied by a
// known extension. It uses a fixed table, never the host MIME database.
func effectiveContentType(contentType, filename string) string {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if contentType != "" && contentType != "application/octet-stream" {
		return contentType
	}
	if dot := strings.LastIndexByte(filename, '.'); dot >= 0 {
		if known, ok := extensionTypes[strings.ToLower(filename[dot:])]; ok {
			return known
		}
	}
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

func isImageType(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// renderAttachment chooses the delivery for one decoded part. Images go to
// the model as images, text and Word documents as bounded text, and other
// files as an embedded resource when they fit the inline limit.
func renderAttachment(ctx context.Context, result *AttachmentResult, part fetchedPart, format string, maxText int) error {
	contentType := result.ContentType
	data := part.Data
	if isImageType(contentType) {
		if len(data) <= MaxAttachmentInlineBytes {
			result.Delivery, result.Data, result.DataMIME = DeliveryImage, data, contentType
			if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
				result.Width, result.Height = config.Width, config.Height
			}
			return nil
		}
		if format == "original" || contentType == "image/webp" {
			return newError(CodePayloadTooLarge, "image exceeds the 600 KiB inline limit; use format auto for a resized copy, or attach it to a message by reference")
		}
		resized, width, height, err := resizeImage(ctx, data)
		if err != nil {
			return err
		}
		result.Delivery, result.Data, result.DataMIME = DeliveryImageResized, resized, "image/jpeg"
		result.Width, result.Height = width, height
		result.Warnings = append(result.Warnings, Warning{Code: "image_resized", Message: "the image was resized to JPEG to fit the inline limit; size and sha256 describe the original"})
		return nil
	}
	if format == "auto" {
		switch {
		case contentType == docxMediaType:
			text, truncated, err := serializedDocxText(ctx, data, maxText)
			if err != nil {
				return err
			}
			result.Delivery, result.Text, result.TextTruncated = DeliveryText, text, truncated
			return nil
		case contentType == "text/html":
			decoded, err := decodeCharset(data, part.Charset)
			if err != nil {
				return err
			}
			text, truncated, err := htmlToText(decoded, maxText)
			if err != nil {
				return newError(CodeProtocolError, "HTML attachment was malformed")
			}
			result.Delivery, result.Text, result.TextTruncated = DeliveryText, text, truncated
			return nil
		case strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml":
			decoded, err := decodeCharset(data, part.Charset)
			if err != nil {
				return err
			}
			if len(decoded) > maxText {
				decoded = truncateUTF8(decoded, maxText)
				result.TextTruncated = true
			}
			result.Delivery, result.Text = DeliveryText, decoded
			return nil
		}
	}
	if len(data) > MaxAttachmentInlineBytes {
		return newError(CodePayloadTooLarge, "attachment exceeds the 600 KiB inline limit; attach it to a message by reference instead")
	}
	result.Delivery, result.Data, result.DataMIME = DeliveryResource, data, contentType
	return nil
}

func decodeCharset(data []byte, label string) (string, error) {
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		return strings.ToValidUTF8(string(data), "\uFFFD"), nil
	}
	reader, err := charset.Reader(label, bytes.NewReader(data))
	if err != nil {
		return strings.ToValidUTF8(string(data), "\uFFFD"), nil
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(MaxAttachmentWireBytes)*4))
	if err != nil {
		return "", newError(CodeProtocolError, "text attachment could not be decoded")
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD"), nil
}

// resizeImage decodes a PNG, JPEG, or GIF after a dimension and memory check
// and encodes a smaller JPEG that fits the inline limit.
func resizeImage(ctx context.Context, data []byte) ([]byte, int, int, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, newError(CodeProtocolError, "image could not be decoded")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxImageEdge || config.Height > maxImageEdge ||
		int64(config.Width)*int64(config.Height) > maxImagePixels {
		return nil, 0, 0, newError(CodePayloadTooLarge, "image dimensions exceed the decode limit")
	}
	if imageDecodeBytes(config, format, data) > maxImageDecodeBytes {
		return nil, 0, 0, newError(CodePayloadTooLarge, "image needs more decode memory than the limit")
	}
	if err := acquire(ctx, renderSlot); err != nil {
		return nil, 0, 0, err
	}
	defer func() { <-renderSlot }()
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, newError(CodeProtocolError, "image could not be decoded")
	}
	for _, edge := range resizeEdges {
		scaled := scaleImage(source, edge)
		for _, quality := range []int{82, 70} {
			var buffer bytes.Buffer
			if err := jpeg.Encode(&buffer, scaled, &jpeg.Options{Quality: quality}); err != nil {
				return nil, 0, 0, newError(CodeInternalError, "image could not be encoded")
			}
			if buffer.Len() <= MaxAttachmentInlineBytes {
				bounds := scaled.Bounds()
				return buffer.Bytes(), bounds.Dx(), bounds.Dy(), nil
			}
		}
	}
	return nil, 0, 0, newError(CodePayloadTooLarge, "image does not fit the inline limit after resizing")
}

// imageDecodeBytes estimates the memory that the standard decoders use for
// the image described by config. An Adam7 PNG decode also keeps one pass
// image of up to half the full image.
func imageDecodeBytes(config image.Config, format string, data []byte) int64 {
	estimate := int64(config.Width) * int64(config.Height) * decodeBytesPerPixel(format, config.ColorModel, data)
	if format == "png" && pngInterlaced(data) {
		estimate += estimate / 2
	}
	return estimate
}

// pngInterlaced reports whether the IHDR chunk sets an interlace method. A PNG
// stores IHDR first, right after the 8-byte signature.
func pngInterlaced(data []byte) bool {
	return len(data) > 28 && string(data[12:16]) == "IHDR" && data[28] != 0
}

// decodeBytesPerPixel is an upper bound of the memory that the standard
// decoders use for each pixel. A PNG transparency chunk can widen gray to
// NRGBA. A JPEG can need an RGB copy of its YCbCr planes, and a progressive
// JPEG keeps one 32-bit coefficient for each sample of each component.
func decodeBytesPerPixel(format string, model color.Model, data []byte) int64 {
	if format == "jpeg" {
		perPixel, components := int64(1), int64(1)
		switch model {
		case color.YCbCrModel:
			perPixel, components = 7, 3
		case color.CMYKModel:
			perPixel, components = 8, 4
		}
		if jpegProgressive(data) {
			perPixel += 4 * components
		}
		return perPixel
	}
	if _, ok := model.(color.Palette); ok {
		return 1
	}
	switch model {
	case color.GrayModel, color.RGBAModel, color.NRGBAModel:
		return 4
	}
	return 8
}

// jpegProgressive reports whether the first frame header of a JPEG is
// progressive. A header that it cannot find counts as progressive.
func jpegProgressive(data []byte) bool {
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			return true
		}
		switch marker := data[i+1]; {
		case marker == 0xff:
			i++
		case marker == 0xc0 || marker == 0xc1:
			return false
		case marker == 0x01 || marker >= 0xd0 && marker <= 0xd8:
			i += 2
		default:
			i += 2 + (int(data[i+2])<<8 | int(data[i+3]))
		}
	}
	return true
}

// scaleImage downsamples with a bounded box filter and composites any alpha
// over white. It samples at most 3 by 3 source pixels per target pixel. It
// reads samples without an interface value for each one, so the scan makes
// no garbage.
func scaleImage(source image.Image, longEdge int) *image.RGBA {
	direct, _ := source.(image.RGBA64Image)
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	targetW, targetH := width, height
	if width >= height && width > longEdge {
		targetW, targetH = longEdge, max(1, height*longEdge/width)
	} else if height > width && height > longEdge {
		targetW, targetH = max(1, width*longEdge/height), longEdge
	}
	out := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	for y := 0; y < targetH; y++ {
		y0 := bounds.Min.Y + y*height/targetH
		y1 := bounds.Min.Y + max((y+1)*height/targetH, y*height/targetH+1)
		for x := 0; x < targetW; x++ {
			x0 := bounds.Min.X + x*width/targetW
			x1 := bounds.Min.X + max((x+1)*width/targetW, x*width/targetW+1)
			var r, g, b, n uint64
			stepY, stepX := max(1, (y1-y0)/3), max(1, (x1-x0)/3)
			for sy := y0; sy < y1; sy += stepY {
				for sx := x0; sx < x1; sx += stepX {
					var cr, cg, cb, ca uint32
					if direct != nil {
						sample := direct.RGBA64At(sx, sy)
						cr, cg, cb, ca = uint32(sample.R), uint32(sample.G), uint32(sample.B), uint32(sample.A)
					} else {
						cr, cg, cb, ca = source.At(sx, sy).RGBA()
					}
					// Composite premultiplied color over white.
					white := 0xffff - ca
					r += uint64(cr + white)
					g += uint64(cg + white)
					b += uint64(cb + white)
					n++
				}
			}
			out.SetRGBA(x, y, color.RGBA{R: uint8(r / n >> 8), G: uint8(g / n >> 8), B: uint8(b / n >> 8), A: 0xff})
		}
	}
	return out
}

var errDocxMalformed = &Error{Code: CodeProtocolError, Message: "Word document could not be read as text"}

// serializedDocxText runs docxText in the render slot, so one Word text
// extraction or image decode runs at a time in the process.
func serializedDocxText(ctx context.Context, data []byte, maxText int) (string, bool, error) {
	if err := acquire(ctx, renderSlot); err != nil {
		return "", false, err
	}
	defer func() { <-renderSlot }()
	return docxText(data, maxText)
}

// docxText extracts the paragraph text of word/document.xml. It bounds the
// archive entry count, the decompressed size, the compression ratio, the
// element depth, the attributes of each element, the token count, and the
// output. It never extracts files and ignores every other archive entry.
func docxText(data []byte, maxText int) (string, bool, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", false, errDocxMalformed
	}
	if len(reader.File) > maxDocxEntries {
		return "", false, newError(CodePayloadTooLarge, "Word document has too many archive entries")
	}
	var document *zip.File
	for _, file := range reader.File {
		if file.Name == "word/document.xml" {
			document = file
			break
		}
	}
	if document == nil {
		return "", false, errDocxMalformed
	}
	if document.UncompressedSize64 > maxDocxDocumentBytes {
		return "", false, newError(CodePayloadTooLarge, "Word document text exceeds the decode limit")
	}
	// The archive reader rejects an entry that inflates beyond its declared
	// size, so the declared sizes bound the real ratio.
	if document.UncompressedSize64 > 0 && (document.CompressedSize64 == 0 || document.UncompressedSize64/document.CompressedSize64 > maxDocxRatio) {
		return "", false, newError(CodePayloadTooLarge, "Word document is compressed beyond the decode limit")
	}
	stream, err := document.Open()
	if err != nil {
		return "", false, errDocxMalformed
	}
	defer func() { _ = stream.Close() }()
	return extractDocxXML(io.LimitReader(stream, maxDocxDocumentBytes+1), maxText)
}

func extractDocxXML(stream io.Reader, maxText int) (string, bool, error) {
	decoder := xml.NewDecoder(stream)
	decoder.Strict = true
	decoder.CharsetReader = nil
	var builder strings.Builder
	inText := false
	tokens := 0
	depth := 0
	newlines := 0
	write := func(value string) bool {
		for _, r := range value {
			if r == '\n' {
				if newlines >= 2 {
					continue
				}
				newlines++
			} else {
				newlines = 0
			}
			if builder.Len()+utf8.RuneLen(r) > maxText {
				return false
			}
			builder.WriteRune(r)
		}
		return true
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, errDocxMalformed
		}
		tokens++
		if tokens > maxDocxTokens {
			return "", false, newError(CodePayloadTooLarge, "Word document is too complex to read")
		}
		switch value := token.(type) {
		case xml.StartElement:
			// The decoder keeps one entry for each open element.
			depth++
			if depth > maxDocxDepth {
				return "", false, newError(CodePayloadTooLarge, "Word document is nested too deeply to read")
			}
			// The decoder keeps the namespace declarations of each open
			// element. This bound limits them; renderSlot limits the parse
			// cost of one oversized element to one call at a time.
			if len(value.Attr) > maxDocxAttributes {
				return "", false, newError(CodePayloadTooLarge, "Word document has too many attributes on one element")
			}
			switch value.Name.Local {
			case "t":
				inText = true
			case "tab":
				if !write("\t") {
					return strings.TrimSpace(builder.String()), true, nil
				}
			case "br", "cr":
				if !write("\n") {
					return strings.TrimSpace(builder.String()), true, nil
				}
			}
		case xml.EndElement:
			depth--
			switch value.Name.Local {
			case "t":
				inText = false
			case "p":
				if !write("\n") {
					return strings.TrimSpace(builder.String()), true, nil
				}
			}
		case xml.CharData:
			if inText && !write(string(value)) {
				return strings.TrimSpace(builder.String()), true, nil
			}
		}
	}
	return strings.TrimSpace(builder.String()), false, nil
}
