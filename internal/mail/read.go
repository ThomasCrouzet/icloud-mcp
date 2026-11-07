package mail

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
)

func (s *Client) GetMessage(ctx context.Context, input GetMessageInput) (Message, error) {
	if err := validateIdentity(input.Mailbox, input.UIDValidity, input.UID); err != nil {
		return Message{}, err
	}
	maxBody := input.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultBodyBytes
	}
	if maxBody < 1 || maxBody > MaxBodyBytes {
		return Message{}, validationError("max_body_bytes must be between 1 and 204800")
	}
	value, err := s.read(ctx, func(session imapSession) (any, error) {
		selected, err := session.Select(input.Mailbox, true)
		if err != nil {
			return nil, err
		}
		if selected.UIDValidity != input.UIDValidity {
			return nil, newError(CodeConcurrentModification, "mailbox UIDVALIDITY changed; search for the message again")
		}
		metadata, err := session.FetchMetadata([]uint32{input.UID}, true)
		if err != nil {
			return nil, err
		}
		if len(metadata) != 1 || metadata[0].UID != input.UID {
			return nil, &imapadapter.Error{Kind: imapadapter.ErrorNotFound}
		}
		item := metadata[0]
		result := Message{MessageSummary: summaryFromProtocol(input.Mailbox, input.UIDValidity, item)}
		result.ReplyTo = publicAddresses(item.Headers.ReplyTo)
		if len(result.ReplyTo) == 0 {
			result.ReplyTo = publicAddresses(item.Envelope.ReplyTo)
		}
		result.InReplyTo = append([]string(nil), item.Headers.InReplyTo...)
		if len(result.InReplyTo) == 0 {
			result.InReplyTo = append([]string(nil), item.Envelope.InReplyTo...)
		}
		result.References = append([]string(nil), item.Headers.References...)
		if item.Headers.MessageID != "" {
			result.MessageID = item.Headers.MessageID
		}
		var htmlPart *imapadapter.BodyPart
		hasHTML := false
		for i := range item.Parts {
			part := &item.Parts[i]
			if part.ContentType == "text/html" && !part.Attachment {
				hasHTML = true
				if htmlPart == nil {
					htmlPart = part
				}
			}
			if part.Attachment {
				if len(result.Attachments) >= MaxAttachments {
					return nil, &imapadapter.Error{Kind: imapadapter.ErrorTooLarge}
				}
				result.Attachments = append(result.Attachments, Attachment{
					PartID: partID(part.Path), Filename: part.Filename,
					ContentType: part.ContentType, Size: part.Size, ContentID: part.ContentID,
				})
			}
		}
		var firstPlainBody string
		hasPlain := false
		for _, part := range item.Parts {
			if !part.InlinePlain {
				continue
			}
			bodyData, err := session.FetchBodyPart(input.UID, part.Path)
			if err != nil {
				return nil, err
			}
			if bodyData.TooLarge {
				result.BodyOmitted = true
				result.Warnings = append(result.Warnings, Warning{Code: "body_too_large", Message: "plain-text body exceeded the wire-byte limit and was omitted"})
				return fitMessageResult(result)
			}
			body, oversized, decodeErr := decodePlainBody(bodyData.MIMEHeader, bodyData.Body, maxBody)
			if decodeErr != nil {
				result.BodyOmitted = true
				result.Warnings = append(result.Warnings, Warning{Code: "body_decode_failed", Message: "plain-text body could not be safely decoded and was omitted"})
				return fitMessageResult(result)
			}
			if oversized {
				result.BodyOmitted = true
				result.Warnings = append(result.Warnings, Warning{Code: "body_too_large", Message: "decoded plain-text body exceeded max_body_bytes and was omitted"})
				return fitMessageResult(result)
			}
			if !hasPlain {
				firstPlainBody = body
				hasPlain = true
			}
			if !input.AllowHTMLFallback || strings.TrimSpace(body) != "" {
				result.Body = body
				result.BodySource = "plain_text"
				return fitMessageResult(result)
			}
		}
		if !hasPlain || hasHTML && input.AllowHTMLFallback {
			if hasHTML && input.AllowHTMLFallback && htmlPart != nil {
				htmlData, htmlErr := session.FetchBodyPart(input.UID, htmlPart.Path)
				if htmlErr != nil {
					return nil, htmlErr
				}
				if htmlData.TooLarge {
					result.BodyOmitted = true
					result.BodyUnavailableReason = "html_only"
					result.Warnings = append(result.Warnings, Warning{Code: "html_too_large", Message: "HTML body exceeded the wire-byte limit and was omitted"})
					return fitMessageResult(result)
				}
				htmlBody, decodeErr := decodeHTMLBody(htmlData.MIMEHeader, htmlData.Body)
				if decodeErr != nil {
					result.BodyOmitted = true
					result.BodyUnavailableReason = "html_only"
					result.Warnings = append(result.Warnings, Warning{Code: "html_decode_failed", Message: "HTML body could not be safely decoded and was omitted"})
					return fitMessageResult(result)
				}
				text, truncated, convertErr := htmlToText(htmlBody, maxBody)
				if convertErr != nil {
					result.BodyOmitted = true
					result.BodyUnavailableReason = "html_only"
					result.Warnings = append(result.Warnings, Warning{Code: "html_malformed", Message: "HTML body was malformed and was omitted"})
					return fitMessageResult(result)
				}
				result.Body = text
				result.BodySource = "html_fallback"
				if truncated {
					result.Warnings = append(result.Warnings, Warning{Code: "html_truncated", Message: "HTML text was truncated to max_body_bytes"})
				}
				return fitMessageResult(result)
			}
			if hasHTML {
				result.BodyUnavailableReason = "html_only"
			} else {
				result.BodyUnavailableReason = "no_plain_text"
			}
			return fitMessageResult(result)
		}
		result.Body = firstPlainBody
		result.BodySource = "plain_text"
		return fitMessageResult(result)
	})
	if err != nil {
		return Message{}, err
	}
	return value.(Message), nil
}

func decodePlainBody(header, body []byte, maxBytes int) (string, bool, error) {
	if len(header) > MaxHeaderBytes || len(body) > MaxWireBodyBytes {
		return "", true, nil
	}
	combined := make([]byte, 0, len(header)+len(body)+4)
	combined = append(combined, header...)
	if !bytes.HasSuffix(combined, []byte("\r\n\r\n")) && !bytes.HasSuffix(combined, []byte("\n\n")) {
		if bytes.HasSuffix(combined, []byte("\r\n")) {
			combined = append(combined, '\r', '\n')
		} else if bytes.HasSuffix(combined, []byte("\n")) {
			combined = append(combined, '\n')
		} else {
			combined = append(combined, '\r', '\n', '\r', '\n')
		}
	}
	combined = append(combined, body...)
	entity, err := message.ReadWithOptions(bytes.NewReader(combined), &message.ReadOptions{MaxHeaderBytes: MaxHeaderBytes})
	if err != nil {
		return "", false, err
	}
	mediaType, _, err := entity.Header.ContentType()
	if err != nil || !strings.EqualFold(mediaType, "text/plain") {
		return "", false, errUnsafeMIME
	}
	decoded, err := io.ReadAll(io.LimitReader(entity.Body, int64(maxBytes)+1))
	if err != nil {
		return "", false, err
	}
	if len(decoded) > maxBytes {
		return "", true, nil
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD"), false, nil
}

var errUnsafeMIME = &Error{Code: CodeProtocolError, Message: "selected MIME part is not plain text"}

func partID(path []int) string {
	if len(path) == 0 {
		return "root"
	}
	var builder strings.Builder
	for i, value := range path {
		if i > 0 {
			builder.WriteByte('.')
		}
		builder.WriteString(itoa(value))
	}
	return builder.String()
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[position:])
}

func fitMessageResult(result Message) (Message, error) {
	if serializedSize(result) <= MaxResultBytes {
		return result, nil
	}
	if result.Body != "" {
		result.Warnings = append(result.Warnings, Warning{Code: "body_truncated", Message: "message body was truncated to fit the result limit"})
		for serializedSize(result) > MaxResultBytes && len(result.Body) > 0 {
			over := serializedSize(result) - MaxResultBytes
			cut := len(result.Body) - over - 1024
			if cut < 0 {
				cut = 0
			}
			result.Body = truncateUTF8(result.Body, cut)
		}
	}
	if serializedSize(result) > MaxResultBytes {
		result.Body = ""
		result.BodyOmitted = true
	}
	if serializedSize(result) > MaxResultBytes {
		return Message{}, newError(CodePayloadTooLarge, "message metadata exceeds the result limit")
	}
	return result, nil
}

func decodeHTMLBody(header, body []byte) (string, error) {
	if len(header) > MaxHeaderBytes || len(body) > MaxWireBodyBytes {
		return "", errTooLargeHTML
	}
	combined := make([]byte, 0, len(header)+len(body)+4)
	combined = append(combined, header...)
	if !bytes.HasSuffix(combined, []byte("\r\n\r\n")) && !bytes.HasSuffix(combined, []byte("\n\n")) {
		if bytes.HasSuffix(combined, []byte("\r\n")) {
			combined = append(combined, '\r', '\n')
		} else if bytes.HasSuffix(combined, []byte("\n")) {
			combined = append(combined, '\n')
		} else {
			combined = append(combined, '\r', '\n', '\r', '\n')
		}
	}
	combined = append(combined, body...)
	entity, err := message.ReadWithOptions(bytes.NewReader(combined), &message.ReadOptions{MaxHeaderBytes: MaxHeaderBytes})
	if err != nil {
		return "", err
	}
	mediaType, _, err := entity.Header.ContentType()
	if err != nil || !strings.EqualFold(mediaType, "text/html") {
		return "", errUnsafeMIME
	}
	decoded, err := io.ReadAll(io.LimitReader(entity.Body, int64(MaxWireBodyBytes)+1))
	if err != nil {
		return "", err
	}
	if len(decoded) > MaxWireBodyBytes {
		return "", errTooLargeHTML
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD"), nil
}

var errTooLargeHTML = &Error{Code: CodePayloadTooLarge, Message: "HTML body exceeded its byte limit"}
