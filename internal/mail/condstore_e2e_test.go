package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// condStoreWireServer is one synthetic CONDSTORE IMAP server behind the real
// adapter. It answers SELECT, the pre-store FETCH, the guarded STORE, and the
// post-store FETCH. The STORE reply is the scripted tag of the test, so the
// service outcome is decided by real wire bytes, not by a fake session.
type condStoreWireServer struct {
	mu       sync.Mutex
	commands []string
	stores   int
	flags    string
	modseq   uint64
	noFetch  bool
	modified bool
}

func (s *condStoreWireServer) dial(t *testing.T) func(context.Context) (net.Conn, error) {
	t.Helper()
	return func(context.Context) (net.Conn, error) {
		local, remote := net.Pipe()
		go func() { _ = s.serve(remote) }()
		return local, nil
	}
}

func (s *condStoreWireServer) serve(conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	write := func(format string, args ...any) error {
		_, err := fmt.Fprintf(conn, format, args...)
		return err
	}
	caps := "IMAP4rev1 CONDSTORE"
	if err := write("* OK [CAPABILITY %s] fixture\r\n", caps); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}
		trimmed := strings.TrimRight(line, "\r\n")
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			return fmt.Errorf("empty IMAP command")
		}
		tag := fields[0]
		end := len(fields)
		if end > 3 {
			end = 3
		}
		name := strings.ToUpper(strings.Join(fields[1:end], " "))
		if !strings.HasPrefix(name, "LOGIN") {
			s.mu.Lock()
			s.commands = append(s.commands, trimmed)
			s.mu.Unlock()
		}
		switch {
		case strings.HasPrefix(name, "LOGIN"):
			err = write("%s OK [CAPABILITY %s] authenticated\r\n", tag, caps)
		case strings.HasPrefix(name, "CAPABILITY"):
			err = write("* CAPABILITY %s\r\n%s OK done\r\n", caps, tag)
		case strings.HasPrefix(name, "SELECT"):
			err = write("* 1 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 12] next\r\n* OK [HIGHESTMODSEQ %d] highest\r\n%s OK [READ-WRITE] selected\r\n", s.currentModSeq(), tag)
		case strings.Contains(name, "STORE"):
			err = s.replyStore(write, tag, trimmed)
		case strings.Contains(name, "FETCH"):
			err = s.replyFetch(write, tag)
		case strings.HasPrefix(name, "LOGOUT"):
			err = write("* BYE closing\r\n%s OK logout\r\n", tag)
		default:
			err = write("%s OK done\r\n", tag)
		}
		if err != nil {
			return err
		}
	}
}

func (s *condStoreWireServer) currentModSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modseq == 0 {
		s.modseq = 42
	}
	return s.modseq
}

func (s *condStoreWireServer) replyStore(write func(string, ...any) error, tag, line string) error {
	s.mu.Lock()
	s.stores++
	if add := strings.Contains(strings.ToUpper(line), "+FLAGS"); add {
		s.flags = "\\Seen \\Flagged"
	} else {
		s.flags = "\\Seen"
	}
	s.modseq++
	modseq := s.modseq
	flags := s.flags
	noFetch := s.noFetch
	modified := s.modified
	s.mu.Unlock()
	if modified {
		return write("%s OK [MODIFIED 7] Conditional STORE failed\r\n", tag)
	}
	if noFetch {
		return write("%s OK stored\r\n", tag)
	}
	return write("* 1 FETCH (UID 7 FLAGS (%s) MODSEQ (%d))\r\n%s OK stored\r\n", flags, modseq, tag)
}

func (s *condStoreWireServer) replyFetch(write func(string, ...any) error, tag string) error {
	s.mu.Lock()
	if s.modseq == 0 {
		s.modseq = 42
	}
	flags := s.flags
	modseq := s.modseq
	s.mu.Unlock()
	if flags == "" {
		flags = "\\Seen"
	}
	return write("* 1 FETCH (UID 7 FLAGS (%s) MODSEQ (%d))\r\n%s OK fetched\r\n", flags, modseq, tag)
}

func (s *condStoreWireServer) storeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stores
}

func (s *condStoreWireServer) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, "\n")
}

// testCondStoreService builds the real service over the synthetic server. It
// uses the default adapter factory, so no fake session is involved.
func testCondStoreService(t *testing.T, server *condStoreWireServer) *Client {
	t.Helper()
	policy, err := ParseRecipientPolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{Address: "sender@icloud.com", Password: "app-password", RecipientPolicy: policy},
		server.dial(t), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func condStoreSetFlagsInput() SetFlagsInput {
	return SetFlagsInput{
		Mailbox: "INBOX", UIDValidity: 9, UID: 7,
		Operation: FlagOperationAdd, Flags: []MessageFlag{FlagFlagged}, ExpectedModSeq: 42,
	}
}

// TestSetFlagsCondStoreProvenSuccessE2E proves the restored capability through
// the real adapter: one guarded STORE, the tagged OK without MODIFIED, and the
// returned FETCH with the requested delta report a conditional success.
func TestSetFlagsCondStoreProvenSuccessE2E(t *testing.T) {
	server := &condStoreWireServer{flags: "\\Seen"}
	service := testCondStoreService(t, server)
	result, err := service.SetMessageFlags(context.Background(), condStoreSetFlagsInput())
	if err != nil {
		t.Fatalf("proven conditional flag mutation: %v", err)
	}
	if !result.ConditionalUpdate || result.ModSeq != 43 || len(result.Flags) != 2 {
		t.Fatalf("conditional result = %+v", result)
	}
	if server.storeCount() != 1 {
		t.Fatalf("STORE count = %d, want 1: %s", server.storeCount(), server.transcript())
	}
	if !strings.Contains(strings.ToUpper(server.transcript()), "UNCHANGEDSINCE 42") {
		t.Fatalf("guarded STORE lost the precondition: %s", server.transcript())
	}
}

// TestSetFlagsCondStoreRFC7162Example10E2E replays RFC 7162 3.1.3 Example 10
// through the service: an unsolicited matching FETCH plus tagged MODIFIED must
// never report a conditional success.
func TestSetFlagsCondStoreRFC7162Example10E2E(t *testing.T) {
	server := &condStoreWireServer{flags: "\\Seen", modified: true}
	service := testCondStoreService(t, server)
	result, err := service.SetMessageFlags(context.Background(), condStoreSetFlagsInput())
	if err == nil {
		t.Fatalf("RFC 7162 Example 10 reported success: %+v", result)
	}
	if code := mustCode(t, err); code != CodeConcurrentModification {
		t.Fatalf("RFC 7162 Example 10 code = %s, want concurrent_modification", code)
	}
	if server.storeCount() != 1 {
		t.Fatalf("RFC 7162 Example 10 sent %d STORE commands, want 1", server.storeCount())
	}
}

// TestSetFlagsCondStoreUnverifiableIsOutcomeUnknownE2E covers a dispatched STORE
// whose tagged OK carried no MODIFIED code and no FETCH. The command ran, so the
// result must be an unknown outcome, never a success and never a replay.
func TestSetFlagsCondStoreUnverifiableIsOutcomeUnknownE2E(t *testing.T) {
	server := &condStoreWireServer{flags: "\\Seen", noFetch: true}
	service := testCondStoreService(t, server)
	result, err := service.SetMessageFlags(context.Background(), condStoreSetFlagsInput())
	if err == nil {
		t.Fatalf("unverifiable conditional STORE reported success: %+v", result)
	}
	if code := mustCode(t, err); code != CodeOutcomeUnknown {
		t.Fatalf("unverifiable conditional STORE code = %s, want outcome_unknown", code)
	}
	if server.storeCount() != 1 {
		t.Fatalf("unverifiable conditional STORE sent %d STORE commands, want 1", server.storeCount())
	}
}

// TestSetFlagsCondStoreNeverFallsBackToUnconditionalSTORE proves that a
// CONDSTORE server never receives a plain delta STORE, whatever the guarded
// outcome was.
func TestSetFlagsCondStoreNeverFallsBackToUnconditionalSTORE(t *testing.T) {
	for name, server := range map[string]*condStoreWireServer{
		"modified": {flags: "\\Seen", modified: true},
		"no fetch": {flags: "\\Seen", noFetch: true},
	} {
		t.Run(name, func(t *testing.T) {
			service := testCondStoreService(t, server)
			_, err := service.SetMessageFlags(context.Background(), condStoreSetFlagsInput())
			if err == nil {
				t.Fatal("guarded conditional STORE reported success")
			}
			for _, line := range strings.Split(server.transcript(), "\n") {
				if strings.Contains(strings.ToUpper(line), " STORE ") && !strings.Contains(strings.ToUpper(line), "UNCHANGEDSINCE") {
					t.Fatalf("unconditional STORE reached a CONDSTORE server: %q", line)
				}
			}
			if server.storeCount() > 1 {
				t.Fatalf("guarded STORE was replayed %d times", server.storeCount())
			}
		})
	}
}

func mustCode(t *testing.T, err error) Code {
	t.Helper()
	public := AsError(err)
	if public == nil {
		t.Fatalf("unclassified error: %T %v", err, err)
	}
	return public.Code
}
