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

type replySource struct {
	MessageID  string
	Subject    string
	References []string
	InReplyTo  []string
	ReplyTo    []Address
	From       []Address
	To         []Address
	Cc         []Address
}

// getReplyMetadata reads only curated headers and envelope metadata. It never
// fetches a body part, so a reply does not process message text it cannot use.
func (s *Client) getReplyMetadata(ctx context.Context, input GetMessageInput) (replySource, error) {
	if err := validateIdentity(input.Mailbox, input.UIDValidity, input.UID); err != nil {
		return replySource{}, err
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
		source := replySource{
			MessageID:  item.Envelope.MessageID,
			Subject:    item.Envelope.Subject,
			References: append([]string(nil), item.Headers.References...),
			InReplyTo:  append([]string(nil), item.Headers.InReplyTo...),
			ReplyTo:    publicAddresses(item.Headers.ReplyTo),
			From:       publicAddresses(item.Envelope.From),
			To:         publicAddresses(item.Envelope.To),
			Cc:         publicAddresses(item.Envelope.Cc),
		}
		if len(source.ReplyTo) == 0 {
			source.ReplyTo = publicAddresses(item.Envelope.ReplyTo)
		}
		if len(source.InReplyTo) == 0 {
			source.InReplyTo = append([]string(nil), item.Envelope.InReplyTo...)
		}
		if item.Headers.MessageID != "" {
			source.MessageID = item.Headers.MessageID
		}
		return source, nil
	})
	if err != nil {
		return replySource{}, err
	}
	return value.(replySource), nil
}

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
		selectedPlain := make([]*imapadapter.BodyPart, 0, len(item.Parts))
		for index := range item.Parts {
			if item.Parts[index].InlinePlain {
				selectedPlain = append(selectedPlain, &item.Parts[index])
			}
		}

		var segments []string
		firstRawBody := ""
		plainFailed := false
		chosenAlternative := make(map[string]bool)
		for index, part := range selectedPlain {
			// Parts in one multipart/alternative group are equivalent
			// representations. Search the group until one meaningful plain
			// alternative is found, then never concatenate another.
			if part.AlternativeGroup != "" && chosenAlternative[part.AlternativeGroup] {
				continue
			}
			bodyData, err := session.FetchBodyPart(input.UID, part.Path)
			if err != nil {
				return nil, err
			}
			if bodyData.TooLarge {
				result.BodyOmitted = true
				plainFailed = true
				result.Warnings = append(result.Warnings, Warning{Code: "body_too_large", Message: "one plain-text part exceeded the wire-byte limit and was omitted"})
				continue
			}
			body, oversized, decodeErr := decodePlainBody(bodyData.MIMEHeader, bodyData.Body, maxBody)
			if decodeErr != nil {
				result.BodyOmitted = true
				plainFailed = true
				result.Warnings = append(result.Warnings, Warning{Code: "body_decode_failed", Message: "one plain-text part could not be safely decoded and was omitted"})
				continue
			}
			if oversized {
				result.BodyOmitted = true
				plainFailed = true
				result.Warnings = append(result.Warnings, Warning{Code: "body_too_large", Message: "one decoded plain-text part exceeded max_body_bytes and was omitted"})
				continue
			}
			if index == 0 {
				firstRawBody = body
			}
			if strings.TrimSpace(body) == "" {
				continue
			}
			segments = append(segments, body)
			if part.AlternativeGroup != "" {
				chosenAlternative[part.AlternativeGroup] = true
			}
		}

		var combined strings.Builder
		joinTruncated := false
		for _, segment := range segments {
			separator := ""
			if combined.Len() > 0 {
				separator = "\n\n"
			}
			if combined.Len()+len(separator)+len(segment) > maxBody {
				remaining := maxBody - combined.Len() - len(separator)
				if remaining > 0 {
					combined.WriteString(separator)
					combined.WriteString(truncateUTF8(segment, remaining))
				}
				joinTruncated = true
				break
			}
			combined.WriteString(separator)
			combined.WriteString(segment)
		}
		plainBody := combined.String()
		if len(segments) == 0 && len(selectedPlain) > 0 && !plainFailed {
			plainBody = firstRawBody
		}
		if plainFailed && len(segments) == 0 {
			return fitMessageResult(result)
		}
		if joinTruncated {
			result.BodyOmitted = true
			result.Warnings = append(result.Warnings, Warning{Code: "body_truncated", Message: "later plain-text parts did not fit in max_body_bytes"})
		}

		usePlain := plainFailed || strings.TrimSpace(plainBody) != "" || !input.AllowHTMLFallback || len(selectedPlain) > 0 && !hasHTML
		if usePlain {
			result.Body = plainBody
			result.BodySource = "plain_text"
			if strings.TrimSpace(plainBody) == "" && len(selectedPlain) == 0 {
				if hasHTML {
					result.BodyUnavailableReason = "html_only"
				} else {
					result.BodyUnavailableReason = "no_plain_text"
				}
			}
			return fitMessageResult(result)
		}
		if hasHTML && htmlPart != nil {
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
