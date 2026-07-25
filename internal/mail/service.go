package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"golang.org/x/time/rate"
)

// Client is the concrete, concurrency-safe Mail Service implementation.
type Client struct {
	config          Config
	imapDial        IMAPDialFunc
	smtpDial        SMTPDialFunc
	imapFactory     imapSessionFactory
	smtpFactory     smtpSessionFactory
	mutationEnabled bool
	sendEnabled     bool
	readLimiter     *rate.Limiter
	mutationLimiter *rate.Limiter
	sendLimiter     *rate.Limiter
	readSem         chan struct{}
	mutationSem     chan struct{}
	sendSem         chan struct{}
	now             func() time.Time
}

var _ Service = (*Client)(nil)

// NewService validates and copies config. It performs no network access.
func NewService(config Config, imapDial IMAPDialFunc, smtpDial SMTPDialFunc, enableMutation, enableSend bool) (*Client, error) {
	return newService(config, imapDial, smtpDial, enableMutation, enableSend,
		func(ctx context.Context, conn net.Conn, address, password string) (imapSession, error) {
			return imapadapter.NewSession(ctx, conn, address, password)
		}, newSMTPSession)
}

func newService(config Config, imapDial IMAPDialFunc, smtpDial SMTPDialFunc, enableMutation, enableSend bool, imapFactory imapSessionFactory, smtpFactory smtpSessionFactory) (*Client, error) {
	address, err := validateAddrSpec(config.Address)
	if err != nil {
		return nil, validationError("mail address must be a plain addr-spec")
	}
	if len(config.Password) < 8 {
		return nil, validationError("mail password must be at least 8 characters")
	}
	if imapDial == nil || imapFactory == nil {
		return nil, validationError("an IMAP dialer is required")
	}
	if enableSend && (!config.RecipientPolicy.valid() || smtpDial == nil || smtpFactory == nil) {
		return nil, validationError("mail send requires an SMTP dialer and recipient policy")
	}
	config.Address = address
	config.RecipientPolicy = config.RecipientPolicy.clone()
	return &Client{
		config: config, imapDial: imapDial, smtpDial: smtpDial,
		imapFactory: imapFactory, smtpFactory: smtpFactory,
		mutationEnabled: enableMutation, sendEnabled: enableSend,
		readLimiter:     rate.NewLimiter(rate.Every(time.Minute/60), 10),
		mutationLimiter: rate.NewLimiter(rate.Every(time.Minute/20), 3),
		sendLimiter:     rate.NewLimiter(rate.Every(time.Minute/20), 3),
		readSem:         make(chan struct{}, 2), mutationSem: make(chan struct{}, 1), sendSem: make(chan struct{}, 1),
		now: time.Now,
	}, nil
}

func acquire(ctx context.Context, semaphore chan struct{}) error {
	select {
	case semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return contextError()
	}
}

func contextError() *Error {
	return newRetryableError(CodeTimeout, "mail operation deadline reached")
}

func (s *Client) read(ctx context.Context, operation func(imapSession) (any, error)) (any, error) {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if !s.readLimiter.Allow() {
			return nil, newRetryableError(CodeRateLimited, "local mail read rate limit reached")
		}
		result, err := s.readWithSlot(ctx, operation)
		if err == nil {
			return result, nil
		}
		if attempt == 0 && retryableRead(err, ctx) {
			continue
		}
		return nil, mapIMAPError(err, ctx, false, "")
	}
	return nil, newError(CodeInternalError, "mail read failed")
}

func (s *Client) readWithSlot(ctx context.Context, operation func(imapSession) (any, error)) (any, error) {
	if err := acquire(ctx, s.readSem); err != nil {
		return nil, err
	}
	defer func() { <-s.readSem }()
	return s.imapAttempt(ctx, nil, operation)
}

type imapMutationPhase struct {
	dispatched bool
}

func (s *Client) imapAttempt(ctx context.Context, phase *imapMutationPhase, operation func(imapSession) (any, error)) (result any, err error) {
	var conn net.Conn
	var session imapSession
	defer func() {
		if recover() != nil {
			safeCloseConn(conn)
			result = nil
			// Preserve a classified failure (e.g. authentication) when cleanup
			// panics after the factory already closed the connection.
			if err == nil {
				err = &imapadapter.Error{Kind: imapadapter.ErrorProtocol, Ambiguous: phase != nil && phase.dispatched}
			}
			return
		}
		if session != nil && closeIMAPSession(session) {
			safeCloseConn(conn)
			// The operation result is already definitive. A cleanup panic must not
			// turn a successful read or mutation into a retryable-looking failure.
		}
	}()

	conn, err = s.imapDial(ctx)
	if err != nil {
		return nil, err
	}
	session, err = s.imapFactory(ctx, conn, s.config.Address, s.config.Password)
	if err != nil {
		// NewSession closes the conn on failure; use a panic-safe close so a
		// second Close cannot rewrite authentication or other factory errors.
		safeCloseConn(conn)
		return nil, err
	}
	return operation(session)
}

func closeIMAPSession(session imapSession) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	_ = session.Close()
	return false
}

func retryableRead(err error, ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	var protocolErr *imapadapter.Error
	if errors.As(err, &protocolErr) {
		return protocolErr.Kind == imapadapter.ErrorUnavailable
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (s *Client) mutate(ctx context.Context, reconciliation string, operation func(imapSession, *imapMutationPhase) (any, error)) (any, error) {
	if !s.mutationEnabled {
		return nil, newError(CodeAuthorization, "mail mutation is disabled")
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if !s.mutationLimiter.Allow() {
		return nil, newRetryableError(CodeRateLimited, "local mail mutation rate limit reached")
	}
	if err := acquire(ctx, s.mutationSem); err != nil {
		return nil, err
	}
	defer func() { <-s.mutationSem }()
	phase := &imapMutationPhase{}
	result, err := s.imapAttempt(ctx, phase, func(session imapSession) (any, error) {
		return operation(session, phase)
	})
	if err != nil {
		return nil, mapIMAPError(err, ctx, phase.dispatched, reconciliation)
	}
	return result, nil
}

func safeCloseConn(conn net.Conn) {
	if conn == nil {
		return
	}
	defer func() { _ = recover() }()
	_ = conn.Close()
}

func mapIMAPError(err error, ctx context.Context, mutation bool, reconciliation string) error {
	if err == nil {
		return nil
	}
	var public *Error
	if errors.As(err, &public) {
		return public
	}
	var protocolErr *imapadapter.Error
	if errors.As(err, &protocolErr) {
		if mutation && protocolErr.Ambiguous {
			return &Error{Code: CodeOutcomeUnknown, Message: "mail mutation outcome is unknown", Reconciliation: reconciliation}
		}
		code := CodeProtocolError
		message := "mail server returned an unsafe or unexpected protocol response"
		switch protocolErr.Kind {
		case imapadapter.ErrorAuthentication:
			code, message = CodeAuthentication, "mail authentication was rejected"
		case imapadapter.ErrorAuthorization:
			code, message = CodeAuthorization, "mail operation was not authorized"
		case imapadapter.ErrorNotFound:
			code, message = CodeNotFound, "mail resource was not found"
		case imapadapter.ErrorConflict:
			code, message = CodeConflict, "mail operation conflicts with current state"
		case imapadapter.ErrorRateLimited:
			code, message = CodeRateLimited, "mail server rate limit reached"
		case imapadapter.ErrorTimeout:
			code, message = CodeTimeout, "mail operation deadline reached"
		case imapadapter.ErrorUnavailable:
			code, message = CodeUnavailable, "mail service is temporarily unavailable"
		case imapadapter.ErrorTooLarge:
			code, message = CodePayloadTooLarge, "mail protocol data exceeded a safety limit"
		}
		if !mutation && (code == CodeRateLimited || code == CodeTimeout || code == CodeUnavailable) {
			return newRetryableError(code, message)
		}
		return newError(code, message)
	}
	if ctx.Err() != nil {
		if mutation {
			return newError(CodeTimeout, "mail operation deadline reached")
		}
		return contextError()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if mutation {
			return newError(CodeTimeout, "mail operation deadline reached")
		}
		return contextError()
	}
	if !mutation {
		return newRetryableError(CodeUnavailable, "mail service is temporarily unavailable")
	}
	return newError(CodeUnavailable, "mail service is temporarily unavailable")
}

func (s *Client) ListMailboxes(ctx context.Context) (ListMailboxesResult, error) {
	value, err := s.read(ctx, func(session imapSession) (any, error) {
		items, err := session.List()
		if err != nil {
			return nil, err
		}
		result := ListMailboxesResult{Truncated: len(items) > MaxMailboxes}
		if len(items) > MaxMailboxes {
			items = items[:MaxMailboxes]
		}
		for _, item := range items {
			delimiter := ""
			if item.Delimiter != 0 {
				delimiter = string(item.Delimiter)
			}
			result.Mailboxes = append(result.Mailboxes, Mailbox{Name: item.Name, Delimiter: delimiter, Attributes: append([]string(nil), item.Attributes...)})
		}
		for serializedSize(result) > MaxResultBytes && len(result.Mailboxes) > 0 {
			result.Mailboxes = result.Mailboxes[:len(result.Mailboxes)-1]
			result.Truncated = true
		}
		return result, nil
	})
	if err != nil {
		return ListMailboxesResult{}, err
	}
	return value.(ListMailboxesResult), nil
}

func serializedSize(value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		return MaxResultBytes + 1
	}
	return len(data)
}

func (s *Client) SearchMessages(ctx context.Context, input SearchInput) (SearchResult, error) {
	if err := validateSearchInput(input); err != nil {
		return SearchResult{}, err
	}
	limit := input.Limit
	if limit == 0 {
		limit = 20
	}
	value, err := s.read(ctx, func(session imapSession) (any, error) {
		selected, err := session.Select(input.Mailbox, true)
		if err != nil {
			return nil, err
		}
		if input.BeforeUID != 0 && selected.UIDValidity != input.UIDValidity {
			return nil, newError(CodeConcurrentModification, "mailbox UIDVALIDITY changed; restart pagination")
		}
		result := SearchResult{UIDValidity: selected.UIDValidity}
		high := selected.UIDNext - 1
		if input.BeforeUID != 0 && input.BeforeUID-1 < high {
			high = input.BeforeUID - 1
		}
		const window = uint32(5000)
		var found []uint32
		seen := make(map[uint32]struct{})
		scanned := 0
		for high > 0 && scanned < MaxUIDsScanned && len(found) <= limit {
			remaining := MaxUIDsScanned - scanned
			width := int(window)
			if remaining < width {
				width = remaining
			}
			low := uint32(1)
			if high >= uint32(width) {
				low = high - uint32(width) + 1
			}
			uids, searchErr := session.Search(imapadapter.SearchRequest{
				UIDMin: low, UIDMax: high, Query: input.Query, From: input.From, To: input.To,
				Subject: input.Subject, Since: input.Since, Before: input.Before,
				UnseenOnly: input.UnseenOnly, FlaggedOnly: input.FlaggedOnly,
			})
			if searchErr != nil {
				return nil, searchErr
			}
			for _, uid := range uids {
				if uid < low || uid > high || uid == 0 {
					return nil, &imapadapter.Error{Kind: imapadapter.ErrorProtocol}
				}
				if _, duplicate := seen[uid]; !duplicate {
					seen[uid] = struct{}{}
					found = append(found, uid)
				}
			}
			scanned += int(high-low) + 1
			if low == 1 {
				high = 0
			} else {
				high = low - 1
			}
		}
		sort.Slice(found, func(i, j int) bool { return found[i] > found[j] })
		if len(found) > limit {
			result.Truncated = true
			found = found[:limit]
		}
		result.ScanLimitReached = scanned == MaxUIDsScanned && high > 0
		if len(found) > 0 {
			metadata, fetchErr := session.FetchMetadata(found, false)
			if fetchErr != nil {
				return nil, fetchErr
			}
			for _, item := range metadata {
				result.Messages = append(result.Messages, summaryFromProtocol(input.Mailbox, selected.UIDValidity, item))
			}
			sort.Slice(result.Messages, func(i, j int) bool { return result.Messages[i].UID > result.Messages[j].UID })
			result.NextBeforeUID = found[len(found)-1]
		}
		for serializedSize(result) > MaxResultBytes && len(result.Messages) > 0 {
			result.Messages = result.Messages[:len(result.Messages)-1]
			result.Truncated = true
			if len(result.Messages) > 0 {
				result.NextBeforeUID = result.Messages[len(result.Messages)-1].UID
			}
		}
		return result, nil
	})
	if err != nil {
		return SearchResult{}, err
	}
	return value.(SearchResult), nil
}

func validateSearchInput(input SearchInput) error {
	if err := validateMailbox(input.Mailbox); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{{"query", input.Query}, {"from", input.From}, {"to", input.To}, {"subject", input.Subject}} {
		if err := validateSearchValue(field.name, field.value); err != nil {
			return err
		}
	}
	if !input.Since.IsZero() && !input.Before.IsZero() && !input.Before.After(input.Since) {
		return validationError("before must be after since")
	}
	if input.BeforeUID != 0 && input.UIDValidity == 0 {
		return validationError("uid_validity is required with before_uid")
	}
	if input.Limit < 0 || input.Limit > MaxSearchResults {
		return validationError("limit must be between 1 and 50")
	}
	return nil
}

func summaryFromProtocol(mailbox string, uidValidity uint32, item imapadapter.Message) MessageSummary {
	summary := MessageSummary{
		Mailbox: mailbox, UIDValidity: uidValidity, UID: item.UID, Flags: publicFlags(item.Flags),
		From: publicAddresses(item.Envelope.From), To: publicAddresses(item.Envelope.To), Cc: publicAddresses(item.Envelope.Cc),
		Subject: item.Envelope.Subject, InternalDate: item.InternalDate, Size: item.Size,
		MessageID: item.Envelope.MessageID, ModSeq: item.ModSeq,
	}
	if !item.Envelope.Date.IsZero() {
		date := item.Envelope.Date
		summary.HeaderDate = &date
	}
	return summary
}

func publicAddresses(in []imapadapter.Address) []Address {
	out := make([]Address, len(in))
	for i, address := range in {
		out[i] = Address{Name: address.Name, Address: address.Address}
	}
	return out
}

func publicFlags(in []string) []MessageFlag {
	var out []MessageFlag
	for _, flag := range in {
		switch strings.ToUpper(flag) {
		case "\\SEEN":
			out = append(out, FlagSeen)
		case "\\FLAGGED":
			out = append(out, FlagFlagged)
		case "\\ANSWERED":
			out = append(out, FlagAnswered)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
