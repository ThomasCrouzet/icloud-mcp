package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	msgmail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type smtpClientSession struct {
	client   *smtp.Client
	done     chan struct{}
	once     sync.Once
	budget   *smtpReadBudgetConn
	tlsReady bool
}

const maxSMTPInboundBytes int64 = 1024 * 1024

func newSMTPSession(ctx context.Context, conn net.Conn) (smtpSession, error) {
	return newSMTPSessionWithTLS(ctx, conn, &tls.Config{ServerName: smtpHost, MinVersion: tls.VersionTLS12})
}

func newSMTPSessionWithTLS(ctx context.Context, conn net.Conn, tlsConfig *tls.Config) (smtpSession, error) {
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	deadline := time.Now().Add(toolTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	clamped := &deadlineConn{Conn: conn, deadline: deadline}
	if err := clamped.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	budgeted := &smtpReadBudgetConn{Conn: clamped, limit: maxSMTPInboundBytes}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = clamped.Close()
		case <-done:
		}
	}()
	client, err := smtp.NewClientStartTLS(budgeted, tlsConfig)
	if err != nil {
		close(done)
		return nil, budgeted.normalize(err)
	}
	remaining, err := remainingSMTPTimeout(deadline)
	if err != nil {
		_ = client.Close()
		close(done)
		return nil, err
	}
	client.CommandTimeout = remaining
	client.SubmissionTimeout = remaining
	// NewClientStartTLS wraps the connection but does not perform the TLS
	// handshake or post-TLS EHLO. Hello is the public error-returning path that
	// forces both before capability checks whose API otherwise suppresses errors.
	if err := client.Hello("localhost"); err != nil {
		err = budgeted.normalize(err)
		if errors.Is(err, errSMTPInboundLimit) {
			_ = budgeted.Close()
		}
		_ = client.Close()
		close(done)
		return nil, err
	}
	state, ok := client.TLSConnectionState()
	if !ok || !state.HandshakeComplete || len(state.VerifiedChains) == 0 {
		_ = client.Close()
		close(done)
		return nil, errSMTPTLSUnverified
	}
	return &smtpClientSession{client: client, done: done, budget: budgeted, tlsReady: true}, nil
}

func remainingSMTPTimeout(deadline time.Time) (time.Duration, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	if remaining > toolTimeout {
		return toolTimeout, nil
	}
	return remaining, nil
}

// deadlineConn prevents library defaults from extending the tool deadline.
type deadlineConn struct {
	net.Conn
	deadline time.Time
}

func (c *deadlineConn) clamp(value time.Time) time.Time {
	if c.deadline.IsZero() {
		return value
	}
	if value.IsZero() || c.deadline.Before(value) {
		return c.deadline
	}
	return value
}

func (c *deadlineConn) SetDeadline(value time.Time) error {
	return c.Conn.SetDeadline(c.clamp(value))
}

func (c *deadlineConn) SetReadDeadline(value time.Time) error {
	return c.Conn.SetReadDeadline(c.clamp(value))
}

func (c *deadlineConn) SetWriteDeadline(value time.Time) error {
	return c.Conn.SetWriteDeadline(c.clamp(value))
}

type smtpReadBudgetConn struct {
	net.Conn
	limit     int64
	used      int64
	exhausted bool
}

func (c *smtpReadBudgetConn) Read(p []byte) (int, error) {
	if c.exhausted {
		return 0, errSMTPInboundLimit
	}
	if len(p) == 0 {
		return c.Conn.Read(p)
	}
	remaining := c.limit - c.used
	if remaining < 0 {
		remaining = 0
	}
	readSize := len(p)
	if int64(readSize) > remaining+1 {
		readSize = int(remaining + 1)
	}
	n, err := c.Conn.Read(p[:readSize])
	c.used += int64(n)
	if int64(n) > remaining {
		c.exhausted = true
		return int(remaining), errSMTPInboundLimit
	}
	return n, err
}

func (c *smtpReadBudgetConn) normalize(err error) error {
	if err != nil && c.exhausted {
		return errSMTPInboundLimit
	}
	return err
}

func (s *smtpClientSession) Auth(address, password string) error {
	if !s.tlsReady {
		return errSMTPTLSUnverified
	}
	if !s.client.SupportsAuth(sasl.Plain) {
		return errSMTPAuthUnavailable
	}
	return s.budget.normalize(s.client.Auth(sasl.NewPlainClient("", address, password)))
}

func (s *smtpClientSession) Mail(from string, size int64, utf8 bool) error {
	if utf8 {
		if supported, _ := s.client.Extension("SMTPUTF8"); !supported {
			return errSMTPUTF8Unavailable
		}
	}
	return s.budget.normalize(s.client.Mail(from, &smtp.MailOptions{Size: size, UTF8: utf8}))
}

func (s *smtpClientSession) Rcpt(address string) error {
	return s.budget.normalize(s.client.Rcpt(address, nil))
}

func (s *smtpClientSession) Reset() error { return s.budget.normalize(s.client.Reset()) }

func (s *smtpClientSession) Data(messageBytes []byte, phase *smtpDataPhase) error {
	command, err := s.client.Data()
	if err != nil {
		return s.budget.normalize(err)
	}
	phase.started = true
	if err := writeSMTPPayload(command, messageBytes); err != nil {
		return err
	}
	return s.budget.normalize(command.Close())
}

func writeSMTPPayload(writer io.Writer, payload []byte) error {
	written, err := writer.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *smtpClientSession) Close() error {
	var err error
	s.once.Do(func() {
		if s.budget.exhausted {
			err = s.budget.Close()
		} else {
			err = s.client.Close()
		}
		close(s.done)
	})
	return err
}

var (
	errSMTPAuthUnavailable = errors.New("SMTP PLAIN authentication unavailable")
	errSMTPUTF8Unavailable = errors.New("SMTPUTF8 unavailable")
	errSMTPTLSUnverified   = errors.New("SMTP TLS verification incomplete")
	errSMTPInboundLimit    = errors.New("SMTP inbound response limit exceeded")
)

type preparedMessage struct {
	bytes      []byte
	messageID  string
	recipients []string
	smtpUTF8   bool
}

func (s *Client) SendMessage(ctx context.Context, input SendInput) (result SendResult, err error) {
	var conn net.Conn
	var dataPhase smtpDataPhase
	defer func() {
		if recover() != nil {
			safeCloseConn(conn)
			if result.Status == SendAccepted {
				err = nil
				return
			}
			if dataPhase.started {
				err = unknownSMTPOutcome(&result)
				return
			}
			result.Status = SendRejected
			err = newError(CodeProtocolError, "SMTP parser returned an unsafe protocol response")
		}
	}()

	if !s.sendEnabled {
		return SendResult{}, newError(CodeAuthorization, "mail send is disabled")
	}
	prepared, err := s.buildMessage(input)
	if err != nil {
		return SendResult{Status: SendRejected}, err
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if !s.sendLimiter.Allow() {
		return SendResult{}, newRetryableError(CodeRateLimited, "local mail send rate limit reached")
	}
	if err := acquire(ctx, s.sendSem); err != nil {
		return SendResult{}, err
	}
	defer func() { <-s.sendSem }()
	result = SendResult{Status: SendRejected, MessageID: prepared.messageID}
	conn, err = s.smtpDial(ctx)
	if err != nil {
		return result, mapSMTPError(err, ctx, false, "submission connection failed")
	}
	session, err := s.smtpFactory(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return result, mapSMTPError(err, ctx, false, "STARTTLS negotiation failed")
	}
	defer safeCloseSMTPSession(session, conn)
	if err := session.Auth(s.config.Address, s.config.Password); err != nil {
		return result, mapSMTPAuthError(err, ctx)
	}
	if err := session.Mail(s.config.Address, int64(len(prepared.bytes)), prepared.smtpUTF8); err != nil {
		return result, mapSMTPError(err, ctx, false, "envelope sender was rejected")
	}
	var firstRejection error
	for index, recipient := range prepared.recipients {
		rcptErr := session.Rcpt(recipient)
		status := RecipientStatus{Index: index, Accepted: rcptErr == nil}
		if rcptErr != nil {
			status.Category = "rejected"
			if firstRejection == nil {
				firstRejection = rcptErr
			}
		}
		result.Recipients = append(result.Recipients, status)
		if rcptErr != nil && !definitiveSMTPReply(rcptErr) {
			for remaining := index + 1; remaining < len(prepared.recipients); remaining++ {
				result.Recipients = append(result.Recipients, RecipientStatus{Index: remaining, Category: "not_attempted"})
			}
			_ = session.Reset()
			return result, mapSMTPError(rcptErr, ctx, false, "recipient command failed")
		}
	}
	if firstRejection != nil {
		_ = session.Reset()
		return result, mapSMTPError(firstRejection, ctx, false, "one or more recipients were rejected")
	}
	err = session.Data(prepared.bytes, &dataPhase)
	if err != nil {
		if dataPhase.started && !definitiveSMTPReply(err) {
			return result, unknownSMTPOutcome(&result)
		}
		message := "DATA command failed"
		if dataPhase.started {
			message = "message data was rejected"
		}
		return result, mapSMTPError(err, ctx, dataPhase.started, message)
	}
	result.Status = SendAccepted
	result.SentCopyUnavailable = true
	return result, nil
}

func unknownSMTPOutcome(result *SendResult) error {
	result.Status = SendOutcomeUnknown
	result.Reconciliation = "Check Sent and the recipients before deciding whether to send again."
	return &Error{Code: CodeOutcomeUnknown, Message: "message submission outcome is unknown", Reconciliation: result.Reconciliation}
}

func safeCloseSMTPSession(session smtpSession, conn net.Conn) {
	defer func() {
		if recover() != nil {
			safeCloseConn(conn)
		}
	}()
	if err := session.Close(); err != nil {
		safeCloseConn(conn)
	}
}

func (s *Client) buildMessage(input SendInput) (preparedMessage, error) {
	to, recipients, seen, err := s.validateRecipientGroup("to", input.To, nil, nil)
	if err != nil {
		return preparedMessage{}, err
	}
	cc, recipients, seen, err := s.validateRecipientGroup("cc", input.Cc, recipients, seen)
	if err != nil {
		return preparedMessage{}, err
	}
	_, recipients, _, err = s.validateRecipientGroup("bcc", input.Bcc, recipients, seen)
	if err != nil {
		return preparedMessage{}, err
	}
	if len(recipients) == 0 {
		return preparedMessage{}, validationError("at least one recipient is required")
	}
	if len(recipients) > MaxRecipients {
		return preparedMessage{}, validationError("recipient count exceeds 50")
	}
	if len(input.Subject) > MaxSubjectBytes || !utf8.ValidString(input.Subject) || hasDisallowedControl(input.Subject, false) {
		return preparedMessage{}, validationError("subject contains invalid or excessive data")
	}
	if len(input.Body) > MaxSendBodyBytes || !utf8.ValidString(input.Body) || strings.ContainsRune(input.Body, '\x00') {
		return preparedMessage{}, validationError("body contains invalid or excessive data")
	}
	messageID, err := generateMessageID(s.config.Address)
	if err != nil {
		return preparedMessage{}, newError(CodeInternalError, "could not generate a message identifier")
	}
	var header msgmail.Header
	header.SetAddressList("From", []*msgmail.Address{{Address: s.config.Address}})
	header.SetAddressList("To", to)
	header.SetAddressList("Cc", cc)
	header.SetDate(s.now())
	header.SetMessageID(messageID)
	header.SetSubject(input.Subject)
	header.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	var buffer bytes.Buffer
	writer, err := msgmail.CreateSingleInlineWriter(&buffer, header)
	if err != nil {
		return preparedMessage{}, newError(CodeInternalError, "could not encode the message")
	}
	if _, err := io.WriteString(writer, input.Body); err != nil {
		_ = writer.Close()
		return preparedMessage{}, newError(CodeInternalError, "could not encode the message")
	}
	if err := writer.Close(); err != nil {
		return preparedMessage{}, newError(CodeInternalError, "could not encode the message")
	}
	if buffer.Len() > MaxEncodedMessage {
		return preparedMessage{}, newError(CodePayloadTooLarge, "encoded message exceeds 256 KiB")
	}
	smtpUTF8 := containsNonASCII(s.config.Address)
	for _, recipient := range recipients {
		smtpUTF8 = smtpUTF8 || containsNonASCII(recipient)
	}
	return preparedMessage{bytes: append([]byte(nil), buffer.Bytes()...), messageID: messageID, recipients: recipients, smtpUTF8: smtpUTF8}, nil
}

func containsNonASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

func (s *Client) validateRecipientGroup(name string, values []string, recipients []string, seen map[string]struct{}) ([]*msgmail.Address, []string, map[string]struct{}, error) {
	if seen == nil {
		seen = make(map[string]struct{})
	}
	addresses := make([]*msgmail.Address, 0, len(values))
	for _, value := range values {
		address, err := validateAddrSpec(value)
		if err != nil {
			return nil, nil, nil, validationError(name + " contains an invalid plain addr-spec")
		}
		key := asciiLower(address)
		if _, duplicate := seen[key]; duplicate {
			return nil, nil, nil, validationError("recipient addresses must be unique")
		}
		if !s.config.RecipientPolicy.allows(address) {
			return nil, nil, nil, newError(CodeAuthorization, "recipient is not permitted by the configured allowlist")
		}
		seen[key] = struct{}{}
		recipients = append(recipients, address)
		addresses = append(addresses, &msgmail.Address{Address: address})
	}
	return addresses, recipients, seen, nil
}

func generateMessageID(address string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	domain := "localhost"
	if at := strings.LastIndexByte(address, '@'); at >= 0 && at+1 < len(address) {
		domain = address[at+1:]
	}
	return hex.EncodeToString(nonce) + "@" + domain, nil
}

func definitiveSMTPReply(err error) bool {
	var smtpErr *smtp.SMTPError
	return errors.As(err, &smtpErr) && smtpErr.Code >= 400 && smtpErr.Code <= 599
}

func mapSMTPAuthError(err error, ctx context.Context) error {
	if errors.Is(err, errSMTPInboundLimit) || errors.Is(err, smtp.ErrTooLongLine) {
		return newError(CodePayloadTooLarge, "SMTP response exceeded the inbound limit")
	}
	if errors.Is(err, errSMTPAuthUnavailable) {
		return newError(CodeProtocolError, "SMTP PLAIN authentication is unavailable after STARTTLS")
	}
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		switch smtpErr.Code / 100 {
		case 4:
			return newRetryableError(CodeUnavailable, "SMTP authentication is temporarily unavailable")
		case 5:
			return newError(CodeAuthentication, "SMTP authentication was rejected")
		default:
			return newError(CodeProtocolError, "SMTP server returned an unexpected authentication response")
		}
	}
	if ctx.Err() != nil {
		return contextError()
	}
	return mapSMTPError(err, ctx, false, "SMTP authentication failed")
}

func mapSMTPError(err error, ctx context.Context, dataStarted bool, message string) error {
	if dataStarted && !definitiveSMTPReply(err) {
		return &Error{Code: CodeOutcomeUnknown, Message: "message submission outcome is unknown", Reconciliation: "Check Sent and the recipients before deciding whether to send again."}
	}
	if errors.Is(err, errSMTPInboundLimit) || errors.Is(err, smtp.ErrTooLongLine) {
		return newError(CodePayloadTooLarge, "SMTP response exceeded the inbound limit")
	}
	if errors.Is(err, errSMTPUTF8Unavailable) {
		return newError(CodeProtocolError, "SMTPUTF8 is required for an internationalized address but was not advertised")
	}
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		switch {
		case smtpErr.Code/100 == 4:
			if !dataStarted {
				return newRetryableError(CodeUnavailable, message)
			}
			return newError(CodeUnavailable, message)
		case smtpErr.Code == 535:
			return newError(CodeAuthentication, "SMTP authentication was rejected")
		case smtpErr.Code == 552:
			return newError(CodePayloadTooLarge, "SMTP server rejected the message size")
		case smtpErr.Code/100 == 5:
			return newError(CodeAuthorization, message)
		default:
			return newError(CodeProtocolError, message)
		}
	}
	if ctx.Err() != nil {
		return contextError()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return contextError()
	}
	if !dataStarted {
		return newRetryableError(CodeUnavailable, message)
	}
	return newError(CodeUnavailable, message)
}
