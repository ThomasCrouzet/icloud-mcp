package mail

import (
	"context"
	"errors"
	"strings"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

const (
	ArchivalDisabled     = "disabled"
	ArchivalNotAttempted = "not_attempted"
	ArchivalStored       = "stored"
	ArchivalFailed       = "failed"
	ArchivalUnknown      = "unknown"
)

// archiveSent appends the exact accepted message to the unique selectable
// SPECIAL-USE Sent mailbox. It never retries APPEND.
func (s *Client) archiveSent(ctx context.Context, message []byte) (string, string, imapadapter.AppendData, error) {
	if !s.config.EnableSentCopy {
		return ArchivalDisabled, "", imapadapter.AppendData{}, nil
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	conn, err := s.imapDial(ctx)
	if err != nil {
		return ArchivalFailed, "", imapadapter.AppendData{}, err
	}
	session, err := s.imapFactory(ctx, conn, s.config.Address, s.config.Password)
	if err != nil {
		safeCloseConn(conn)
		return ArchivalFailed, "", imapadapter.AppendData{}, err
	}
	defer safeCloseIMAPSession(session, conn)
	mailboxes, err := session.List()
	if err != nil {
		return ArchivalFailed, "", imapadapter.AppendData{}, err
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
		return ArchivalFailed, "", imapadapter.AppendData{}, errors.New("exactly one selectable SPECIAL-USE Sent mailbox is required")
	}
	data, err := session.AppendSent(sent[0], message)
	if err != nil {
		var protocolErr *imapadapter.Error
		if errors.As(err, &protocolErr) && protocolErr.Ambiguous {
			return ArchivalUnknown, sent[0], imapadapter.AppendData{}, err
		}
		return ArchivalFailed, sent[0], imapadapter.AppendData{}, err
	}
	return ArchivalStored, sent[0], data, nil
}

func safeCloseIMAPSession(session imapSession, conn interface{ Close() error }) {
	defer func() { _ = recover() }()
	if session != nil {
		_ = session.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}
