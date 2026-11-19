package imapadapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedIMAP is one offline IMAP server. It reads command lines from a pipe
// and writes the replies of its handler. The transcription of every command
// lets a test prove how many STORE commands reached the wire.
//
// The fixture exists because go-imap v2 beta.9 cannot emit the tagged MODIFIED
// response code, and the adapter must observe it. The fixture therefore writes
// raw response lines, not imapserver values.
type scriptedIMAP struct {
	t        *testing.T
	greeting string
	handle   func(line string) string
	errs     chan error

	mu       sync.Mutex
	commands []string
}

func newScriptedIMAP(t *testing.T, greeting string, handle func(line string) string) *scriptedIMAP {
	t.Helper()
	server := &scriptedIMAP{t: t, greeting: greeting, handle: handle, errs: make(chan error, 1)}
	return server
}

func (s *scriptedIMAP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprint(conn, s.greeting); err != nil {
		s.errs <- err
		return
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			s.errs <- nil
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, strings.TrimRight(line, "\r\n"))
		s.mu.Unlock()
		reply := s.handle(line)
		if reply == "" {
			continue
		}
		if _, err := fmt.Fprint(conn, reply); err != nil {
			s.errs <- err
			return
		}
	}
}

func (s *scriptedIMAP) transcript() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *scriptedIMAP) stores() []string {
	var out []string
	for _, line := range s.transcript() {
		if strings.Contains(strings.ToUpper(line), " STORE ") {
			out = append(out, line)
		}
	}
	return out
}

// newCondStoreSession opens one synthetic CONDSTORE session. The handler
// answers SELECT and the pre-store FETCH; each test supplies the STORE reply.
func newCondStoreSession(t *testing.T, storeReply func(tag string) string) (*Session, *scriptedIMAP) {
	t.Helper()
	return newCondStoreSessionWithTimeout(t, 10*time.Second, storeReply)
}

// newCondStoreSessionWithTimeout sets the session deadline. A test that withholds
// the tagged reply uses a short deadline so the session cannot outlive it.
func newCondStoreSessionWithTimeout(t *testing.T, timeout time.Duration, storeReply func(tag string) string) (*Session, *scriptedIMAP) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := newScriptedIMAP(t, "* OK [CAPABILITY IMAP4rev1 CONDSTORE] ready\r\n", func(line string) string {
		tag := commandTag(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, " LOGIN "):
			return tag + " OK [CAPABILITY IMAP4rev1 CONDSTORE] authenticated\r\n"
		case strings.Contains(upper, " SELECT ") || strings.Contains(upper, " EXAMINE "):
			return "* 1 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 12] next\r\n* OK [HIGHESTMODSEQ 42] highest\r\n" +
				tag + " OK [READ-WRITE] selected\r\n"
		case strings.Contains(upper, " STORE "):
			return storeReply(tag)
		case strings.Contains(upper, " FETCH "):
			return "* 1 FETCH (UID 7 FLAGS (\\Seen) MODSEQ (42))\r\n" + tag + " OK fetched\r\n"
		case strings.Contains(upper, " LOGOUT "):
			return "* BYE closing\r\n" + tag + " OK logout\r\n"
		}
		return tag + " OK done\r\n"
	})
	go server.serve(serverConn)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	session, err := NewSession(ctx, clientConn, "sender@example.test", "app-password")
	if err != nil {
		cancel()
		t.Fatalf("synthetic CONDSTORE session: %v", err)
	}
	if _, err := session.Select("INBOX", false); err != nil {
		_ = session.Close()
		cancel()
		t.Fatalf("synthetic CONDSTORE select: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		select {
		case err := <-server.errs:
			_ = err
		case <-time.After(2 * time.Second):
		}
	})
	return session, server
}

const (
	condStoreUID        = uint32(7)
	condStoreExpected   = uint64(42)
	condStoreNewModSeq  = uint64(43)
	condStoreStoreReply = "* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged) MODSEQ (43))\r\n%s OK stored\r\n"
)

func TestConditionalStoreProvenSuccessOnWire(t *testing.T) {
	session, server := newCondStoreSession(t, func(tag string) string {
		return fmt.Sprintf(condStoreStoreReply, tag)
	})
	if !session.SupportsConditionalStore() {
		t.Fatal("a guarded CONDSTORE session reported no conditional STORE support")
	}
	updated, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	if err != nil {
		t.Fatalf("proven conditional STORE: %v", err)
	}
	if updated.UID != condStoreUID || updated.ModSeq != condStoreNewModSeq {
		t.Fatalf("conditional STORE result = %+v", updated)
	}
	if strings.Join(updated.Flags, ",") != "\\Seen,\\Flagged" {
		t.Fatalf("conditional STORE flags = %v", updated.Flags)
	}
	stores := server.stores()
	if len(stores) != 1 {
		t.Fatalf("guarded STORE count = %d, want 1: %v", len(stores), stores)
	}
	if !strings.Contains(strings.ToUpper(stores[0]), "UNCHANGEDSINCE 42") {
		t.Fatalf("guarded STORE lost the precondition: %q", stores[0])
	}
}

// TestConditionalStoreRFC7162Example10 covers RFC 7162 3.1.3 Example 10: the
// server sends an unsolicited FETCH whose flags already match the request and
// a tagged OK [MODIFIED uid]. The command failed for this UID, so no returned
// state can prove a conditional success.
func TestConditionalStoreRFC7162Example10(t *testing.T) {
	session, _ := newCondStoreSession(t, func(tag string) string {
		return fmt.Sprintf(condStoreStoreReply, tag+" OK [MODIFIED 7] Conditional STORE failed")
	})
	_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	if !isAdapterKind(err, ErrorConflict) {
		t.Fatalf("RFC 7162 Example 10 outcome = %v, want conflict", err)
	}
}

func TestConditionalStoreStaleModSeqIsConflict(t *testing.T) {
	session, server := newCondStoreSession(t, func(tag string) string {
		return tag + " OK [MODIFIED 7] Conditional STORE failed\r\n"
	})
	_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	if !isAdapterKind(err, ErrorConflict) {
		t.Fatalf("stale MODSEQ outcome = %v, want conflict", err)
	}
	if len(server.stores()) != 1 {
		t.Fatalf("stale MODSEQ sent %d STORE commands, want 1", len(server.stores()))
	}
}

// TestConditionalStoreModifiedSetBinding rejects a MODIFIED set that does not
// name this UID. A single-UID STORE can only report the UIDs that failed.
func TestConditionalStoreModifiedSetBinding(t *testing.T) {
	for name, code := range map[string]string{
		"other uid":  "OK [MODIFIED 8] failed",
		"empty set":  "OK [MODIFIED] failed",
		"garbage":    "OK [MODIFIED seven] failed",
		"reversed":   "OK [MODIFIED 9:8] failed",
		"zero uid":   "OK [MODIFIED 0] failed",
		"descending": "OK [MODIFIED 9,7] failed",
		"too many":   "OK [MODIFIED " + strings.TrimSuffix(strings.Repeat("9,", maxObservedModifiedRanges+1), ",") + "] failed",
	} {
		t.Run(name, func(t *testing.T) {
			session, _ := newCondStoreSession(t, func(tag string) string {
				return "* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged) MODSEQ (43))\r\n" + tag + " " + code + "\r\n"
			})
			_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
			protocolErr, ok := err.(*Error)
			if !ok || protocolErr.Kind != ErrorProtocol || !protocolErr.Ambiguous {
				t.Fatalf("unbound MODIFIED set outcome = %v, want ambiguous protocol error", err)
			}
		})
	}
}

// TestConditionalStoreNoFetchIsUnverifiable covers a tagged OK that carried no
// MODIFIED code and no FETCH: the precondition passed but the final flags stay
// unverified. The command did run, so the outcome is a dispatched but
// unverifiable failure, never a silent success and never a retry.
func TestConditionalStoreNoFetchIsUnverifiable(t *testing.T) {
	session, server := newCondStoreSession(t, func(tag string) string {
		return tag + " OK stored\r\n"
	})
	_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	var protocolErr *Error
	if err == nil || !errors.As(err, &protocolErr) || protocolErr.Kind != ErrorProtocol || !protocolErr.Ambiguous {
		t.Fatalf("missing FETCH outcome = %v, want ambiguous protocol error", err)
	}
	if len(server.stores()) != 1 {
		t.Fatalf("missing FETCH sent %d STORE commands, want 1", len(server.stores()))
	}
}

func TestConditionalStoreMalformedFetchIsAmbiguous(t *testing.T) {
	for name, fetch := range map[string]string{
		"missing uid":    "* 1 FETCH (FLAGS (\\Seen \\Flagged) MODSEQ (43))\r\n",
		"other uid":      "* 1 FETCH (UID 8 FLAGS (\\Seen \\Flagged) MODSEQ (43))\r\n",
		"missing modseq": "* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged))\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			session, _ := newCondStoreSession(t, func(tag string) string {
				return fetch + tag + " OK stored\r\n"
			})
			_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
			protocolErr, ok := err.(*Error)
			if !ok || protocolErr.Kind != ErrorProtocol || !protocolErr.Ambiguous {
				t.Fatalf("malformed FETCH outcome = %v, want ambiguous protocol error", err)
			}
		})
	}
}

// TestConditionalStoreCompetingClient proves that a FETCH which contradicts the
// requested delta is a conflict, even when the tagged status carried no
// MODIFIED code.
func TestConditionalStoreCompetingClient(t *testing.T) {
	session, _ := newCondStoreSession(t, func(tag string) string {
		return "* 1 FETCH (UID 7 FLAGS (\\Seen) MODSEQ (43))\r\n" + tag + " OK stored\r\n"
	})
	_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	if !isAdapterKind(err, ErrorConflict) {
		t.Fatalf("competing client outcome = %v, want conflict", err)
	}
}

// TestConditionalStoreNoOpIsSuccess covers a delta that is already present: the
// precondition passes and the FETCH confirms the requested final state.
func TestConditionalStoreNoOpIsSuccess(t *testing.T) {
	session, server := newCondStoreSession(t, func(tag string) string {
		return "* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged) MODSEQ (42))\r\n" + tag + " OK stored\r\n"
	})
	updated, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	if err != nil {
		t.Fatalf("no-op conditional STORE: %v", err)
	}
	if updated.ModSeq != condStoreExpected {
		t.Fatalf("no-op MODSEQ = %d, want %d", updated.ModSeq, condStoreExpected)
	}
	if len(server.stores()) != 1 {
		t.Fatalf("no-op sent %d STORE commands, want 1", len(server.stores()))
	}
}

// TestConditionalStoreUnobservableStatusIsAmbiguous covers a dispatched STORE
// whose tagged status never arrives. The outcome stays unknown and no second
// STORE is sent.
func TestConditionalStoreUnobservableStatusIsAmbiguous(t *testing.T) {
	session, server := newCondStoreSessionWithTimeout(t, time.Second, func(string) string {
		return "* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged) MODSEQ (43))\r\n"
	})
	_, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected)
	protocolErr, ok := err.(*Error)
	if !ok || !protocolErr.Ambiguous {
		t.Fatalf("unobservable status outcome = %v, want ambiguous protocol error", err)
	}
	if len(server.stores()) != 1 {
		t.Fatalf("unobservable status sent %d STORE commands, want 1", len(server.stores()))
	}
}

// TestConditionalStoreRequiresCondStoreHarness keeps the capability honest: a
// session without a guard reports no support and refuses before dispatch.
func TestConditionalStoreRequiresCondStoreHarness(t *testing.T) {
	bare := &Session{}
	if bare.SupportsConditionalStore() || bare.SupportsModifiedDetection() {
		t.Fatal("an unguarded session claimed conditional STORE support")
	}
	clientConn, serverConn := net.Pipe()
	server := newScriptedIMAP(t, "* OK [CAPABILITY IMAP4rev1] ready\r\n", func(line string) string {
		tag := commandTag(line)
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, " LOGIN "):
			return tag + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
		case strings.Contains(upper, " SELECT "):
			return "* 1 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 12] next\r\n" + tag + " OK [READ-WRITE] selected\r\n"
		}
		return tag + " OK done\r\n"
	})
	go server.serve(serverConn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := NewSession(ctx, clientConn, "sender@example.test", "app-password")
	if err != nil {
		t.Fatalf("synthetic plain session: %v", err)
	}
	defer func() { _ = session.Close() }()
	if session.SupportsConditionalStore() {
		t.Fatal("a plain IMAP session claimed conditional STORE support")
	}
	if _, err := session.ConditionalStoreDelta(condStoreUID, true, []string{"\\Flagged"}, condStoreExpected); !isAdapterKind(err, ErrorProtocol) {
		t.Fatalf("plain conditional STORE = %v, want protocol error", err)
	}
	if len(server.stores()) != 0 {
		t.Fatalf("plain conditional STORE dispatched %d STORE commands", len(server.stores()))
	}
}
