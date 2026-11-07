package mail

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

const (
	ArchivalDisabled     = "disabled"
	ArchivalNotAttempted = "not_attempted"
	ArchivalStored       = "stored"
	ArchivalFailed       = "failed"
	ArchivalUnknown      = "unknown"
)

// resolveSentDestination must complete before any SMTP connection. The returned
// name pins APPEND to the authorized mailbox, even if later discovery changes.
func (s *Client) resolveSentDestination(ctx context.Context) (string, error) {
	result, err := s.imapAttempt(ctx, nil, func(session imapSession) (any, error) {
		mailboxes, err := session.List()
		if err != nil {
			return nil, err
		}
		var sent []string
		for _, mailbox := range mailboxes {
			if !isSelectable(mailbox) {
				continue
			}
			for _, attribute := range mailbox.Attributes {
				if strings.EqualFold(attribute, "\\Sent") {
					sent = append(sent, mailbox.Name)
					break
				}
			}
		}
		if len(sent) != 1 {
			return nil, newError(CodeProtocolError, "exactly one selectable SPECIAL-USE Sent mailbox is required")
		}
		if err := validateMailbox(sent[0]); err != nil {
			return nil, newError(CodeProtocolError, "Sent mailbox name is invalid")
		}
		if len(s.config.ArchiveScope) > 0 && !scopeAllows(s.config.ArchiveScope, sent[0]) {
			return nil, newError(CodeAuthorization, "Sent mailbox is outside the configured resource scope")
		}
		return sent[0], nil
	})
	if err != nil {
		if deadline, ok := ctx.Deadline(); ctx.Err() != nil || ok && !time.Now().Before(deadline) {
			return "", contextError()
		}
		return "", mapIMAPError(err, ctx, false, "")
	}
	return result.(string), nil
}

// archiveSent uses only the preauthorized destination. Never retry APPEND or
// substitute another mailbox after definitive SMTP acceptance.
func (s *Client) archiveSent(ctx context.Context, destination string, message []byte) (string, string, imapadapter.AppendData, error) {
	phase := &imapMutationPhase{}
	result, err := s.imapAttempt(ctx, phase, func(session imapSession) (any, error) {
		phase.dispatched = true
		return session.AppendSent(destination, message)
	})
	if err != nil {
		var protocolErr *imapadapter.Error
		if errors.As(err, &protocolErr) && protocolErr.Ambiguous {
			return ArchivalUnknown, destination, imapadapter.AppendData{}, err
		}
		return ArchivalFailed, destination, imapadapter.AppendData{}, err
	}
	return ArchivalStored, destination, result.(imapadapter.AppendData), nil
}

func scopeAllows(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
