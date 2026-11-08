package imapadapter

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Concrete synthetic-wire metadata fixture. The real adapter Session talks to
// the go-imap server over net.Pipe. The server writes FETCH data on the wire,
// and the client parser plus convertMessage build the adapter Message. This is
// not a live Apple connection and not a full mailbox service fixture.
const (
	imapWireBenchmarkSeed      = "imap-wire-metadata-v1"
	imapWireBenchmarkUID       = uint32(4_000_000_000)
	imapWireBenchmarkUIDNext   = imapWireBenchmarkUID + 1
	imapWireBenchmarkBodyBytes = 204800
)

var imapWireBenchmarkHeaders = []byte(
	"Message-ID: <wire-benchmark@example.test>\r\n" +
		"In-Reply-To: <wire-parent@example.test>\r\n" +
		"References: <wire-root@example.test> <wire-parent@example.test>\r\n" +
		"Reply-To: Wire Bench <reply@example.test>\r\n" +
		"\r\n",
)

type singleConnListener struct {
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{conn: conn, closed: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var accepted net.Conn
	l.once.Do(func() { accepted = l.conn })
	if accepted != nil {
		return accepted, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type imapWireBenchmarkSession struct {
	imapserver.Session
	fetches atomic.Int64
	closes  atomic.Int64
}

func (s *imapWireBenchmarkSession) Close() error {
	s.closes.Add(1)
	return nil
}

func (s *imapWireBenchmarkSession) Login(string, string) error { return nil }

func (s *imapWireBenchmarkSession) Select(string, *imap.SelectOptions) (*imap.SelectData, error) {
	return &imap.SelectData{UIDValidity: 7, UIDNext: imap.UID(imapWireBenchmarkUIDNext), NumMessages: 1}, nil
}

func (s *imapWireBenchmarkSession) Poll(*imapserver.UpdateWriter, bool) error { return nil }

func (s *imapWireBenchmarkSession) Unselect() error { return nil }

func (s *imapWireBenchmarkSession) Fetch(writer *imapserver.FetchWriter, _ imap.NumSet, options *imap.FetchOptions) error {
	s.fetches.Add(1)
	message := writer.CreateMessage(1)
	if options.UID {
		message.WriteUID(imap.UID(imapWireBenchmarkUID))
	}
	if options.Flags {
		message.WriteFlags([]imap.Flag{imap.FlagSeen})
	}
	if options.InternalDate {
		message.WriteInternalDate(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	}
	if options.RFC822Size {
		message.WriteRFC822Size(imapWireBenchmarkBodyBytes + int64(len(imapWireBenchmarkHeaders)))
	}
	if options.Envelope {
		message.WriteEnvelope(&imap.Envelope{
			Date:    time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
			Subject: "wire metadata fixture",
			From: []imap.Address{{
				Name: "Wire Sender", Mailbox: "sender", Host: "example.test",
			}},
			To: []imap.Address{{
				Name: "Wire Recipient", Mailbox: "recipient", Host: "example.test",
			}},
			ReplyTo: []imap.Address{{
				Name: "Wire Bench", Mailbox: "reply", Host: "example.test",
			}},
			MessageID: "wire-benchmark@example.test",
			InReplyTo: []string{"wire-parent@example.test"},
		})
	}
	if options.BodyStructure != nil {
		message.WriteBodyStructure(&imap.BodyStructureSinglePart{
			Type: "text", Subtype: "plain", Encoding: "7bit",
			Size: imapWireBenchmarkBodyBytes,
			Text: &imap.BodyStructureText{NumLines: 1},
			Extended: &imap.BodyStructureSinglePartExt{
				Disposition: &imap.BodyStructureDisposition{Value: "inline"},
			},
		})
	}
	for _, section := range options.BodySection {
		part := message.WriteBodySection(section, int64(len(imapWireBenchmarkHeaders)))
		if _, err := part.Write(imapWireBenchmarkHeaders); err != nil {
			_ = part.Close()
			return err
		}
		if err := part.Close(); err != nil {
			return err
		}
	}
	return message.Close()
}

func imapWireBenchmarkFixture(tb testing.TB) (*Session, *imapWireBenchmarkSession) {
	tb.Helper()
	serverConn, clientConn := net.Pipe()
	fake := &imapWireBenchmarkSession{}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return fake, &imapserver.GreetingData{}, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}},
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
	})
	listener := newSingleConnListener(serverConn)
	go func() { _ = server.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	session, err := NewSession(ctx, clientConn, "sender@example.test", "app-password")
	if err != nil {
		cancel()
		_ = server.Close()
		tb.Fatalf("synthetic wire session: %v", err)
	}
	if _, err := session.Select("INBOX", true); err != nil {
		_ = session.Close()
		_ = server.Close()
		cancel()
		tb.Fatalf("synthetic wire select: %v", err)
	}
	tb.Cleanup(func() {
		_ = session.Close()
		_ = server.Close()
		cancel()
	})
	return session, fake
}

func assertIMAPWireMetadata(tb testing.TB, session *Session) {
	tb.Helper()
	messages, err := session.FetchMetadata([]uint32{imapWireBenchmarkUID}, true)
	if err != nil {
		tb.Fatalf("wire metadata: %v", err)
	}
	if len(messages) != 1 {
		tb.Fatalf("wire metadata messages = %d, want 1", len(messages))
	}
	message := messages[0]
	if message.UID != imapWireBenchmarkUID || message.Envelope.Subject != "wire metadata fixture" {
		tb.Fatalf("wire metadata message = %+v", message)
	}
	if len(message.Envelope.From) != 1 || message.Envelope.From[0].Address != "sender@example.test" {
		tb.Fatalf("wire metadata sender = %+v", message.Envelope.From)
	}
	if len(message.Parts) != 1 || message.Parts[0].ContentType != "text/plain" {
		tb.Fatalf("wire metadata parts = %+v", message.Parts)
	}
	if message.Headers.MessageID != "<wire-benchmark@example.test>" ||
		len(message.Headers.InReplyTo) != 1 ||
		len(message.Headers.References) != 2 ||
		len(message.Headers.ReplyTo) != 1 {
		tb.Fatalf("wire curated headers = %+v", message.Headers)
	}
}

func TestIMAPWireBenchmarkScenario(t *testing.T) {
	t.Logf("fixture_seed=%s uid=%d", imapWireBenchmarkSeed, imapWireBenchmarkUID)
	session, fake := imapWireBenchmarkFixture(t)
	base := fake.fetches.Load()
	assertIMAPWireMetadata(t, session)
	assertIMAPWireMetadata(t, session)
	if got := fake.fetches.Load() - base; got != 2 {
		t.Fatalf("wire FETCH count = %d, want 2", got)
	}
}

func BenchmarkIMAPWireMetadata(b *testing.B) {
	session, fake := imapWireBenchmarkFixture(b)
	assertIMAPWireMetadata(b, session)
	base := fake.fetches.Load()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		assertIMAPWireMetadata(b, session)
	}
	b.StopTimer()
	b.ReportMetric(float64(fake.fetches.Load()-base)/float64(b.N), "wire-rpc/op")
	b.ReportMetric(1, "metadata-messages")
	b.ReportMetric(float64(len(imapWireBenchmarkHeaders)), "header-bytes")
	b.ReportMetric(float64(imapWireBenchmarkUID), "sparse-uid-base")
}
