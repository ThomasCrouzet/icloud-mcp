package imapadapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	msgmail "github.com/emersion/go-message/mail"
)

const (
	maxMetadataString   = 4 * 1024
	maxAddresses        = 100
	maxParts            = 200
	maxDepth            = 20
	maxHeaderBytes      = 64 * 1024
	maxWireBodyBytes    = 512 * 1024
	maxMailboxResponses = 201
	maxMetadataMessages = 50
	maxSearchWindowUIDs = 5000
)

// Session is one fresh authenticated IMAP connection.
type Session struct {
	client        *imapclient.Client
	conn          net.Conn
	ctx           context.Context
	caps          Capabilities
	done          chan struct{}
	once          sync.Once
	authenticated bool
	bounded       bool
}

// NewSession authenticates using the address local part first, then retries
// with the full addr-spec after an authentication rejection. iCloud often
// rejects the local part with an uncoded NO while accepting the full email
// address with the same app-specific password.
func NewSession(ctx context.Context, conn net.Conn, address, password string) (*Session, error) {
	deadline, bounded := ctx.Deadline()
	clamped := &deadlineConn{Conn: conn, deadline: deadline}
	if bounded {
		if err := clamped.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, classify(err, false)
		}
	}
	guarded := newGuardedConn(clamped)
	client := imapclient.New(guarded, &imapclient.Options{
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
	})
	s := &Session{client: client, conn: guarded, ctx: ctx, done: make(chan struct{}), bounded: bounded}
	go func() {
		select {
		case <-ctx.Done():
			_ = guarded.Close()
		case <-s.done:
		}
	}()
	if err := client.WaitGreeting(); err != nil {
		_ = s.Close()
		return nil, classify(err, false)
	}
	local := address
	if at := strings.LastIndexByte(address, '@'); at > 0 {
		local = address[:at]
	}
	err := client.Login(local, password).Wait()
	if err != nil && loginRejection(err) && local != address {
		err = client.Login(address, password).Wait()
	}
	if err != nil {
		_ = s.Close()
		if loginRejection(err) {
			return nil, &Error{Kind: ErrorAuthentication}
		}
		return nil, classify(err, false)
	}
	s.authenticated = true
	caps := client.Caps()
	if caps == nil {
		_ = s.Close()
		return nil, &Error{Kind: ErrorProtocol}
	}
	s.caps = Capabilities{
		CondStore:  caps.Has(imap.CapCondStore),
		Move:       caps.Has(imap.CapMove),
		UIDPlus:    caps.Has(imap.CapUIDPlus),
		Binary:     caps.Has(imap.CapBinary),
		SpecialUse: caps.Has(imap.CapSpecialUse),
	}
	return s, nil
}

func explicitAuthRejection(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeNo && imapErr.Code == imap.ResponseCodeAuthenticationFailed
}

func loginRejection(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeNo &&
		(imapErr.Code == "" || imapErr.Code == imap.ResponseCodeAuthenticationFailed)
}

func (s *Session) Capabilities() Capabilities { return s.caps }

// SupportsModifiedDetection is false for beta.8: tagged MODIFIED is discarded.
func (s *Session) SupportsModifiedDetection() bool { return false }

func (s *Session) List() ([]Mailbox, error) {
	var options *imap.ListOptions
	if s.caps.SpecialUse {
		options = &imap.ListOptions{ReturnSpecialUse: true}
	}
	command := s.client.List("", "*", options)
	defer func() { _ = command.Close() }()
	out := make([]Mailbox, 0, maxMailboxResponses)
	for {
		item := command.Next()
		if item == nil {
			break
		}
		// maxMailboxResponses is MaxMailboxes+1: the overflow row fails closed
		// so Trash/move discovery cannot miss SPECIAL-USE targets.
		if len(out) >= maxMailboxResponses-1 {
			return nil, &Error{Kind: ErrorTooLarge}
		}
		if err := boundedString(item.Mailbox); err != nil {
			return nil, err
		}
		attrs := make([]string, len(item.Attrs))
		for i, attr := range item.Attrs {
			attrs[i] = string(attr)
			if err := boundedString(attrs[i]); err != nil {
				return nil, err
			}
		}
		out = append(out, Mailbox{Name: item.Mailbox, Delimiter: item.Delim, Attributes: attrs})
	}
	if err := command.Close(); err != nil {
		return nil, classify(err, false)
	}
	return out, nil
}

func (s *Session) Select(mailbox string, readOnly bool) (SelectedMailbox, error) {
	data, err := s.client.Select(mailbox, &imap.SelectOptions{ReadOnly: readOnly, CondStore: s.caps.CondStore}).Wait()
	if err != nil {
		return SelectedMailbox{}, classify(err, false)
	}
	if data.UIDValidity == 0 || data.UIDNext == 0 {
		return SelectedMailbox{}, &Error{Kind: ErrorProtocol}
	}
	return SelectedMailbox{UIDValidity: data.UIDValidity, UIDNext: uint32(data.UIDNext)}, nil
}

func (s *Session) Search(request SearchRequest) ([]uint32, error) {
	if request.UIDMin == 0 || request.UIDMax < request.UIDMin {
		return nil, &Error{Kind: ErrorProtocol}
	}
	windowSize := uint64(request.UIDMax) - uint64(request.UIDMin) + 1
	if windowSize > maxSearchWindowUIDs {
		return nil, &Error{Kind: ErrorTooLarge}
	}
	var uidSet imap.UIDSet
	uidSet.AddRange(imap.UID(request.UIDMin), imap.UID(request.UIDMax))
	criteria := &imap.SearchCriteria{
		UID:    []imap.UIDSet{uidSet},
		Since:  request.Since,
		Before: request.Before,
	}
	if request.Query != "" {
		criteria.Text = []string{request.Query}
	}
	for _, field := range []struct{ key, value string }{
		{"FROM", request.From}, {"TO", request.To}, {"SUBJECT", request.Subject},
	} {
		if field.value != "" {
			criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: field.key, Value: field.value})
		}
	}
	if request.UnseenOnly {
		criteria.NotFlag = append(criteria.NotFlag, imap.FlagSeen)
	}
	if request.FlaggedOnly {
		criteria.Flag = append(criteria.Flag, imap.FlagFlagged)
	}
	data, err := s.client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, classify(err, false)
	}
	if data == nil {
		return nil, &Error{Kind: ErrorProtocol}
	}
	out, err := expandUIDSet(data.All, request.UIDMin, request.UIDMax, maxSearchWindowUIDs)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

func expandUIDSet(numSet imap.NumSet, requestedMin, requestedMax uint32, maxCount uint64) ([]uint32, error) {
	set, ok := numSet.(imap.UIDSet)
	if !ok || imap.IsSearchRes(set) || requestedMin == 0 || requestedMax < requestedMin || maxCount == 0 {
		return nil, &Error{Kind: ErrorProtocol}
	}
	if uint64(len(set)) > maxCount {
		return nil, &Error{Kind: ErrorTooLarge}
	}

	minUID, maxUID := uint64(requestedMin), uint64(requestedMax)
	var cardinality, previousStop uint64
	for index, uidRange := range set {
		start, stop := uint64(uidRange.Start), uint64(uidRange.Stop)
		if start == 0 || stop == 0 || start > stop {
			return nil, &Error{Kind: ErrorProtocol}
		}
		if start < minUID || stop > maxUID || index > 0 && start <= previousStop {
			return nil, &Error{Kind: ErrorProtocol}
		}
		rangeSize := stop - start + 1
		if rangeSize > maxCount-cardinality {
			return nil, &Error{Kind: ErrorTooLarge}
		}
		cardinality += rangeSize
		previousStop = stop
	}

	out := make([]uint32, 0, int(cardinality))
	for _, uidRange := range set {
		for uid := uint64(uidRange.Start); uid <= uint64(uidRange.Stop); uid++ {
			out = append(out, uint32(uid))
		}
	}
	return out, nil
}

func (s *Session) FetchMetadata(uids []uint32, includeHeaders bool) ([]Message, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if len(uids) > maxMetadataMessages {
		return nil, &Error{Kind: ErrorTooLarge}
	}
	set := make(imap.UIDSet, 0, len(uids))
	expected := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return nil, &Error{Kind: ErrorProtocol}
		}
		if _, duplicate := expected[uid]; duplicate {
			return nil, &Error{Kind: ErrorProtocol}
		}
		expected[uid] = struct{}{}
		set.AddNum(imap.UID(uid))
	}
	options := &imap.FetchOptions{
		UID:           true,
		Flags:         true,
		Envelope:      true,
		InternalDate:  true,
		RFC822Size:    true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		ModSeq:        s.caps.CondStore,
	}
	if includeHeaders {
		options.BodySection = []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"Message-ID", "In-Reply-To", "References", "Reply-To"},
			Partial:      &imap.SectionPartial{Size: maxHeaderBytes + 1},
			Peek:         true,
		}}
	}
	command := s.client.Fetch(set, options)
	defer func() { _ = command.Close() }()
	out := make([]Message, 0, len(uids))
	seen := make(map[uint32]struct{}, len(uids))
	for {
		messageData := command.Next()
		if messageData == nil {
			break
		}
		if len(seen) >= len(expected) {
			return nil, &Error{Kind: ErrorProtocol}
		}
		buffer, err := messageData.Collect()
		if err != nil {
			return nil, classify(err, false)
		}
		uid := uint32(buffer.UID)
		if uid == 0 {
			return nil, &Error{Kind: ErrorProtocol}
		}
		if _, requested := expected[uid]; !requested {
			return nil, &Error{Kind: ErrorProtocol}
		}
		if _, duplicate := seen[uid]; duplicate {
			return nil, &Error{Kind: ErrorProtocol}
		}
		seen[uid] = struct{}{}
		converted, err := convertMessage(buffer, includeHeaders)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	if err := command.Close(); err != nil {
		return nil, classify(err, false)
	}
	return out, nil
}

func convertMessage(buffer *imapclient.FetchMessageBuffer, includeHeaders bool) (Message, error) {
	if buffer.UID == 0 || buffer.Envelope == nil || buffer.BodyStructure == nil {
		return Message{}, &Error{Kind: ErrorProtocol}
	}
	envelope, err := convertEnvelope(buffer.Envelope)
	if err != nil {
		return Message{}, err
	}
	parts, err := flattenBodyStructure(buffer.BodyStructure)
	if err != nil {
		return Message{}, err
	}
	msg := Message{
		UID: uint32(buffer.UID), Envelope: envelope, InternalDate: buffer.InternalDate,
		Size: buffer.RFC822Size, ModSeq: buffer.ModSeq, Parts: parts,
	}
	for _, flag := range buffer.Flags {
		msg.Flags = append(msg.Flags, string(flag))
	}
	if includeHeaders {
		if len(buffer.BodySection) != 1 || len(buffer.BodySection[0].Bytes) > maxHeaderBytes {
			return Message{}, &Error{Kind: ErrorTooLarge}
		}
		headers, err := parseCuratedHeaders(buffer.BodySection[0].Bytes)
		if err != nil {
			return Message{}, err
		}
		msg.Headers = headers
	}
	return msg, nil
}

func convertEnvelope(in *imap.Envelope) (Envelope, error) {
	if err := boundedString(in.Subject); err != nil {
		return Envelope{}, err
	}
	from, err := convertAddresses(in.From)
	if err != nil {
		return Envelope{}, err
	}
	replyTo, err := convertAddresses(in.ReplyTo)
	if err != nil {
		return Envelope{}, err
	}
	to, err := convertAddresses(in.To)
	if err != nil {
		return Envelope{}, err
	}
	cc, err := convertAddresses(in.Cc)
	if err != nil {
		return Envelope{}, err
	}
	if len(from)+len(replyTo)+len(to)+len(cc)+len(in.Bcc)+len(in.Sender) > maxAddresses {
		return Envelope{}, &Error{Kind: ErrorTooLarge}
	}
	if err := boundedString(in.MessageID); err != nil {
		return Envelope{}, err
	}
	for _, id := range in.InReplyTo {
		if err := boundedString(id); err != nil {
			return Envelope{}, err
		}
	}
	return Envelope{Date: in.Date, Subject: in.Subject, From: from, ReplyTo: replyTo, To: to, Cc: cc, MessageID: in.MessageID, InReplyTo: in.InReplyTo}, nil
}

func convertAddresses(in []imap.Address) ([]Address, error) {
	if len(in) > maxAddresses {
		return nil, &Error{Kind: ErrorTooLarge}
	}
	out := make([]Address, 0, len(in))
	for _, addr := range in {
		value := addr.Addr()
		if value == "" {
			continue
		}
		if err := boundedString(addr.Name); err != nil {
			return nil, err
		}
		if err := boundedString(value); err != nil {
			return nil, err
		}
		out = append(out, Address{Name: addr.Name, Address: value})
	}
	return out, nil
}

func flattenBodyStructure(root imap.BodyStructure) ([]BodyPart, error) {
	parts := make([]BodyPart, 0, 8)
	var walk func(imap.BodyStructure, []int, bool) error
	walk = func(part imap.BodyStructure, path []int, excluded bool) error {
		if len(parts) >= maxParts || len(path) > maxDepth {
			return &Error{Kind: ErrorTooLarge}
		}
		disposition := ""
		if d := part.Disposition(); d != nil {
			disposition = strings.ToLower(d.Value)
			if err := boundedString(disposition); err != nil {
				return err
			}
		}
		if disposition == "attachment" {
			excluded = true
		}
		switch typed := part.(type) {
		case *imap.BodyStructureSinglePart:
			filename := typed.Filename()
			for _, value := range []string{typed.Type, typed.Subtype, typed.ID, filename, typed.Description, typed.Encoding} {
				if err := boundedString(value); err != nil {
					return err
				}
			}
			for key, value := range typed.Params {
				if err := boundedString(key); err != nil {
					return err
				}
				if err := boundedString(value); err != nil {
					return err
				}
			}
			mediaType := typed.MediaType()
			attachment := excluded || filename != "" || mediaType == "message/rfc822" || mediaType == "message/global"
			pathCopy := append([]int(nil), path...)
			parts = append(parts, BodyPart{
				Path: pathCopy, ContentType: mediaType, Disposition: disposition,
				Filename: filename, ContentID: typed.ID, Size: typed.Size,
				InlinePlain: mediaType == "text/plain" && !attachment,
				Attachment:  attachment,
			})
		case *imap.BodyStructureMultiPart:
			parts = append(parts, BodyPart{Path: append([]int(nil), path...), ContentType: typed.MediaType(), Disposition: disposition, Attachment: excluded})
			if excluded {
				return nil
			}
			for i, child := range typed.Children {
				if err := walk(child, append(path, i+1), false); err != nil {
					return err
				}
			}
		default:
			return &Error{Kind: ErrorProtocol}
		}
		return nil
	}
	startPath := []int{1}
	if _, multipart := root.(*imap.BodyStructureMultiPart); multipart {
		startPath = nil
	}
	if err := walk(root, startPath, false); err != nil {
		return nil, err
	}
	return parts, nil
}

func parseCuratedHeaders(data []byte) (CuratedHeaders, error) {
	entity, err := message.ReadWithOptions(bytes.NewReader(data), &message.ReadOptions{MaxHeaderBytes: maxHeaderBytes})
	if err != nil {
		return CuratedHeaders{}, &Error{Kind: ErrorProtocol}
	}
	header := msgmail.Header{Header: entity.Header}
	messageID, err := header.MessageID()
	if err != nil {
		return CuratedHeaders{}, &Error{Kind: ErrorProtocol}
	}
	inReplyTo, err := header.MsgIDList("In-Reply-To")
	if err != nil {
		return CuratedHeaders{}, &Error{Kind: ErrorProtocol}
	}
	references, err := header.MsgIDList("References")
	if err != nil {
		return CuratedHeaders{}, &Error{Kind: ErrorProtocol}
	}
	replyRaw, err := header.AddressList("Reply-To")
	if err != nil || len(replyRaw) > maxAddresses {
		return CuratedHeaders{}, &Error{Kind: ErrorProtocol}
	}
	reply := make([]Address, 0, len(replyRaw))
	for _, addr := range replyRaw {
		if err := boundedString(addr.Name); err != nil {
			return CuratedHeaders{}, err
		}
		if err := boundedString(addr.Address); err != nil {
			return CuratedHeaders{}, err
		}
		reply = append(reply, Address{Name: addr.Name, Address: addr.Address})
	}
	for _, value := range append(append([]string{messageID}, inReplyTo...), references...) {
		if err := boundedString(value); err != nil {
			return CuratedHeaders{}, err
		}
	}
	return CuratedHeaders{MessageID: messageID, InReplyTo: inReplyTo, References: references, ReplyTo: reply}, nil
}

func (s *Session) FetchBodyPart(uid uint32, path []int) (BodyData, error) {
	mimeSection := &imap.FetchItemBodySection{Part: path, Specifier: imap.PartSpecifierMIME, Partial: &imap.SectionPartial{Size: maxHeaderBytes + 1}, Peek: true}
	bodySection := &imap.FetchItemBodySection{Part: path, Partial: &imap.SectionPartial{Size: maxWireBodyBytes + 1}, Peek: true}
	buffers, err := s.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{mimeSection, bodySection},
	}).Collect()
	if err != nil {
		return BodyData{}, classify(err, false)
	}
	if len(buffers) != 1 || buffers[0].UID != imap.UID(uid) {
		return BodyData{}, &Error{Kind: ErrorNotFound}
	}
	mimeBytes := buffers[0].FindBodySection(mimeSection)
	bodyBytes := buffers[0].FindBodySection(bodySection)
	if len(mimeBytes) > maxHeaderBytes || len(bodyBytes) > maxWireBodyBytes {
		return BodyData{TooLarge: true}, nil
	}
	if mimeBytes == nil || bodyBytes == nil {
		return BodyData{}, &Error{Kind: ErrorProtocol}
	}
	return BodyData{MIMEHeader: mimeBytes, Body: bodyBytes}, nil
}

func (s *Session) StoreDelta(uid uint32, add bool, flags []string) error {
	op := imap.StoreFlagsDel
	if add {
		op = imap.StoreFlagsAdd
	}
	imapFlags := make([]imap.Flag, len(flags))
	for i, flag := range flags {
		imapFlags[i] = imap.Flag(flag)
	}
	err := s.client.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: op, Silent: true, Flags: imapFlags}, nil).Close()
	return classify(err, true)
}

func (s *Session) FetchFlags(uid uint32) (Message, error) {
	buffers, err := s.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, Flags: true, ModSeq: s.caps.CondStore}).Collect()
	if err != nil {
		return Message{}, classify(err, false)
	}
	if len(buffers) != 1 || buffers[0].UID != imap.UID(uid) {
		return Message{}, &Error{Kind: ErrorNotFound}
	}
	msg := Message{UID: uid, ModSeq: buffers[0].ModSeq}
	for _, flag := range buffers[0].Flags {
		msg.Flags = append(msg.Flags, string(flag))
	}
	return msg, nil
}

func (s *Session) NativeMove(uid uint32, destination string) (CopyData, error) {
	if !s.caps.Move {
		return CopyData{}, &Error{Kind: ErrorProtocol}
	}
	data, err := s.client.Move(imap.UIDSetNum(imap.UID(uid)), destination).Wait()
	if err != nil {
		return CopyData{}, classify(err, true)
	}
	if data == nil {
		return CopyData{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	return validatedCopyData(imap.UID(uid), data.UIDValidity, data.SourceUIDs, data.DestUIDs, false)
}

func (s *Session) Copy(uid uint32, destination string) (CopyData, error) {
	data, err := s.client.Copy(imap.UIDSetNum(imap.UID(uid)), destination).Wait()
	if err != nil {
		return CopyData{}, classify(err, true)
	}
	if data == nil {
		return CopyData{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	return validatedCopyData(imap.UID(uid), data.UIDValidity, data.SourceUIDs, data.DestUIDs, true)
}

func validatedCopyData(requested imap.UID, uidValidity uint32, sourceSet, destinationSet imap.NumSet, required bool) (CopyData, error) {
	if sourceSet == nil && destinationSet == nil && uidValidity == 0 && !required {
		return CopyData{}, nil
	}
	if uidValidity == 0 {
		return CopyData{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	source, err := scalarUID(sourceSet)
	if err != nil || source != requested {
		return CopyData{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	destination, err := scalarUID(destinationSet)
	if err != nil {
		return CopyData{}, &Error{Kind: ErrorProtocol, Ambiguous: true}
	}
	return CopyData{UIDValidity: uidValidity, DestinationUID: uint32(destination)}, nil
}

func scalarUID(numSet imap.NumSet) (imap.UID, error) {
	set, ok := numSet.(imap.UIDSet)
	if !ok || len(set) != 1 {
		return 0, &Error{Kind: ErrorProtocol}
	}
	uidRange := set[0]
	if uidRange.Start == 0 || uidRange.Stop == 0 || uidRange.Start != uidRange.Stop {
		return 0, &Error{Kind: ErrorProtocol}
	}
	return uidRange.Start, nil
}

func (s *Session) AddDeleted(uid uint32) error {
	err := s.client.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
	return classify(err, true)
}

func (s *Session) UIDExpunge(uid uint32) error {
	if !s.caps.UIDPlus {
		return &Error{Kind: ErrorProtocol}
	}
	err := s.client.UIDExpunge(imap.UIDSetNum(imap.UID(uid))).Close()
	return classify(err, true)
}

func (s *Session) Close() error {
	s.once.Do(func() {
		// The context watcher remains active while LOGOUT is in flight. If the
		// context is already canceled, close directly instead of starting a
		// command which can no longer complete within the caller's deadline.
		if s.authenticated && s.bounded && s.ctx.Err() == nil && s.client.State() != imap.ConnStateLogout {
			_ = s.client.Logout().Wait()
		}
		_ = s.client.Close()
		close(s.done)
	})
	return nil
}

// deadlineConn prevents protocol code from clearing or extending the caller's
// absolute deadline. A zero requested deadline means "clear", so it must also
// remain clamped when the context has a deadline.
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

func boundedString(value string) error {
	if len(value) > maxMetadataString {
		return &Error{Kind: ErrorTooLarge}
	}
	return nil
}

func classify(err error, ambiguous bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errInboundLimit) {
		return &Error{Kind: ErrorTooLarge, Ambiguous: ambiguous}
	}
	var imapErr *imap.Error
	if errors.As(err, &imapErr) {
		kind := ErrorProtocol
		switch imapErr.Code {
		case imap.ResponseCodeAuthenticationFailed:
			kind = ErrorAuthentication
		case imap.ResponseCodeAuthorizationFailed, imap.ResponseCodeNoPerm, imap.ResponseCodePrivacyRequired:
			kind = ErrorAuthorization
		case imap.ResponseCodeNonExistent:
			kind = ErrorNotFound
		case imap.ResponseCodeAlreadyExists, imap.ResponseCodeInUse:
			kind = ErrorConflict
		case imap.ResponseCodeLimit:
			kind = ErrorRateLimited
		case imap.ResponseCodeUnavailable:
			kind = ErrorUnavailable
		case imap.ResponseCodeTooBig, imap.ResponseCodeOverQuota:
			kind = ErrorTooLarge
		}
		return &Error{Kind: kind}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Kind: ErrorTimeout, Ambiguous: ambiguous}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return &Error{Kind: ErrorUnavailable, Ambiguous: ambiguous}
	}
	return &Error{Kind: ErrorProtocol, Ambiguous: ambiguous}
}
