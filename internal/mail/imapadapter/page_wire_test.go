package imapadapter

import (
	"bytes"
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Synthetic metadata page fixture. The real adapter Session talks to a
// go-imap server over a loopback pipe, so the test reads the FETCH command the
// client writes and the response bytes the server writes. It is offline and
// uses no account.
const (
	imapPageSeed        = "imap-wire-page-v1"
	imapPageMessages    = 20
	imapPageParts       = 40
	imapPageUIDValidity = 31
	imapPageUIDBase     = uint32(4_100_000_000)
)

type byteCountingConn struct {
	net.Conn
	written atomic.Int64
}

func (c *byteCountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

type commandRecordingConn struct {
	net.Conn
	mu   sync.Mutex
	sent bytes.Buffer
}

func (c *commandRecordingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.sent.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

func (c *commandRecordingConn) commands() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent.String()
}

type imapPageSession struct {
	imapserver.Session
	messages int
	parts    int
	depth    int
	fetches  atomic.Int64
	structs  atomic.Int64
}

func (s *imapPageSession) Close() error               { return nil }
func (s *imapPageSession) Login(string, string) error { return nil }

func (s *imapPageSession) Select(string, *imap.SelectOptions) (*imap.SelectData, error) {
	return &imap.SelectData{
		UIDValidity: imapPageUIDValidity,
		UIDNext:     imap.UID(imapPageUIDBase) + imap.UID(s.messages) + 1,
		NumMessages: uint32(s.messages),
	}, nil
}

func (s *imapPageSession) Poll(*imapserver.UpdateWriter, bool) error { return nil }
func (s *imapPageSession) Unselect() error                           { return nil }

func (s *imapPageSession) envelope(index int) *imap.Envelope {
	name := strings.Repeat("N", 120)
	return &imap.Envelope{
		Date:      time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Subject:   "Synthetic page message " + strconv.Itoa(index) + " " + strings.Repeat("x", 320),
		From:      []imap.Address{{Name: name, Mailbox: "sender", Host: "example.test"}},
		To:        []imap.Address{{Name: name, Mailbox: "recipient", Host: "example.test"}},
		Cc:        []imap.Address{{Name: name, Mailbox: "copy", Host: "example.test"}},
		MessageID: "wire-page@example.test",
		InReplyTo: []string{"wire-parent@example.test"},
	}
}

// bodyStructure returns a wide multipart tree, then nests it in extra
// multipart levels. A depth above the flatten limit models the deep trees
// reported in issue #214.
func (s *imapPageSession) bodyStructure() imap.BodyStructure {
	children := make([]imap.BodyStructure, 0, s.parts)
	for index := 0; index < s.parts; index++ {
		children = append(children, &imap.BodyStructureSinglePart{
			Type: "text", Subtype: "plain",
			Params: map[string]string{"charset": "utf-8"},
			Size:   1200,
			Text:   &imap.BodyStructureText{NumLines: 20},
			Extended: &imap.BodyStructureSinglePartExt{
				Disposition: &imap.BodyStructureDisposition{Value: "inline"},
			},
		})
	}
	var root imap.BodyStructure = &imap.BodyStructureMultiPart{
		Subtype:  "mixed",
		Children: children,
		Extended: &imap.BodyStructureMultiPartExt{Params: map[string]string{"boundary": "wire-boundary"}},
	}
	for index := 0; index < s.depth; index++ {
		root = &imap.BodyStructureMultiPart{
			Subtype:  "mixed",
			Children: []imap.BodyStructure{root},
			Extended: &imap.BodyStructureMultiPartExt{Params: map[string]string{"boundary": "wire-boundary"}},
		}
	}
	return root
}

var imapPageHeaders = []byte(
	"Message-ID: <wire-page@example.test>\r\n" +
		"In-Reply-To: <wire-parent@example.test>\r\n" +
		"References: <wire-root@example.test> <wire-parent@example.test>\r\n" +
		"Reply-To: Wire Page <reply@example.test>\r\n" +
		"\r\n",
)

var imapPagePartMIME = []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n")

func (s *imapPageSession) writeSection(message *imapserver.FetchResponseWriter, section *imap.FetchItemBodySection) error {
	var data []byte
	switch {
	case section.Specifier == imap.PartSpecifierHeader:
		data = imapPageHeaders
	case section.Specifier == imap.PartSpecifierMIME:
		data = imapPagePartMIME
	case len(section.Part) > 0:
		data = []byte("wire page body")
	default:
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "unsupported page section"}
	}
	part := message.WriteBodySection(section, int64(len(data)))
	if _, err := part.Write(data); err != nil {
		_ = part.Close()
		return err
	}
	return part.Close()
}

// selected maps a requested UID set onto this fixture's sequence numbers. The
// fixture writes one FETCH response per requested UID so per-UID costs are
// visible on the wire.
func (s *imapPageSession) selected(numSet imap.NumSet) []uint32 {
	set, ok := numSet.(imap.UIDSet)
	if !ok {
		return nil
	}
	out := make([]uint32, 0, len(set))
	for _, r := range set {
		for uid := uint64(r.Start); uid <= uint64(r.Stop) && len(out) < s.messages; uid++ {
			if uid < uint64(imapPageUIDBase)+1 || uid > uint64(imapPageUIDBase)+uint64(s.messages) {
				continue
			}
			out = append(out, uint32(uid))
		}
	}
	return out
}

func (s *imapPageSession) Fetch(writer *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	s.fetches.Add(1)
	if options.BodyStructure != nil {
		s.structs.Add(1)
	}
	for index, uid := range s.selected(numSet) {
		message := writer.CreateMessage(uint32(index + 1))
		if options.UID {
			message.WriteUID(imap.UID(uid))
		}
		if options.Flags {
			message.WriteFlags([]imap.Flag{imap.FlagSeen})
		}
		if options.InternalDate {
			message.WriteInternalDate(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
		}
		if options.RFC822Size {
			message.WriteRFC822Size(1 << 20)
		}
		if options.Envelope {
			message.WriteEnvelope(s.envelope(index))
		}
		if options.BodyStructure != nil {
			message.WriteBodyStructure(s.bodyStructure())
		}
		for _, section := range options.BodySection {
			if err := s.writeSection(message, section); err != nil {
				_ = message.Close()
				return err
			}
		}
		if err := message.Close(); err != nil {
			return err
		}
	}
	return nil
}

func pageLog(tb testing.TB) *log.Logger {
	return log.New(wireBenchmarkLogWriter{tb}, "", 0)
}

type pageFixture struct {
	session   *Session
	server    *imapPageSession
	commands  *commandRecordingConn
	responses *byteCountingConn
}

type pageOptions struct {
	messages int
	parts    int
	depth    int
}

func newPageFixture(tb testing.TB, options pageOptions) *pageFixture {
	tb.Helper()
	if options.messages == 0 {
		options.messages = imapPageMessages
	}
	if options.parts == 0 {
		options.parts = imapPageParts
	}
	serverConn, clientConn := net.Pipe()
	responses := &byteCountingConn{Conn: serverConn}
	commands := &commandRecordingConn{Conn: clientConn}
	fake := &imapPageSession{messages: options.messages, parts: options.parts, depth: options.depth}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return fake, &imapserver.GreetingData{}, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}},
		InsecureAuth: true,
		Logger:       pageLog(tb),
	})
	listener := newSingleConnListener(responses)
	go func() { _ = server.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	session, err := NewSession(ctx, commands, "sender@example.test", "app-password")
	if err != nil {
		cancel()
		_ = server.Close()
		tb.Fatalf("synthetic page session: %v", err)
	}
	if _, err := session.Select("INBOX", true); err != nil {
		_ = session.Close()
		_ = server.Close()
		cancel()
		tb.Fatalf("synthetic page select: %v", err)
	}
	tb.Cleanup(func() {
		_ = session.Close()
		_ = server.Close()
		cancel()
	})
	return &pageFixture{session: session, server: fake, commands: commands, responses: responses}
}

func (f *pageFixture) uids() []uint32 {
	out := make([]uint32, 0, imapPageMessages)
	for index := 1; index <= imapPageMessages; index++ {
		out = append(out, imapPageUIDBase+uint32(index))
	}
	return out
}

func (f *pageFixture) fetchCommands() []string {
	var found []string
	for _, line := range strings.Split(f.commands.commands(), "\r\n") {
		if strings.Contains(strings.ToUpper(line), " FETCH ") {
			found = append(found, line)
		}
	}
	return found
}
