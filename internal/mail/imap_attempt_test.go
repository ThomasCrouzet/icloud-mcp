package mail

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

// TestImapAttemptPreservesAuthenticationErrorOnFailedSession pins the live
// Minji failure mode: NewSession closes the dialed conn on auth rejection, and
// a second Close must not panic-rewrite the error into protocol_error.
func TestImapAttemptPreservesAuthenticationErrorOnFailedSession(t *testing.T) {
	client, err := newService(Config{
		Address:  "user@example.com",
		Password: "password-long-enough",
	}, func(ctx context.Context) (net.Conn, error) {
		return &panicSecondClose{}, nil
	}, nil, false, false, func(ctx context.Context, conn net.Conn, address, password string) (imapSession, error) {
		_ = conn.Close()
		return nil, &imapadapter.Error{Kind: imapadapter.ErrorAuthentication}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListMailboxes(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	mailErr := AsError(err)
	if mailErr == nil {
		t.Fatalf("unclassified error: %T %v", err, err)
	}
	if mailErr.Code != CodeAuthentication {
		t.Fatalf("code=%s message=%s want authentication", mailErr.Code, mailErr.Message)
	}
}

type panicSecondClose struct {
	closed bool
}

func (p *panicSecondClose) Read(b []byte) (int, error)  { return 0, net.ErrClosed }
func (p *panicSecondClose) Write(b []byte) (int, error) { return 0, net.ErrClosed }
func (p *panicSecondClose) Close() error {
	if p.closed {
		panic("double close")
	}
	p.closed = true
	return nil
}
func (p *panicSecondClose) LocalAddr() net.Addr                { return dummyAddr{} }
func (p *panicSecondClose) RemoteAddr() net.Addr               { return dummyAddr{} }
func (p *panicSecondClose) SetDeadline(t time.Time) error      { return nil }
func (p *panicSecondClose) SetReadDeadline(t time.Time) error  { return nil }
func (p *panicSecondClose) SetWriteDeadline(t time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "127.0.0.1:993" }
