package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"github.com/emersion/go-smtp"
)

func FuzzParseRecipientPolicy(f *testing.F) {
	for _, seed := range []string{
		"*",
		"person@example.com",
		"One@example.com, two@example.net",
		"Name <person@example.com>",
		"person@example.com,PERSON@example.com",
		"person@example.com\r\nBcc: injected@example.com",
		"\x00@example.com",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 4096 {
			return
		}
		policy, err := ParseRecipientPolicy(raw)
		if err == nil && !policy.valid() {
			t.Fatal("successful parse returned an invalid recipient policy")
		}
	})
}

func FuzzDecodePlainBody(f *testing.F) {
	f.Add([]byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), []byte("plain text"), uint16(128))
	f.Add([]byte("Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n"), []byte("SGVsbG8="), uint16(5))
	f.Add([]byte("Content-Type: text/html\r\n\r\n"), []byte("<b>unsafe</b>"), uint16(128))
	f.Add([]byte("Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"), []byte("broken="), uint16(1))
	f.Add([]byte("bad header\r\n\r\n"), []byte{0, 0xff, '\r', '\n'}, uint16(32))
	f.Fuzz(func(t *testing.T, header, body []byte, requested uint16) {
		if len(header) > MaxHeaderBytes+1 || len(body) > MaxWireBodyBytes+1 {
			return
		}
		limit := int(requested)%4096 + 1
		decoded, oversized, err := decodePlainBody(header, body, limit)
		if err == nil && !oversized && len(decoded) > 3*limit {
			t.Fatalf("decoded body length %d exceeds bounded UTF-8 expansion for limit %d", len(decoded), limit)
		}
	})
}

func TestMailValidationAndMIMEBoundaryPaths(t *testing.T) {
	tests := []struct {
		name      string
		header    []byte
		body      []byte
		limit     int
		want      string
		oversized bool
		wantErr   bool
	}{
		{name: "base64", header: []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n"), body: []byte("SGVsbG8="), limit: 5, want: "Hello"},
		{name: "invalid UTF-8 replacement", header: []byte("Content-Type: text/plain\r\n\r\n"), body: []byte{0xff}, limit: 4, want: "\uFFFD"},
		{name: "HTML rejected", header: []byte("Content-Type: text/html\r\n\r\n"), body: []byte("body"), limit: 10, wantErr: true},
		{name: "decoded overflow", header: []byte("Content-Type: text/plain\r\n\r\n"), body: []byte("12345"), limit: 4, oversized: true},
		{name: "wire overflow", header: []byte("Content-Type: text/plain\r\n\r\n"), body: make([]byte, MaxWireBodyBytes+1), limit: 4, oversized: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, oversized, err := decodePlainBody(test.header, test.body, test.limit)
			if (err != nil) != test.wantErr || oversized != test.oversized || got != test.want {
				t.Fatalf("decodePlainBody() = %q, %v, %v", got, oversized, err)
			}
		})
	}

	if got := truncateUTF8("ab\xc3\xa9", 3); got != "ab" {
		t.Fatalf("truncateUTF8() = %q", got)
	}
	for _, input := range []SetFlagsInput{
		{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Operation: FlagOperationAdd, Flags: []MessageFlag{FlagSeen, FlagSeen}},
		{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Operation: "replace", Flags: []MessageFlag{FlagSeen}},
		{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Operation: FlagOperationRemove},
	} {
		if err := validateFlags(input); err == nil {
			t.Fatalf("invalid flags accepted: %+v", input)
		}
	}
	if err := validateFlags(SetFlagsInput{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Operation: FlagOperationAdd, Flags: []MessageFlag{FlagSeen, FlagFlagged}}); err != nil {
		t.Fatalf("valid flags rejected: %v", err)
	}
}

func TestIMAPErrorMappingCoversAdapterKinds(t *testing.T) {
	tests := []struct {
		kind imapadapter.ErrorKind
		code Code
	}{
		{imapadapter.ErrorProtocol, CodeProtocolError},
		{imapadapter.ErrorAuthentication, CodeAuthentication},
		{imapadapter.ErrorAuthorization, CodeAuthorization},
		{imapadapter.ErrorNotFound, CodeNotFound},
		{imapadapter.ErrorConflict, CodeConflict},
		{imapadapter.ErrorRateLimited, CodeRateLimited},
		{imapadapter.ErrorTimeout, CodeTimeout},
		{imapadapter.ErrorUnavailable, CodeUnavailable},
		{imapadapter.ErrorTooLarge, CodePayloadTooLarge},
	}
	for _, test := range tests {
		err := mapIMAPError(&imapadapter.Error{Kind: test.kind}, context.Background(), false, "")
		if got := AsError(err); got == nil || got.Code != test.code {
			t.Errorf("kind %d mapped to %v, want %s", test.kind, err, test.code)
		}
	}
	reconciliation := "re-read the message"
	err := mapIMAPError(&imapadapter.Error{Kind: imapadapter.ErrorUnavailable, Ambiguous: true}, context.Background(), true, reconciliation)
	if got := AsError(err); got == nil || got.Code != CodeOutcomeUnknown || got.Reconciliation != reconciliation {
		t.Fatalf("ambiguous mutation mapped to %#v", got)
	}
	if got := mapIMAPError(validationError("invalid"), context.Background(), false, ""); got == nil || AsError(got).Code != CodeValidation {
		t.Fatalf("public error changed: %v", got)
	}
}

func TestSMTPClientSubmissionCommands(t *testing.T) {
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	clientConn, serverConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		write := func(value string) error {
			_, err := io.WriteString(serverConn, value)
			return err
		}
		readCommand := func(want string) error {
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			if !strings.HasPrefix(strings.ToUpper(line), want) {
				return fmt.Errorf("SMTP command %q does not start with %q", line, want)
			}
			return nil
		}
		if err := write("220 smtp.test ESMTP ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := readCommand("EHLO"); err != nil {
			serverErr <- err
			return
		}
		if err := write("250-localhost\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := readCommand("STARTTLS"); err != nil {
			serverErr <- err
			return
		}
		if err := write("220 begin TLS\r\n"); err != nil {
			serverErr <- err
			return
		}
		tlsConn := tlsServer(serverConn, serverTLS)
		if err := tlsConn.Handshake(); err != nil {
			serverErr <- err
			return
		}
		reader = bufio.NewReader(tlsConn)
		write = func(value string) error {
			_, err := io.WriteString(tlsConn, value)
			return err
		}
		if err := readCommand("EHLO"); err != nil {
			serverErr <- err
			return
		}
		if err := write("250-localhost\r\n250-AUTH PLAIN\r\n250-SIZE 262144\r\n250 SMTPUTF8\r\n"); err != nil {
			serverErr <- err
			return
		}
		for _, step := range []struct {
			command string
			reply   string
		}{{"AUTH", "235 authenticated\r\n"}, {"MAIL", "250 sender ok\r\n"}, {"RCPT", "250 recipient ok\r\n"}, {"DATA", "354 continue\r\n"}} {
			if err := readCommand(step.command); err != nil {
				serverErr <- err
				return
			}
			if err := write(step.reply); err != nil {
				serverErr <- err
				return
			}
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverErr <- err
				return
			}
			if line == ".\r\n" {
				break
			}
		}
		if err := write("250 queued\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := readCommand("RSET"); err != nil {
			serverErr <- err
			return
		}
		serverErr <- write("250 reset\r\n")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Auth("sender@icloud.com", "test-password"); err != nil {
		t.Fatal(err)
	}
	if err := session.Mail("sender@icloud.com", 6, true); err != nil {
		t.Fatal(err)
	}
	if err := session.Rcpt("recipient@example.com"); err != nil {
		t.Fatal(err)
	}
	var phase smtpDataPhase
	if err := session.Data([]byte("body\r\n"), &phase); err != nil || !phase.started {
		t.Fatalf("Data() = started %v, %v", phase.started, err)
	}
	if err := session.Reset(); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestSMTPErrorMappingBoundaries(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		err  error
		code Code
	}{
		{errSMTPUTF8Unavailable, CodeProtocolError},
		{&smtp.SMTPError{Code: 450}, CodeUnavailable},
		{&smtp.SMTPError{Code: 535}, CodeAuthentication},
		{&smtp.SMTPError{Code: 552}, CodePayloadTooLarge},
		{&smtp.SMTPError{Code: 550}, CodeAuthorization},
		{&smtp.SMTPError{Code: 399}, CodeProtocolError},
		{io.ErrUnexpectedEOF, CodeUnavailable},
	}
	for _, test := range tests {
		if got := AsError(mapSMTPError(test.err, ctx, false, "submission failed")); got == nil || got.Code != test.code {
			t.Errorf("mapSMTPError(%T) = %#v, want %s", test.err, got, test.code)
		}
	}
	for _, startedErr := range []error{io.ErrUnexpectedEOF, errSMTPInboundLimit, smtp.ErrTooLongLine} {
		if got := AsError(mapSMTPError(startedErr, ctx, true, "submission failed")); got == nil || got.Code != CodeOutcomeUnknown {
			t.Fatalf("started DATA error %v = %#v", startedErr, got)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := AsError(mapSMTPError(errors.New("ignored"), canceled, false, "submission failed")); got == nil || got.Code != CodeTimeout {
		t.Fatalf("canceled SMTP error = %#v", got)
	}
}

func TestSafeRetryMetadata(t *testing.T) {
	for name, err := range map[string]error{
		"context deadline": contextError(),
		"IMAP unavailable": mapIMAPError(&imapadapter.Error{Kind: imapadapter.ErrorUnavailable}, context.Background(), false, ""),
		"IMAP rate limit":  mapIMAPError(&imapadapter.Error{Kind: imapadapter.ErrorRateLimited}, context.Background(), false, ""),
		"SMTP unavailable": mapSMTPError(io.ErrUnexpectedEOF, context.Background(), false, "submission failed"),
		"SMTP auth 4xx":    mapSMTPAuthError(&smtp.SMTPError{Code: 454}, context.Background()),
	} {
		if public := AsError(err); public == nil || !public.Retryable {
			t.Errorf("%s did not expose retry metadata: %#v", name, public)
		}
	}

	dispatched := AsError(mapIMAPError(&imapadapter.Error{Kind: imapadapter.ErrorUnavailable}, context.Background(), true, "reconcile"))
	if dispatched == nil || dispatched.Retryable {
		t.Fatalf("dispatched IMAP error was marked safely retryable: %#v", dispatched)
	}
	dispatchedSMTP := AsError(mapSMTPError(&smtp.SMTPError{Code: 450}, context.Background(), true, "submission failed"))
	if dispatchedSMTP == nil || dispatchedSMTP.Retryable {
		t.Fatalf("dispatched SMTP rejection was marked safely retryable: %#v", dispatchedSMTP)
	}
}

func TestMailResultAndSMTPHelperBoundaries(t *testing.T) {
	result, err := fitMessageResult(Message{Body: strings.Repeat("x", MaxResultBytes)})
	if err != nil || len(result.Body) >= MaxResultBytes || len(result.Warnings) != 1 || result.Warnings[0].Code != "body_truncated" {
		t.Fatalf("fitMessageResult() = body %d, warnings %#v, %v", len(result.Body), result.Warnings, err)
	}
	attachments := make([]Attachment, MaxAttachments)
	for i := range attachments {
		attachments[i] = Attachment{PartID: itoa(i + 1), Filename: strings.Repeat("x", MaxMetadataString), ContentType: "application/octet-stream"}
	}
	if _, err := fitMessageResult(Message{Attachments: attachments}); errorCode(t, err) != CodePayloadTooLarge {
		t.Fatalf("oversized metadata error = %v", err)
	}

	if timeout, err := remainingSMTPTimeout(time.Now().Add(toolTimeout * 2)); err != nil || timeout != toolTimeout {
		t.Fatalf("long SMTP timeout = %v, %v", timeout, err)
	}
	if _, err := remainingSMTPTimeout(time.Now().Add(-time.Second)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired SMTP timeout = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if session, err := newSMTPSession(canceled, fakeConn{}); session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled SMTP session = %T, %v", session, err)
	}
	if err := (&smtpClientSession{}).Auth("sender@example.com", "password"); !errors.Is(err, errSMTPTLSUnverified) {
		t.Fatalf("pre-TLS authentication error = %v", err)
	}
	if got := AsError(mapSMTPAuthError(errSMTPAuthUnavailable, context.Background())); got == nil || got.Code != CodeProtocolError {
		t.Fatalf("missing AUTH capability = %#v", got)
	}
	if got := AsError(mapSMTPAuthError(&smtp.SMTPError{Code: 399}, context.Background())); got == nil || got.Code != CodeProtocolError {
		t.Fatalf("unexpected AUTH response = %#v", got)
	}
}

// tlsServer is kept behind a tiny helper so the fuzz targets themselves never
// create sockets or goroutines.
func tlsServer(conn net.Conn, config *tls.Config) *tls.Conn {
	return tls.Server(conn, config)
}
