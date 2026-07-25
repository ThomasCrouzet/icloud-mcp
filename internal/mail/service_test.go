package mail

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"github.com/emersion/go-smtp"
)

type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return "test" }
func (a fakeAddr) String() string  { return string(a) }

type fakeIMAP struct {
	caps              imapadapter.Capabilities
	modifiedDetection bool
	mailboxes         []imapadapter.Mailbox
	selected          imapadapter.SelectedMailbox
	searchFn          func(imapadapter.SearchRequest) ([]uint32, error)
	listFn            func() ([]imapadapter.Mailbox, error)
	metadata          []imapadapter.Message
	body              imapadapter.BodyData
	flags             imapadapter.Message
	copyData          imapadapter.CopyData
	nativeMoveFn      func() (imapadapter.CopyData, error)
	closeFn           func()
	errors            map[string]error
	log               []string
	searches          []imapadapter.SearchRequest
	mu                sync.Mutex
}

func (f *fakeIMAP) record(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, value)
}

func (f *fakeIMAP) operationError(name string) error {
	if f.errors == nil {
		return nil
	}
	return f.errors[name]
}

func (f *fakeIMAP) Capabilities() imapadapter.Capabilities { return f.caps }
func (f *fakeIMAP) SupportsModifiedDetection() bool        { return f.modifiedDetection }
func (f *fakeIMAP) List() ([]imapadapter.Mailbox, error) {
	f.record("list")
	if f.listFn != nil {
		return f.listFn()
	}
	return f.mailboxes, f.operationError("list")
}
func (f *fakeIMAP) Select(_ string, readOnly bool) (imapadapter.SelectedMailbox, error) {
	if readOnly {
		f.record("examine")
	} else {
		f.record("select")
	}
	return f.selected, f.operationError("select")
}
func (f *fakeIMAP) Search(request imapadapter.SearchRequest) ([]uint32, error) {
	f.record("search")
	f.mu.Lock()
	f.searches = append(f.searches, request)
	f.mu.Unlock()
	if f.searchFn != nil {
		return f.searchFn(request)
	}
	return nil, f.operationError("search")
}
func (f *fakeIMAP) FetchMetadata(_ []uint32, _ bool) ([]imapadapter.Message, error) {
	f.record("metadata")
	return f.metadata, f.operationError("metadata")
}
func (f *fakeIMAP) FetchBodyPart(_ uint32, _ []int) (imapadapter.BodyData, error) {
	f.record("body-peek")
	return f.body, f.operationError("body")
}
func (f *fakeIMAP) StoreDelta(_ uint32, _ bool, _ []string) error {
	f.record("store-delta")
	return f.operationError("store")
}
func (f *fakeIMAP) FetchFlags(_ uint32) (imapadapter.Message, error) {
	f.record("fetch-flags")
	return f.flags, f.operationError("flags")
}
func (f *fakeIMAP) NativeMove(_ uint32, _ string) (imapadapter.CopyData, error) {
	f.record("native-move")
	if f.nativeMoveFn != nil {
		return f.nativeMoveFn()
	}
	return f.copyData, f.operationError("move")
}
func (f *fakeIMAP) Copy(_ uint32, _ string) (imapadapter.CopyData, error) {
	f.record("copy")
	return f.copyData, f.operationError("copy")
}
func (f *fakeIMAP) AddDeleted(_ uint32) error {
	f.record("add-deleted")
	return f.operationError("deleted")
}
func (f *fakeIMAP) UIDExpunge(_ uint32) error {
	f.record("uid-expunge")
	return f.operationError("uidexpunge")
}
func (f *fakeIMAP) Close() error {
	f.record("close")
	if f.closeFn != nil {
		f.closeFn()
	}
	return nil
}

type fakeSMTP struct {
	log         *[]string
	rcptErrors  map[string]error
	authErr     error
	authFn      func() error
	mailErr     error
	dataErr     error
	dataStarted bool
	dataFn      func() error
	closeFn     func()
	message     []byte
	from        string
}

func (f *fakeSMTP) add(value string) { *f.log = append(*f.log, value) }
func (f *fakeSMTP) Auth(_, _ string) error {
	f.add("auth")
	if f.authFn != nil {
		return f.authFn()
	}
	return f.authErr
}
func (f *fakeSMTP) Mail(from string, _ int64, _ bool) error {
	f.from = from
	f.add("mail")
	return f.mailErr
}
func (f *fakeSMTP) Rcpt(address string) error {
	f.add("rcpt:" + address)
	return f.rcptErrors[address]
}
func (f *fakeSMTP) Reset() error { f.add("rset"); return nil }
func (f *fakeSMTP) Data(message []byte, phase *smtpDataPhase) error {
	f.add("data")
	f.message = append([]byte(nil), message...)
	phase.started = f.dataStarted
	if f.dataFn != nil {
		return f.dataFn()
	}
	return f.dataErr
}
func (f *fakeSMTP) Close() error {
	f.add("close")
	if f.closeFn != nil {
		f.closeFn()
	}
	return nil
}

func testService(t *testing.T, imap *fakeIMAP, smtpFake *fakeSMTP, mutation, send bool) *Client {
	t.Helper()
	policy, err := ParseRecipientPolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	if imap == nil {
		imap = &fakeIMAP{}
	}
	imapFactory := func(context.Context, net.Conn, string, string) (imapSession, error) { return imap, nil }
	var smtpFactory smtpSessionFactory
	if smtpFake != nil {
		smtpFactory = func(context.Context, net.Conn) (smtpSession, error) {
			smtpFake.add("starttls")
			return smtpFake, nil
		}
	}
	service, err := newService(Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
		func(context.Context) (net.Conn, error) { return fakeConn{}, nil },
		mutation, send, imapFactory, smtpFactory)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	return service
}

func errorCode(t *testing.T, err error) Code {
	t.Helper()
	var public *Error
	if !errors.As(err, &public) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	return public.Code
}

func TestParseRecipientPolicy(t *testing.T) {
	t.Parallel()
	valid := []string{"*", "a@example.com", "A@example.com, b@example.net"}
	for _, value := range valid {
		if _, err := ParseRecipientPolicy(value); err != nil {
			t.Errorf("ParseRecipientPolicy(%q): %v", value, err)
		}
	}
	invalid := []string{"", "*,a@example.com", "a@example.com,A@example.com", "Name <a@example.com>", "@example.com", "a@example.com,", "a@example.com\nb@example.com"}
	for _, value := range invalid {
		if _, err := ParseRecipientPolicy(value); err == nil {
			t.Errorf("ParseRecipientPolicy(%q) unexpectedly succeeded", value)
		}
	}
	policy, _ := ParseRecipientPolicy("Allowed@Example.com")
	if !policy.allows("allowed@example.COM") || policy.allows("other@example.com") {
		t.Fatal("exact ASCII-folded recipient matching failed")
	}
}

func TestConstructorAndInputValidationDoNotDial(t *testing.T) {
	t.Parallel()
	policy, _ := ParseRecipientPolicy("allowed@example.com")
	inertDial := func(context.Context) (net.Conn, error) { return nil, errors.New("inert test dialer") }
	if _, err := NewService(Config{Address: "Display <x@example.com>", Password: "app-password", RecipientPolicy: policy}, inertDial, nil, false, false); errorCode(t, err) != CodeValidation {
		t.Fatal("invalid address was not rejected")
	}
	var dials atomic.Int32
	service, err := newService(Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) { dials.Add(1); return fakeConn{}, nil },
		func(context.Context) (net.Conn, error) { dials.Add(1); return fakeConn{}, nil }, true, true,
		func(context.Context, net.Conn, string, string) (imapSession, error) { return &fakeIMAP{}, nil },
		func(context.Context, net.Conn) (smtpSession, error) { return &fakeSMTP{log: &[]string{}}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 1}); errorCode(t, err) != CodeValidation {
		t.Fatal("zero UID was not rejected")
	}
	if _, err := service.SearchMessages(context.Background(), SearchInput{Mailbox: "INBOX", BeforeUID: 5}); errorCode(t, err) != CodeValidation {
		t.Fatal("cursor without UIDVALIDITY was not rejected")
	}
	if _, err := service.SetMessageFlags(context.Background(), SetFlagsInput{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Operation: FlagOperationAdd, Flags: []MessageFlag{"deleted"}}); errorCode(t, err) != CodeValidation {
		t.Fatal("unsupported flag was not rejected")
	}
	if _, err := service.SendMessage(context.Background(), SendInput{To: []string{"blocked@example.com"}, Subject: "x", Body: "x"}); errorCode(t, err) != CodeAuthorization {
		t.Fatal("blocked recipient was not rejected")
	}
	if dials.Load() != 0 {
		t.Fatalf("validation dialed %d times", dials.Load())
	}
}

func TestSearchUIDWindowsAndIdentity(t *testing.T) {
	t.Parallel()
	imap := &fakeIMAP{selected: imapadapter.SelectedMailbox{UIDValidity: 7, UIDNext: 60_001}}
	service := testService(t, imap, nil, false, false)
	result, err := service.SearchMessages(context.Background(), SearchInput{Mailbox: "INBOX", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ScanLimitReached || result.UIDValidity != 7 || len(imap.searches) != 10 {
		t.Fatalf("unexpected bounded scan: %+v, searches=%d", result, len(imap.searches))
	}
	for _, request := range imap.searches {
		if request.UIDMax-request.UIDMin+1 > 5000 {
			t.Fatalf("unbounded search window: %+v", request)
		}
	}

	imap2 := &fakeIMAP{selected: imapadapter.SelectedMailbox{UIDValidity: 8, UIDNext: 10}}
	service2 := testService(t, imap2, nil, false, false)
	_, err = service2.SearchMessages(context.Background(), SearchInput{Mailbox: "INBOX", BeforeUID: 5, UIDValidity: 7})
	if errorCode(t, err) != CodeConcurrentModification || len(imap2.searches) != 0 {
		t.Fatal("UIDVALIDITY mismatch did not stop before SEARCH")
	}
}

func TestSearchUsesTypedCriteriaAndReturnsNewestUIDFirst(t *testing.T) {
	t.Parallel()
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 12, UIDNext: 30},
		searchFn: func(request imapadapter.SearchRequest) ([]uint32, error) {
			return []uint32{20, 25}, nil
		},
		metadata: []imapadapter.Message{
			{UID: 20, Envelope: imapadapter.Envelope{Subject: "older"}},
			{UID: 25, Envelope: imapadapter.Envelope{Subject: "newer"}},
		},
	}
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	before := since.AddDate(0, 0, 10)
	result, err := testService(t, imap, nil, false, false).SearchMessages(context.Background(), SearchInput{
		Mailbox: "INBOX", Query: "needle", From: "from", To: "to", Subject: "subject",
		Since: since, Before: before, UnseenOnly: true, FlaggedOnly: true, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[0].UID != 25 || result.Messages[1].UID != 20 || result.NextBeforeUID != 20 {
		t.Fatalf("unexpected search result: %+v", result)
	}
	request := imap.searches[0]
	if request.Query != "needle" || request.From != "from" || request.To != "to" || request.Subject != "subject" || !request.UnseenOnly || !request.FlaggedOnly || !request.Since.Equal(since) || !request.Before.Equal(before) {
		t.Fatalf("typed criteria were lost: %+v", request)
	}
	if strings.Contains(strings.Join(imap.log, ","), "body-peek") {
		t.Fatal("search fetched a body section")
	}
}

func TestListMailboxesCapsObjects(t *testing.T) {
	t.Parallel()
	// Service-level soft truncate when the adapter returns exactly MaxMailboxes+
	// (fakeIMAP bypasses adapter hard fail). Real adapter fails closed on overflow.
	imap := &fakeIMAP{}
	for i := 0; i < MaxMailboxes+1; i++ {
		imap.mailboxes = append(imap.mailboxes, imapadapter.Mailbox{Name: "mailbox-" + itoa(i)})
	}
	result, err := testService(t, imap, nil, false, false).ListMailboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mailboxes) != MaxMailboxes || !result.Truncated {
		t.Fatalf("mailbox cap failed: %d, truncated=%v", len(result.Mailboxes), result.Truncated)
	}
}

func TestSafeReadRetriesAtMostOnceBeforeAnyResult(t *testing.T) {
	t.Parallel()
	policy, _ := ParseRecipientPolicy("*")
	var dials atomic.Int32
	imap := &fakeIMAP{mailboxes: []imapadapter.Mailbox{{Name: "INBOX"}}}
	service, err := newService(Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				return nil, io.ErrUnexpectedEOF
			}
			return fakeConn{}, nil
		}, nil, false, false,
		func(context.Context, net.Conn, string, string) (imapSession, error) { return imap, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ListMailboxes(context.Background())
	if err != nil || len(result.Mailboxes) != 1 || dials.Load() != 2 {
		t.Fatalf("safe reconnect failed: %+v, %v, dials=%d", result, err, dials.Load())
	}
}

func TestGetMessageUsesExaminePeekAndOmitsHTML(t *testing.T) {
	t.Parallel()
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 9, UIDNext: 3},
		metadata: []imapadapter.Message{{
			UID: 2, Envelope: imapadapter.Envelope{Subject: "subject"},
			Parts: []imapadapter.BodyPart{{Path: []int{1}, ContentType: "text/plain", InlinePlain: true}, {Path: []int{2}, ContentType: "application/pdf", Filename: "a.pdf", Attachment: true}},
		}},
		body: imapadapter.BodyData{MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"), Body: []byte("hello=20world")},
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 9, UID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if message.Body != "hello world" || len(message.Attachments) != 1 {
		t.Fatalf("unexpected message: %+v", message)
	}
	joined := strings.Join(imap.log, ",")
	if !strings.Contains(joined, "examine") || !strings.Contains(joined, "body-peek") || strings.Contains(joined, "store") {
		t.Fatalf("unsafe read operations: %s", joined)
	}

	html := &fakeIMAP{selected: imapadapter.SelectedMailbox{UIDValidity: 9, UIDNext: 3}, metadata: []imapadapter.Message{{UID: 2, Envelope: imapadapter.Envelope{}, Parts: []imapadapter.BodyPart{{Path: []int{1}, ContentType: "text/html"}}}}}
	message, err = testService(t, html, nil, false, false).GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 9, UID: 2})
	if err != nil || message.BodyUnavailableReason != "html_only" || strings.Contains(strings.Join(html.log, ","), "body-peek") {
		t.Fatalf("HTML-only handling failed: %+v, %v", message, err)
	}
}

func TestGetMessageUIDValidityAndBodyLimits(t *testing.T) {
	t.Parallel()
	imap := &fakeIMAP{selected: imapadapter.SelectedMailbox{UIDValidity: 10, UIDNext: 2}}
	service := testService(t, imap, nil, false, false)
	_, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 9, UID: 1})
	if errorCode(t, err) != CodeConcurrentModification || strings.Contains(strings.Join(imap.log, ","), "metadata") {
		t.Fatal("UIDVALIDITY mismatch did not stop before FETCH")
	}

	body := strings.Repeat("a", 11)
	decoded, oversized, err := decodePlainBody([]byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), []byte(body), 10)
	if err != nil || !oversized || decoded != "" {
		t.Fatalf("decoded body cap failed: %q %v %v", decoded, oversized, err)
	}
}

func TestSetFlagsCapabilityGateAndDeltaFallback(t *testing.T) {
	t.Parallel()
	input := SetFlagsInput{Mailbox: "INBOX", UIDValidity: 5, UID: 2, Operation: FlagOperationAdd, Flags: []MessageFlag{FlagSeen}, ExpectedModSeq: 11}
	condstore := &fakeIMAP{caps: imapadapter.Capabilities{CondStore: true}, selected: imapadapter.SelectedMailbox{UIDValidity: 5, UIDNext: 3}, flags: imapadapter.Message{UID: 2}}
	_, err := testService(t, condstore, nil, true, false).SetMessageFlags(context.Background(), input)
	if errorCode(t, err) != CodeProtocolError || strings.Contains(strings.Join(condstore.log, ","), "store-delta") {
		t.Fatalf("unsafe CONDSTORE mutation: %v, %v", err, condstore.log)
	}

	fallback := &fakeIMAP{selected: imapadapter.SelectedMailbox{UIDValidity: 5, UIDNext: 3}, flags: imapadapter.Message{UID: 2, Flags: []string{"\\Seen"}}}
	result, err := testService(t, fallback, nil, true, false).SetMessageFlags(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConditionalUpdate || len(result.Flags) != 1 || result.Flags[0] != FlagSeen {
		t.Fatalf("unexpected delta result: %+v", result)
	}
	if strings.Count(strings.Join(fallback.log, ","), "store-delta") != 1 {
		t.Fatalf("delta STORE count: %v", fallback.log)
	}
}

func TestMoveCapabilityPathsAndPartialSafety(t *testing.T) {
	t.Parallel()
	base := func(caps imapadapter.Capabilities) *fakeIMAP {
		return &fakeIMAP{
			caps: caps, selected: imapadapter.SelectedMailbox{UIDValidity: 4, UIDNext: 3},
			mailboxes: []imapadapter.Mailbox{{Name: "INBOX"}, {Name: "Archive"}},
			flags:     imapadapter.Message{UID: 2}, copyData: imapadapter.CopyData{UIDValidity: 12, DestinationUID: 8},
		}
	}
	input := MoveInput{Mailbox: "INBOX", UIDValidity: 4, UID: 2, Destination: "Archive"}
	native := base(imapadapter.Capabilities{Move: true})
	result, err := testService(t, native, nil, true, false).MoveMessage(context.Background(), input)
	if err != nil || result.Method != "move" || strings.Contains(strings.Join(native.log, ","), "copy") {
		t.Fatalf("native MOVE failed: %+v %v %v", result, err, native.log)
	}

	fallback := base(imapadapter.Capabilities{UIDPlus: true})
	result, err = testService(t, fallback, nil, true, false).MoveMessage(context.Background(), input)
	if err != nil || result.Method != "uidplus_fallback" {
		t.Fatalf("fallback MOVE failed: %+v %v", result, err)
	}
	joined := strings.Join(fallback.log, ",")
	if !strings.Contains(joined, "copy,add-deleted,uid-expunge") {
		t.Fatalf("fallback command order: %s", joined)
	}

	unsafe := base(imapadapter.Capabilities{})
	_, err = testService(t, unsafe, nil, true, false).MoveMessage(context.Background(), input)
	if errorCode(t, err) != CodeProtocolError || strings.Contains(strings.Join(unsafe.log, ","), "copy") {
		t.Fatal("unsafe fallback was not rejected")
	}

	partial := base(imapadapter.Capabilities{UIDPlus: true})
	partial.errors = map[string]error{"deleted": &imapadapter.Error{Kind: imapadapter.ErrorAuthorization}}
	_, err = testService(t, partial, nil, true, false).MoveMessage(context.Background(), input)
	if errorCode(t, err) != CodePartialFailure || strings.Contains(strings.Join(partial.log, ","), "uid-expunge") {
		t.Fatalf("partial move handling failed: %v %v", err, partial.log)
	}

	unknown := base(imapadapter.Capabilities{UIDPlus: true})
	unknown.errors = map[string]error{"uidexpunge": &imapadapter.Error{Kind: imapadapter.ErrorUnavailable, Ambiguous: true}}
	_, err = testService(t, unknown, nil, true, false).MoveMessage(context.Background(), input)
	if errorCode(t, err) != CodeOutcomeUnknown {
		t.Fatalf("ambiguous UID EXPUNGE: %v", err)
	}

	missingCopyUID := base(imapadapter.Capabilities{UIDPlus: true})
	missingCopyUID.copyData = imapadapter.CopyData{}
	_, err = testService(t, missingCopyUID, nil, true, false).MoveMessage(context.Background(), input)
	if public := AsError(err); public == nil || public.Code != CodeOutcomeUnknown || public.Reconciliation == "" || strings.Contains(strings.Join(missingCopyUID.log, ","), "add-deleted") {
		t.Fatalf("unusable COPYUID lost copy ambiguity: %v %v", err, missingCopyUID.log)
	}

	malformedMoveUID := base(imapadapter.Capabilities{Move: true})
	malformedMoveUID.copyData.DestinationUID = 0
	_, err = testService(t, malformedMoveUID, nil, true, false).MoveMessage(context.Background(), input)
	if public := AsError(err); public == nil || public.Code != CodeOutcomeUnknown || public.Reconciliation == "" {
		t.Fatalf("unusable MOVE COPYUID lost move ambiguity: %v", err)
	}

	cleanupPanic := base(imapadapter.Capabilities{Move: true})
	cleanupPanic.closeFn = func() { panic("cleanup payload") }
	result, err = testService(t, cleanupPanic, nil, true, false).MoveMessage(context.Background(), input)
	if err != nil || result.Method != "move" {
		t.Fatalf("cleanup panic contradicted successful MOVE: %+v, %v", result, err)
	}
}

func TestTrashRequiresExactlyOneSpecialUseMailbox(t *testing.T) {
	t.Parallel()
	input := TrashInput{Mailbox: "INBOX", UIDValidity: 4, UID: 2}
	for name, mailboxes := range map[string][]imapadapter.Mailbox{
		"none":     {{Name: "INBOX"}},
		"multiple": {{Name: "INBOX"}, {Name: "Trash A", Attributes: []string{"\\Trash"}}, {Name: "Trash B", Attributes: []string{"\\TRASH"}}},
	} {
		t.Run(name, func(t *testing.T) {
			imap := &fakeIMAP{mailboxes: mailboxes}
			_, err := testService(t, imap, nil, true, false).TrashMessage(context.Background(), input)
			if errorCode(t, err) != CodeProtocolError || strings.Contains(strings.Join(imap.log, ","), "select") {
				t.Fatalf("Trash ambiguity was unsafe: %v %v", err, imap.log)
			}
		})
	}
}

func TestReadRateAndConcurrencyLimits(t *testing.T) {
	service := testService(t, &fakeIMAP{}, nil, false, false)
	for i := 0; i < 10; i++ {
		if _, err := service.ListMailboxes(context.Background()); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if _, err := service.ListMailboxes(context.Background()); errorCode(t, err) != CodeRateLimited {
		t.Fatal("11th burst read was not rate limited")
	}

	var active, maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	factory := func(context.Context, net.Conn, string, string) (imapSession, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		return &blockingIMAP{release: release, started: started, closeFn: func() { active.Add(-1) }}, nil
	}
	policy, _ := ParseRecipientPolicy("*")
	concurrent, err := newService(Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		func(context.Context) (net.Conn, error) { return fakeConn{}, nil }, nil, false, false, factory, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = concurrent.ListMailboxes(context.Background()) }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("two reads did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("third read bypassed semaphore")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if maximum.Load() != 2 {
		t.Fatalf("maximum read sessions = %d", maximum.Load())
	}
}

func TestIMAPDeadlineCleanupReleasesReadSemaphore(t *testing.T) {
	policy, err := ParseRecipientPolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	stalledLogout := make(chan struct{})
	serverErrors := make(chan error, 2)
	dial := func(context.Context) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		stall := dials.Add(1) == 1
		go func() {
			serverErrors <- serveCleanupIMAP(serverConn, stall, stalledLogout)
		}()
		return clientConn, nil
	}
	service, err := newService(
		Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		dial, nil, false, false,
		func(ctx context.Context, conn net.Conn, address, password string) (imapSession, error) {
			return imapadapter.NewSession(ctx, conn, address, password)
		}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	service.readSem = make(chan struct{}, 1)

	firstCtx, firstCancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer firstCancel()
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.ListMailboxes(firstCtx)
		firstDone <- err
	}()
	select {
	case <-stalledLogout:
	case <-time.After(time.Second):
		t.Fatal("first session did not reach LOGOUT")
	}

	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.ListMailboxes(secondCtx)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second read bypassed held semaphore: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("stalled cleanup outlived the first context")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second read did not acquire released semaphore: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read semaphore was not released after context deadline")
	}
	for range 2 {
		if err := <-serverErrors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestParserPanicIsSanitizedAndReadSlotIsReusable(t *testing.T) {
	var calls atomic.Int32
	imap := &fakeIMAP{mailboxes: []imapadapter.Mailbox{{Name: "INBOX"}}}
	imap.listFn = func() ([]imapadapter.Mailbox, error) {
		if calls.Add(1) == 1 {
			panic("third-party parser payload")
		}
		return imap.mailboxes, nil
	}
	service := testService(t, imap, nil, false, false)
	service.readSem = make(chan struct{}, 1)

	if _, err := service.ListMailboxes(context.Background()); errorCode(t, err) != CodeProtocolError {
		t.Fatalf("parser panic was not sanitized: %v", err)
	}
	result, err := service.ListMailboxes(context.Background())
	if err != nil || len(result.Mailboxes) != 1 || len(service.readSem) != 0 {
		t.Fatalf("second read after parser panic = %+v, %v, semaphore=%d", result, err, len(service.readSem))
	}
}

func TestParserPanicsReleaseMutationAndSendSlots(t *testing.T) {
	t.Run("mutation", func(t *testing.T) {
		var calls atomic.Int32
		imap := &fakeIMAP{
			caps:      imapadapter.Capabilities{Move: true},
			selected:  imapadapter.SelectedMailbox{UIDValidity: 1, UIDNext: 2},
			mailboxes: []imapadapter.Mailbox{{Name: "INBOX"}, {Name: "Archive"}},
			flags:     imapadapter.Message{UID: 1},
		}
		imap.listFn = func() ([]imapadapter.Mailbox, error) {
			if calls.Add(1) == 1 {
				panic("third-party parser payload")
			}
			return imap.mailboxes, nil
		}
		service := testService(t, imap, nil, true, false)
		input := MoveInput{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Destination: "Archive"}
		if _, err := service.MoveMessage(context.Background(), input); errorCode(t, err) != CodeProtocolError {
			t.Fatalf("mutation parser panic was not sanitized: %v", err)
		}
		if _, err := service.MoveMessage(context.Background(), input); err != nil || len(service.mutationSem) != 0 {
			t.Fatalf("second mutation after parser panic = %v, semaphore=%d", err, len(service.mutationSem))
		}
	})

	t.Run("dispatched mutation", func(t *testing.T) {
		imap := &fakeIMAP{
			caps:      imapadapter.Capabilities{Move: true},
			selected:  imapadapter.SelectedMailbox{UIDValidity: 1, UIDNext: 2},
			mailboxes: []imapadapter.Mailbox{{Name: "INBOX"}, {Name: "Archive"}},
			flags:     imapadapter.Message{UID: 1},
			nativeMoveFn: func() (imapadapter.CopyData, error) {
				panic("third-party parser payload")
			},
		}
		service := testService(t, imap, nil, true, false)
		input := MoveInput{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Destination: "Archive"}
		_, err := service.MoveMessage(context.Background(), input)
		if public := AsError(err); public == nil || public.Code != CodeOutcomeUnknown || public.Reconciliation == "" || len(service.mutationSem) != 0 {
			t.Fatalf("dispatched mutation panic = %v, semaphore=%d", err, len(service.mutationSem))
		}
	})

	t.Run("send", func(t *testing.T) {
		var calls atomic.Int32
		var log []string
		smtpFake := &fakeSMTP{log: &log}
		smtpFake.authFn = func() error {
			if calls.Add(1) == 1 {
				panic("third-party parser payload")
			}
			return nil
		}
		service := testService(t, nil, smtpFake, false, true)
		input := SendInput{To: []string{"to@example.com"}, Body: "body"}
		if _, err := service.SendMessage(context.Background(), input); errorCode(t, err) != CodeProtocolError {
			t.Fatalf("SMTP parser panic was not sanitized: %v", err)
		}
		result, err := service.SendMessage(context.Background(), input)
		if err != nil || result.Status != SendAccepted || len(service.sendSem) != 0 {
			t.Fatalf("second send after parser panic = %+v, %v, semaphore=%d", result, err, len(service.sendSem))
		}
	})
}

func serveCleanupIMAP(conn net.Conn, stallLogout bool, stalledLogout chan<- struct{}) error {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
		return err
	}
	login, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(conn, commandTagForMailTest(login)+" OK [CAPABILITY IMAP4rev1] authenticated\r\n"); err != nil {
		return err
	}
	list, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(conn, "* LIST () \"/\" \"INBOX\"\r\n%s OK listed\r\n", commandTagForMailTest(list)); err != nil {
		return err
	}
	logout, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(logout, "LOGOUT") {
		return fmt.Errorf("unexpected cleanup command: %q", logout)
	}
	if stallLogout {
		close(stalledLogout)
		if line, err := reader.ReadString('\n'); err == nil {
			return fmt.Errorf("stalled connection remained open: %q", line)
		}
		return nil
	}
	_, err = fmt.Fprintf(conn, "* BYE closing\r\n%s OK logout\r\n", commandTagForMailTest(logout))
	return err
}

func commandTagForMailTest(line string) string {
	if space := strings.IndexByte(line, ' '); space > 0 {
		return line[:space]
	}
	return "T0"
}

func TestConfiguredSemaphoreAndWriteBurstLimits(t *testing.T) {
	t.Parallel()
	var smtpLog []string
	imap := &fakeIMAP{
		caps:      imapadapter.Capabilities{Move: true},
		selected:  imapadapter.SelectedMailbox{UIDValidity: 1, UIDNext: 2},
		mailboxes: []imapadapter.Mailbox{{Name: "INBOX"}, {Name: "Archive"}},
		flags:     imapadapter.Message{UID: 1},
	}
	service := testService(t, imap, &fakeSMTP{log: &smtpLog}, true, true)
	if cap(service.readSem) != 2 || cap(service.mutationSem) != 1 || cap(service.sendSem) != 1 {
		t.Fatalf("unexpected semaphore capacities: %d/%d/%d", cap(service.readSem), cap(service.mutationSem), cap(service.sendSem))
	}
	move := MoveInput{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Destination: "Archive"}
	for i := 0; i < 3; i++ {
		if _, err := service.MoveMessage(context.Background(), move); err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	if _, err := service.MoveMessage(context.Background(), move); errorCode(t, err) != CodeRateLimited {
		t.Fatal("fourth mutation burst was not rate limited")
	}
	for i := 0; i < 3; i++ {
		if _, err := service.SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Body: "x"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := service.SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Body: "x"}); errorCode(t, err) != CodeRateLimited {
		t.Fatal("fourth send burst was not rate limited")
	}
}

type blockingIMAP struct {
	release chan struct{}
	started chan struct{}
	closeFn func()
}

func (b *blockingIMAP) Capabilities() imapadapter.Capabilities { return imapadapter.Capabilities{} }
func (b *blockingIMAP) SupportsModifiedDetection() bool        { return false }
func (b *blockingIMAP) List() ([]imapadapter.Mailbox, error) {
	b.started <- struct{}{}
	<-b.release
	return nil, nil
}
func (b *blockingIMAP) Select(string, bool) (imapadapter.SelectedMailbox, error) {
	return imapadapter.SelectedMailbox{}, nil
}
func (b *blockingIMAP) Search(imapadapter.SearchRequest) ([]uint32, error)          { return nil, nil }
func (b *blockingIMAP) FetchMetadata([]uint32, bool) ([]imapadapter.Message, error) { return nil, nil }
func (b *blockingIMAP) FetchBodyPart(uint32, []int) (imapadapter.BodyData, error) {
	return imapadapter.BodyData{}, nil
}
func (b *blockingIMAP) StoreDelta(uint32, bool, []string) error { return nil }
func (b *blockingIMAP) FetchFlags(uint32) (imapadapter.Message, error) {
	return imapadapter.Message{}, nil
}
func (b *blockingIMAP) NativeMove(uint32, string) (imapadapter.CopyData, error) {
	return imapadapter.CopyData{}, nil
}
func (b *blockingIMAP) Copy(uint32, string) (imapadapter.CopyData, error) {
	return imapadapter.CopyData{}, nil
}
func (b *blockingIMAP) AddDeleted(uint32) error { return nil }
func (b *blockingIMAP) UIDExpunge(uint32) error { return nil }
func (b *blockingIMAP) Close() error            { b.closeFn(); return nil }

func TestSMTPOrderMIMEAndAllRecipientPolicy(t *testing.T) {
	t.Parallel()
	var log []string
	fake := &fakeSMTP{log: &log}
	service := testService(t, nil, fake, false, true)
	result, err := service.SendMessage(context.Background(), SendInput{
		To: []string{"to@example.com"}, Cc: []string{"cc@example.com"}, Bcc: []string{"bcc@example.com"},
		Subject: "Hello", Body: "plain body",
	})
	if err != nil || result.Status != SendAccepted || !result.SentCopyUnavailable {
		t.Fatalf("send failed: %+v %v", result, err)
	}
	want := "starttls,auth,mail,rcpt:to@example.com,rcpt:cc@example.com,rcpt:bcc@example.com,data,close"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("SMTP order:\n got %s\nwant %s", got, want)
	}
	encoded := string(fake.message)
	if strings.Contains(strings.ToLower(encoded), "bcc:") || !strings.Contains(encoded, "Message-Id: <") || !strings.Contains(encoded, "@icloud.com>") {
		t.Fatalf("unsafe MIME headers:\n%s", encoded)
	}
	if fake.from != "sender@icloud.com" || result.MessageID == "" {
		t.Fatalf("From or Message-ID contract failed: %q %+v", fake.from, result)
	}
}

func TestSMTPRejectsBeforeDataAndContinuesRCPT(t *testing.T) {
	t.Parallel()
	var log []string
	fake := &fakeSMTP{log: &log, rcptErrors: map[string]error{"bad@example.com": &smtp.SMTPError{Code: 550}}}
	service := testService(t, nil, fake, false, true)
	result, err := service.SendMessage(context.Background(), SendInput{To: []string{"ok@example.com", "bad@example.com", "last@example.com"}, Subject: "x", Body: "x"})
	if errorCode(t, err) != CodeAuthorization || result.Status != SendRejected || len(result.Recipients) != 3 {
		t.Fatalf("unexpected RCPT result: %+v %v", result, err)
	}
	joined := strings.Join(log, ",")
	if !strings.Contains(joined, "rcpt:last@example.com,rset") || strings.Contains(joined, "data") {
		t.Fatalf("partial SMTP submission risk: %s", joined)
	}
}

func TestSMTPDefinitiveRejectionsRetainResult(t *testing.T) {
	tests := []struct {
		name           string
		configure      func(*fakeSMTP)
		wantCode       Code
		wantRecipients int
	}{
		{
			name: "authentication",
			configure: func(fake *fakeSMTP) {
				fake.authErr = &smtp.SMTPError{Code: 535}
			},
			wantCode: CodeAuthentication,
		},
		{
			name: "mail",
			configure: func(fake *fakeSMTP) {
				fake.mailErr = &smtp.SMTPError{Code: 550}
			},
			wantCode: CodeAuthorization,
		},
		{
			name: "recipient",
			configure: func(fake *fakeSMTP) {
				fake.rcptErrors = map[string]error{"to@example.com": &smtp.SMTPError{Code: 550}}
			},
			wantCode:       CodeAuthorization,
			wantRecipients: 1,
		},
		{
			name: "data",
			configure: func(fake *fakeSMTP) {
				fake.dataStarted = true
				fake.dataErr = &smtp.SMTPError{Code: 554}
			},
			wantCode:       CodeAuthorization,
			wantRecipients: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var log []string
			fake := &fakeSMTP{log: &log}
			test.configure(fake)
			result, err := testService(t, nil, fake, false, true).SendMessage(context.Background(), SendInput{
				To: []string{"to@example.com"}, Body: "plain body",
			})
			if errorCode(t, err) != test.wantCode || result.Status != SendRejected || result.MessageID == "" || len(result.Recipients) != test.wantRecipients {
				t.Fatalf("SendMessage() = %+v, %v", result, err)
			}
			for index, recipient := range result.Recipients {
				if recipient.Index != index {
					t.Errorf("recipient status %d = %+v", index, recipient)
				}
			}
		})
	}
}

func TestSMTPLocalValidationReturnsRejectedResult(t *testing.T) {
	var log []string
	result, err := testService(t, nil, &fakeSMTP{log: &log}, false, true).SendMessage(context.Background(), SendInput{Body: "plain body"})
	if errorCode(t, err) != CodeValidation || result.Status != SendRejected || result.MessageID != "" || len(log) != 0 {
		t.Fatalf("SendMessage() = %+v, %v, log=%v", result, err, log)
	}
}

func TestSMTPAmbiguousDataOutcome(t *testing.T) {
	for name, dataErr := range map[string]error{
		"connection loss": io.ErrUnexpectedEOF,
		"inbound budget":  errSMTPInboundLimit,
		"overlong line":   smtp.ErrTooLongLine,
		"non-rejection":   &smtp.SMTPError{Code: 399},
	} {
		t.Run(name, func(t *testing.T) {
			var log []string
			fake := &fakeSMTP{log: &log, dataStarted: true, dataErr: dataErr}
			result, err := testService(t, nil, fake, false, true).SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Subject: "x", Body: "x"})
			public := AsError(err)
			if public == nil || public.Code != CodeOutcomeUnknown || public.Reconciliation == "" || result.Status != SendOutcomeUnknown || result.Reconciliation != public.Reconciliation {
				t.Fatalf("ambiguous DATA result: %+v %v", result, err)
			}
		})
	}
}

func TestSMTPPhaseAwarePanicRecovery(t *testing.T) {
	t.Run("panic after DATA dispatch", func(t *testing.T) {
		var log []string
		fake := &fakeSMTP{log: &log, dataStarted: true, dataFn: func() error {
			panic("third-party parser payload")
		}}
		service := testService(t, nil, fake, false, true)
		result, err := service.SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Body: "body"})
		if public := AsError(err); public == nil || public.Code != CodeOutcomeUnknown || public.Reconciliation == "" || result.Status != SendOutcomeUnknown || len(service.sendSem) != 0 {
			t.Fatalf("DATA panic = %+v, %v, semaphore=%d", result, err, len(service.sendSem))
		}
	})

	t.Run("cleanup panic after acceptance", func(t *testing.T) {
		var log []string
		fake := &fakeSMTP{log: &log, dataStarted: true, closeFn: func() {
			panic("third-party cleanup payload")
		}}
		result, err := testService(t, nil, fake, false, true).SendMessage(context.Background(), SendInput{To: []string{"to@example.com"}, Body: "body"})
		if err != nil || result.Status != SendAccepted || !result.SentCopyUnavailable {
			t.Fatalf("accepted result contradicted by cleanup panic: %+v, %v", result, err)
		}
	})
}

func TestSendLimits(t *testing.T) {
	t.Parallel()
	var log []string
	service := testService(t, nil, &fakeSMTP{log: &log}, false, true)
	for name, input := range map[string]SendInput{
		"subject":   {To: []string{"to@example.com"}, Subject: strings.Repeat("s", MaxSubjectBytes+1)},
		"body":      {To: []string{"to@example.com"}, Body: strings.Repeat("b", MaxSendBodyBytes+1)},
		"header":    {To: []string{"to@example.com"}, Subject: "bad\rsubject"},
		"duplicate": {To: []string{"to@example.com"}, Cc: []string{"TO@example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.SendMessage(context.Background(), input); errorCode(t, err) != CodeValidation {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if len(log) != 0 {
		t.Fatalf("invalid messages reached SMTP: %v", log)
	}
}
