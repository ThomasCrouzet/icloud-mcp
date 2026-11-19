package imapadapter

import (
	"net"
	"testing"
	"time"
)

// discardConn accepts every write and never reads. It lets a test drive the
// protocol scan without a network peer.
type discardConn struct{}

func (discardConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) LocalAddr() net.Addr              { return discardAddr{} }
func (discardConn) RemoteAddr() net.Addr             { return discardAddr{} }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

type discardAddr struct{}

func (discardAddr) Network() string { return "test" }
func (discardAddr) String() string  { return "test" }

// TestStoreObservationScopedToActiveCommand proves that the guard reports no
// line while no command is being observed. A late tagged status for the
// previous tag is never evidence for a later command either.
func TestStoreObservationScopedToActiveCommand(t *testing.T) {
	guard := newGuardedConn(discardConn{})
	observation := &storeObservation{}
	observation.begin()
	guard.watch(observation)
	if _, err := guard.Write([]byte("T4 UID STORE 7 (UNCHANGEDSINCE 42) +FLAGS.SILENT (\\Flagged)\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := observation.proof(); !got.failed {
		t.Fatalf("a STORE without a tagged status was proven: %+v", got)
	}
	guard.watch(nil)
	scanInbound(t, guard, "T4 OK [MODIFIED 7] late\r\n")
	if !observation.proof().failed {
		t.Fatal("a disarmed observation recorded a late tagged status")
	}
	// Reuse the same observation for another command. The earlier tag must not
	// carry over.
	observation.begin()
	guard.watch(observation)
	if _, err := guard.Write([]byte("T5 UID STORE 8 (UNCHANGEDSINCE 44) +FLAGS.SILENT (\\Flagged)\r\n")); err != nil {
		t.Fatal(err)
	}
	scanInbound(t, guard, "T4 OK [MODIFIED 7] stale tag\r\nT5 OK stored\r\n")
	proof := observation.proof()
	if !proof.observed || !proof.ok || proof.modified || proof.failed {
		t.Fatalf("second observation proof = %+v", proof)
	}
}

// scanInbound feeds one inbound chunk through the real protocol scan, which is
// how a session read reports protocol lines.
func scanInbound(t *testing.T, guard *guardedConn, data string) {
	t.Helper()
	for index := 0; index < len(data); index++ {
		if err := guard.scanProtocolByte(data[index]); err != nil {
			t.Fatalf("protocol scan rejected valid input: %v", err)
		}
	}
}
