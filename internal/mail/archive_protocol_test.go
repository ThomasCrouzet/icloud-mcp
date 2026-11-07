package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

type archiveProtocol struct {
	client                              *Client
	imap, smtp, lists, appends, data    atomic.Int32
	mu                                  sync.Mutex
	submitted, appended                 []byte
	target                              string
	listing, appendOutcome, smtpOutcome string
	dataStarted, release                chan struct{}
}

func newArchiveProtocol(t *testing.T, listing, scope, outcome string, enabled bool) *archiveProtocol {
	t.Helper()
	f := &archiveProtocol{listing: listing, appendOutcome: outcome}
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	policy, err := ParseRecipientPolicy("to@example.invalid,cc@example.invalid,bcc@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Address: "fixture@icloud.com", Password: "synthetic-archive-secret", RecipientPolicy: policy, EnableSentCopy: enabled}
	if scope != "" {
		cfg.ArchiveScope = []string{scope}
	}
	f.client, err = newService(cfg, func(context.Context) (net.Conn, error) {
		f.imap.Add(1)
		local, remote := net.Pipe()
		go f.serveIMAP(remote)
		return local, nil
	}, func(context.Context) (net.Conn, error) {
		f.smtp.Add(1)
		local, remote := net.Pipe()
		go f.serveSMTP(remote, serverTLS)
		return local, nil
	}, false, true, func(ctx context.Context, conn net.Conn, address, password string) (imapSession, error) {
		return imapadapter.NewSession(ctx, conn, address, password)
	}, func(ctx context.Context, conn net.Conn) (smtpSession, error) {
		return newSMTPSessionWithTLS(ctx, conn, clientTLS)
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// NewArchiveMCPFixture exposes only synthetic protocol fixtures to external
// package tests, without adding production transport injection points.
func NewArchiveMCPFixture(t *testing.T, scope, outcome string, enabled bool) (*Client, func() [5]int32) {
	f := newArchiveProtocol(t, archiveSentListing, scope, outcome, enabled)
	return f.client, f.counts
}

const archiveSentListing = "* LIST (\\Sent) \"/\" \"Private Sent\"\r\n"

func (f *archiveProtocol) serveIMAP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	write := func(s string) bool { _, err := io.WriteString(conn, s); return err == nil }
	if !write("* OK [CAPABILITY IMAP4rev1 UIDPLUS SPECIAL-USE] fixture\r\n") {
		return
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return
		}
		tag := fields[0]
		switch fields[1] {
		case "LOGIN":
			if !write(tag + " OK [CAPABILITY IMAP4rev1 UIDPLUS SPECIAL-USE] authenticated\r\n") {
				return
			}
		case "LIST":
			n := f.lists.Add(1)
			listing := f.listing
			if n > 1 {
				listing = "* LIST (\\Sent) \"/\" \"Replacement Sent\"\r\n"
			}
			if !write(listing + tag + " OK listed\r\n") {
				return
			}
		case "APPEND":
			f.appends.Add(1)
			start := strings.LastIndex(line, "{")
			end := strings.LastIndex(line, "}")
			if start < 0 || end <= start {
				return
			}
			n, err := strconv.Atoi(strings.TrimSuffix(line[start+1:end], "+"))
			if err != nil {
				return
			}
			if !write("+ ready\r\n") {
				return
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				return
			}
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			f.mu.Lock()
			f.appended = payload
			f.target = strings.TrimSpace(line[len(tag)+len(" APPEND ") : start])
			f.mu.Unlock()
			if f.appendOutcome == "unknown" {
				return
			}
			response := tag + " OK [APPENDUID 9 42] stored\r\n"
			if f.appendOutcome == "failed" {
				response = tag + " NO private-server-detail\r\n"
			}
			if !write(response) {
				return
			}
		case "LOGOUT":
			write("* BYE closing\r\n" + tag + " OK logout\r\n")
			return
		default:
			if !write(tag + " BAD unexpected\r\n") {
				return
			}
		}
	}
}

func (f *archiveProtocol) serveSMTP(conn net.Conn, config *tls.Config) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	wire := conn
	r := bufio.NewReader(wire)
	write := func(s string) bool { _, err := io.WriteString(wire, s); return err == nil }
	if !write("220 smtp.fixture ESMTP\r\n") {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return
		}
		switch fields[0] {
		case "EHLO":
			if !write("250-fixture\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n") {
				return
			}
		case "STARTTLS":
			if !write("220 begin TLS\r\n") {
				return
			}
			wire = tls.Server(conn, config)
			r = bufio.NewReader(wire)
		case "AUTH":
			if !write("235 authenticated\r\n") {
				return
			}
		case "MAIL", "RCPT", "RSET":
			if !write("250 accepted\r\n") {
				return
			}
		case "DATA":
			if !write("354 send data\r\n") {
				return
			}
			payload, err := io.ReadAll(textproto.NewReader(r).DotReader())
			if err != nil {
				return
			}
			f.mu.Lock()
			f.submitted = payload
			f.mu.Unlock()
			f.data.Add(1)
			if f.dataStarted != nil {
				close(f.dataStarted)
				<-f.release
			}
			if f.smtpOutcome == "unknown" {
				return
			}
			if f.smtpOutcome == "rejected" {
				if !write("554 rejected\r\n") {
					return
				}
			} else if !write("250 queued\r\n") {
				return
			}
		default:
			return
		}
	}
}

func archiveInput() SendInput {
	return SendInput{To: []string{"to@example.invalid"}, Body: "synthetic archive body", ArchiveSent: true, IdempotencyKey: "archive-key"}
}

func (f *archiveProtocol) counts() [5]int32 {
	return [5]int32{f.imap.Load(), f.lists.Load(), f.smtp.Load(), f.data.Load(), f.appends.Load()}
}

func TestProtocolArchivePreflight(t *testing.T) {
	for _, tc := range []struct {
		name, listing, scope string
		enabled              bool
		want                 [5]int32
	}{
		{"denied", archiveSentListing, "Other", true, [5]int32{1, 1, 0, 0, 0}},
		{"missing", "* LIST () \"/\" INBOX\r\n", "", true, [5]int32{1, 1, 0, 0, 0}},
		{"ambiguous", archiveSentListing + "* LIST (\\Sent) \"/\" Other\r\n", "Private Sent", true, [5]int32{1, 1, 0, 0, 0}},
		{"nonselectable", "* LIST (\\Sent \\Noselect) \"/\" Sent\r\n", "", true, [5]int32{1, 1, 0, 0, 0}},
		{"invalid-empty", "* LIST (\\Sent) \"/\" \"\"\r\n", "", true, [5]int32{1, 1, 0, 0, 0}},
		{"capability-disabled", archiveSentListing, "", false, [5]int32{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveProtocol(t, tc.listing, tc.scope, "stored", tc.enabled)
			res, err := f.client.SendMessage(t.Context(), archiveInput())
			t.Logf("result=%+v error=%v imap/list/smtp/data/append=%v", res, err, f.counts())
			if err == nil || res.Status == SendAccepted || f.counts() != tc.want {
				t.Fatalf("unsafe archival preflight: %+v %v counts=%v want=%v", res, err, f.counts(), tc.want)
			}
			if strings.Contains(fmt.Sprint(err), "Private Sent") || strings.Contains(res.SentDestination, "Private Sent") {
				t.Fatal("denied destination disclosed")
			}
		})
	}
}

func TestProtocolArchiveOutcomesAndReplay(t *testing.T) {
	for _, outcome := range []string{ArchivalStored, ArchivalFailed, ArchivalUnknown} {
		t.Run(outcome, func(t *testing.T) {
			f := newArchiveProtocol(t, archiveSentListing, "Private Sent", outcome, true)
			input := archiveInput()
			first, err := f.client.SendMessage(t.Context(), input)
			if err != nil || first.Status != SendAccepted || first.ArchivalStatus != outcome || first.SentCopyUnavailable != (outcome != ArchivalStored) {
				t.Fatalf("separate outcome: %+v %v", first, err)
			}
			second, err := f.client.SendMessage(t.Context(), input)
			if err != nil || second.MessageID != first.MessageID || second.ArchivalStatus != outcome {
				t.Fatalf("replay changed result: %+v %v", second, err)
			}
			input.ArchiveSent = false
			_, err = f.client.SendMessage(t.Context(), input)
			if err == nil || AsError(err).Code != CodeConflict {
				t.Errorf("changed preference did not conflict: %v", err)
			}
			f.mu.Lock()
			// SMTP DATA terminates the last line; DotReader normalizes CRLF to LF.
			expected := bytes.ReplaceAll(f.appended, []byte("\r\n"), []byte("\n"))
			if !bytes.HasSuffix(expected, []byte("\n")) {
				expected = append(expected, '\n')
			}
			equal := bytes.Equal(expected, f.submitted)
			target := f.target
			f.mu.Unlock()
			if !equal || target != `"Private Sent"` {
				t.Errorf("APPEND must use exact submitted bytes and pinned target: equal=%v target=%q", equal, target)
			}
			t.Logf("accepted=%s archival=%s cached=%s imap/list/smtp/data/append=%v", first.Status, first.ArchivalStatus, second.MessageID, f.counts())
			if f.counts() != [5]int32{2, 1, 1, 1, 1} {
				t.Errorf("unexpected retries/rediscovery: %v", f.counts())
			}
			if outcome != ArchivalStored && first.ArchivalError == "" {
				t.Error("missing archival failure classification")
			}
		})
	}
}

func TestProtocolArchiveConcurrentKey(t *testing.T) {
	f := newArchiveProtocol(t, archiveSentListing, "Private Sent", ArchivalStored, true)
	f.dataStarted, f.release = make(chan struct{}), make(chan struct{})
	input := archiveInput()
	results := make(chan SendResult, 2)
	errs := make(chan error, 2)
	go func() { res, err := f.client.SendMessage(t.Context(), input); results <- res; errs <- err }()
	select {
	case <-f.dataStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("positive SMTP DATA control did not activate")
	}
	go func() { res, err := f.client.SendMessage(t.Context(), input); results <- res; errs <- err }()
	changed := input
	changed.ArchiveSent = false
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err := f.client.SendMessage(ctx, changed)
	cancel()
	close(f.release)
	if public := AsError(err); public == nil || public.Code != CodeConflict {
		t.Errorf("active preference conflict = %v", err)
	}
	first, second := <-results, <-results
	if err1, err2 := <-errs, <-errs; err1 != nil || err2 != nil || first.Status != SendAccepted || second.MessageID != first.MessageID {
		t.Fatalf("concurrent replay: %+v %+v %v %v", first, second, err1, err2)
	}
	if f.counts() != [5]int32{2, 1, 1, 1, 1} {
		t.Fatalf("concurrent duplicate submitted again: %v", f.counts())
	}
}

func TestProtocolArchiveFalseAndFingerprint(t *testing.T) {
	f := newArchiveProtocol(t, archiveSentListing, "Other", ArchivalStored, true)
	input := archiveInput()
	input.ArchiveSent = false
	res, err := f.client.SendMessage(t.Context(), input)
	if err != nil || res.Status != SendAccepted || res.ArchivalStatus != ArchivalNotAttempted {
		t.Fatalf("false unexpectedly archives: %+v %v", res, err)
	}
	changes := []func(*SendInput){
		func(in *SendInput) { in.ArchiveSent = true }, func(in *SendInput) { in.To = []string{"cc@example.invalid"} },
		func(in *SendInput) { in.Cc = []string{"cc@example.invalid"} }, func(in *SendInput) { in.Bcc = []string{"bcc@example.invalid"} },
		func(in *SendInput) { in.Subject = "changed" }, func(in *SendInput) { in.Body = "changed" },
		func(in *SendInput) { in.InReplyTo = "<parent@example.invalid>" }, func(in *SendInput) { in.References = []string{"<root@example.invalid>"} },
	}
	for i, change := range changes {
		changed := input
		change(&changed)
		_, err := f.client.SendMessage(t.Context(), changed)
		if public := AsError(err); public == nil || public.Code != CodeConflict {
			t.Errorf("effectful field %d not fingerprinted: %v", i, err)
		}
	}
	if f.counts() != [5]int32{0, 0, 1, 1, 0} {
		t.Fatalf("false/conflict reached IMAP or sent again: %v", f.counts())
	}
}

func TestProtocolArchiveNoAppendWithoutAcceptance(t *testing.T) {
	for _, outcome := range []string{"rejected", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f := newArchiveProtocol(t, archiveSentListing, "Private Sent", ArchivalStored, true)
			f.smtpOutcome = outcome
			input := archiveInput()
			res, err := f.client.SendMessage(t.Context(), input)
			if err == nil || res.Status == SendAccepted || f.appends.Load() != 0 {
				t.Fatalf("archive without acceptance: %+v %v counts=%v", res, err, f.counts())
			}
			_, _ = f.client.SendMessage(t.Context(), input)
			if f.smtp.Load() != 1 || f.data.Load() != 1 {
				t.Fatal("SMTP failure was replayed")
			}
		})
	}
}

func TestProtocolArchivePreflightDeadline(t *testing.T) {
	f := newArchiveProtocol(t, archiveSentListing, "Private Sent", ArchivalStored, true)
	f.client.imapDial = func(context.Context) (net.Conn, error) {
		f.imap.Add(1)
		local, remote := net.Pipe()
		t.Cleanup(func() { _ = remote.Close() })
		return local, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := f.client.SendMessage(ctx, archiveInput())
	if public := AsError(err); public == nil || public.Code != CodeTimeout || f.smtp.Load() != 0 {
		t.Fatalf("preflight did not respect request deadline: %v counts=%v", err, f.counts())
	}
}

type panicArchiveSession struct{ imapSession }

func (panicArchiveSession) AppendSent(string, []byte) (imapadapter.AppendData, error) {
	panic("synthetic private parser panic")
}

func TestArchivePanicPreservesAcceptedAndUnknown(t *testing.T) {
	var log []string
	imap := &fakeIMAP{mailboxes: []imapadapter.Mailbox{{Name: "Sent", Attributes: []string{"\\Sent"}}}}
	svc := testService(t, imap, &fakeSMTP{log: &log}, false, true)
	svc.config.EnableSentCopy = true
	svc.imapFactory = func(context.Context, net.Conn, string, string) (imapSession, error) {
		return panicArchiveSession{imap}, nil
	}
	input := archiveInput()
	first, err := svc.SendMessage(t.Context(), input)
	second, secondErr := svc.SendMessage(t.Context(), input)
	if err != nil || secondErr != nil || first.Status != SendAccepted || first.ArchivalStatus != ArchivalUnknown || second.ArchivalStatus != ArchivalUnknown || first.ArchivalError == "" || strings.Count(strings.Join(log, ","), "data") != 1 {
		t.Fatalf("parser panic erased archival uncertainty or replayed SMTP: %+v %+v %v %v", first, second, err, secondErr)
	}
}
