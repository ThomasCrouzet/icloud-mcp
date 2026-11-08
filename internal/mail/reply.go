package mail

import (
	"context"
	"strings"
	"unicode/utf8"
)

const (
	replyModeSender    = "sender"
	replyModeReplyAll  = "reply_all"
	maxReplyReferences = 20
)

func validateReplyHeaders(inReplyTo string, references []string) error {
	seen := make(map[string]bool, len(references)+1)
	for _, reference := range references {
		if !validMessageID(reference) {
			return validationError("reply references contain an invalid Message-ID")
		}
		if seen[reference] {
			return validationError("reply references contain a duplicate Message-ID")
		}
		seen[reference] = true
	}
	if len(references) > maxReplyReferences {
		return validationError("reply references exceed the 20-entry limit")
	}
	if inReplyTo != "" && !validMessageID(inReplyTo) {
		return validationError("in-reply-to contains an invalid Message-ID")
	}
	return nil
}

func validMessageID(value string) bool {
	if len(value) < 5 || len(value) > MaxSubjectBytes || !strings.HasPrefix(value, "<") || !strings.HasSuffix(value, ">") {
		return false
	}
	inner := value[1 : len(value)-1]
	if !strings.Contains(inner, "@") || strings.ContainsAny(inner, "<> \t\r\n\x00") {
		return false
	}
	return utf8.ValidString(value)
}

func (s *Client) ReplyMessage(ctx context.Context, input ReplyInput) (SendResult, error) {
	mode := strings.TrimSpace(input.Mode)
	if mode == "" {
		mode = replyModeSender
	}
	if mode != replyModeSender && mode != replyModeReplyAll {
		return SendResult{}, validationError("mode must be sender or reply_all")
	}
	if len(input.Body) > MaxSendBodyBytes || !utf8.ValidString(input.Body) || strings.ContainsRune(input.Body, '\x00') {
		return SendResult{}, validationError("body contains invalid or excessive data")
	}
	if err := validateSendIdempotencyKey(input.IdempotencyKey); err != nil {
		return SendResult{}, err
	}
	source, err := s.GetMessage(ctx, GetMessageInput{
		Mailbox: input.Mailbox, UIDValidity: input.UIDValidity, UID: input.UID,
	})
	if err != nil {
		return SendResult{}, err
	}
	if !validMessageID(source.MessageID) {
		return SendResult{}, validationError("source message has no valid Message-ID for a reply")
	}

	candidates := source.ReplyTo
	if len(candidates) == 0 {
		candidates = source.From
	}
	if mode == replyModeReplyAll {
		candidates = append(append([]Address(nil), candidates...), source.To...)
		candidates = append(candidates, source.Cc...)
	}
	var recipients []string
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		address := strings.TrimSpace(candidate.Address)
		if address == "" || canonicalAddrSpec(address) == canonicalAddrSpec(s.config.Address) {
			continue
		}
		normalized, err := validateAddrSpec(address)
		if err != nil {
			return SendResult{}, validationError("source reply recipient is not a plain addr-spec")
		}
		key := canonicalAddrSpec(normalized)
		if seen[key] {
			continue
		}
		seen[key] = true
		recipients = append(recipients, normalized)
	}
	if len(recipients) == 0 {
		return SendResult{}, validationError("source message has no valid reply recipient")
	}
	if len(recipients) > MaxRecipients {
		return SendResult{}, validationError("reply recipient count exceeds 50")
	}

	subject := source.Subject
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), "re:") {
		subject = "Re: " + subject
	}
	subject = truncateUTF8(subject, MaxSubjectBytes)

	references, err := replyReferences(source.References, source.MessageID)
	if err != nil {
		return SendResult{}, err
	}
	return s.SendMessage(ctx, SendInput{
		To: recipients, Subject: subject, Body: input.Body, IdempotencyKey: input.IdempotencyKey,
		InReplyTo: source.MessageID, References: references,
	})
}

func replyReferences(existing []string, messageID string) ([]string, error) {
	var references []string
	seen := make(map[string]bool)
	for _, reference := range append(append([]string(nil), existing...), messageID) {
		reference = strings.TrimSpace(reference)
		if reference == "" || seen[reference] {
			continue
		}
		if !validMessageID(reference) {
			return nil, validationError("source message has an invalid References header")
		}
		seen[reference] = true
		references = append(references, reference)
		if len(references) > maxReplyReferences {
			return nil, validationError("source message References exceeds the 20-entry limit")
		}
	}
	return references, nil
}
