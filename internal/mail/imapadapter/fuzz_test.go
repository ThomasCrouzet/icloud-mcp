package imapadapter

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func FuzzIMAPInboundGuard(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("* SEARCH 1 2 3:8 4294967295\r\n"),
		[]byte("* 1 FETCH (UID 1 FLAGS (\\Seen) BODY[1] {5}\r\n(a)b)\r\n"),
		[]byte("* OK [CAPABILITY IMAP4rev1] \"quoted ( list )\"\r\n"),
		[]byte(strings.Repeat("(", maxProtocolDepth+1)),
		[]byte("* 1 FETCH (BODY[] {4194305}\r\n"),
		[]byte("* 1 FETCH (BODY[] {-1}\r\n"),
		[]byte("* BAD unmatched ))\r\n"),
		[]byte("\r\n"),
	} {
		f.Add(seed, uint8(7))
	}
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		if len(data) > 64*1024 {
			return
		}
		conn := &fuzzConn{data: append([]byte(nil), data...)}
		guard := newGuardedConn(conn)
		buffer := make([]byte, int(chunk)%128+1)
		for reads := 0; reads <= len(data)+1; reads++ {
			_, err := guard.Read(buffer)
			if err != nil {
				return
			}
		}
		t.Fatal("guard did not terminate at EOF")
	})
}

func FuzzCompactUIDSetExpansion(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 3}, uint32(1), uint32(3), uint16(3))
	f.Add([]byte{0xff, 0xff, 0xff, 0xfe, 0xff, 0xff, 0xff, 0xff}, ^uint32(0)-1, ^uint32(0), uint16(2))
	f.Add([]byte{0, 0, 0, 3, 0, 0, 0, 1}, uint32(1), uint32(3), uint16(3))
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 0}, uint32(1), uint32(10), uint16(10))
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 5, 0, 0, 0, 5, 0, 0, 0, 8}, uint32(1), uint32(8), uint16(8))
	f.Fuzz(func(t *testing.T, encoded []byte, requestedMin, requestedMax uint32, rawCap uint16) {
		if len(encoded) > 1024 {
			return
		}
		set := make(imap.UIDSet, 0, len(encoded)/8)
		for len(encoded) >= 8 {
			set = append(set, imap.UIDRange{
				Start: imap.UID(binary.BigEndian.Uint32(encoded[:4])),
				Stop:  imap.UID(binary.BigEndian.Uint32(encoded[4:8])),
			})
			encoded = encoded[8:]
		}
		values, err := expandUIDSet(set, requestedMin, requestedMax, uint64(rawCap)%5001)
		if err == nil && len(values) > 5000 {
			t.Fatalf("expanded %d UIDs above the fuzz cap", len(values))
		}
	})
}

func TestAdapterConversionsAndLimits(t *testing.T) {
	plain := &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"}, Size: 12}
	attachment := &imap.BodyStructureSinglePart{
		Type: "application", Subtype: "pdf", ID: "part-id", Size: 20,
		Extended: &imap.BodyStructureSinglePartExt{Disposition: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "file.pdf"}}},
	}
	root := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{plain, attachment}}
	parts, err := flattenBodyStructure(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || !parts[1].InlinePlain || !parts[2].Attachment || parts[2].Filename != "file.pdf" {
		t.Fatalf("flattened parts = %#v", parts)
	}

	envelope := &imap.Envelope{
		Date: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC), Subject: "subject",
		From:      []imap.Address{{Name: "Sender", Mailbox: "sender", Host: "example.com"}},
		To:        []imap.Address{{Mailbox: "recipient", Host: "example.net"}},
		MessageID: "message@example.com", InReplyTo: []string{"parent@example.com"},
	}
	buffer := &imapclient.FetchMessageBuffer{
		UID: 9, Flags: []imap.Flag{imap.FlagSeen}, Envelope: envelope,
		InternalDate: envelope.Date, RFC822Size: 32, BodyStructure: root, ModSeq: 4,
	}
	message, err := convertMessage(buffer, false)
	if err != nil {
		t.Fatal(err)
	}
	if message.UID != 9 || len(message.Envelope.From) != 1 || len(message.Parts) != 3 || message.Flags[0] != string(imap.FlagSeen) {
		t.Fatalf("converted message = %#v", message)
	}
	for _, invalid := range []*imapclient.FetchMessageBuffer{
		{},
		{UID: 1, Envelope: envelope},
		{UID: 1, BodyStructure: plain},
	} {
		if _, err := convertMessage(invalid, false); err == nil {
			t.Fatalf("invalid fetch buffer accepted: %#v", invalid)
		}
	}

	deep := imap.BodyStructure(plain)
	for range maxDepth + 2 {
		deep = &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{deep}}
	}
	if _, err := flattenBodyStructure(deep); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("deep body structure error = %v", err)
	}
	wide := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: make([]imap.BodyStructure, maxParts)}
	for i := range wide.Children {
		wide.Children[i] = plain
	}
	if _, err := flattenBodyStructure(wide); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("wide body structure error = %v", err)
	}

	tooMany := &imap.Envelope{Subject: "subject", Bcc: make([]imap.Address, maxAddresses+1)}
	if _, err := convertEnvelope(tooMany); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("address cap error = %v", err)
	}
	if _, err := convertEnvelope(&imap.Envelope{Subject: strings.Repeat("x", maxMetadataString+1)}); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("metadata cap error = %v", err)
	}
}

func TestCuratedHeaderAndCopyUIDParsing(t *testing.T) {
	headers, err := parseCuratedHeaders([]byte("Message-ID: <message@example.com>\r\nIn-Reply-To: <parent@example.com>\r\nReferences: <first@example.com> <second@example.com>\r\nReply-To: Person <reply@example.com>\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if headers.MessageID != "message@example.com" || len(headers.References) != 2 || len(headers.ReplyTo) != 1 {
		t.Fatalf("headers = %#v", headers)
	}
	for _, data := range [][]byte{
		[]byte("Message-ID: not-an-id\r\n\r\n"),
		[]byte("Reply-To: invalid address\r\n\r\n"),
		[]byte("References: not-an-id\r\n\r\n"),
	} {
		if _, err := parseCuratedHeaders(data); err == nil {
			t.Fatalf("invalid curated headers accepted: %q", data)
		}
	}

	one := imap.UIDSetNum(42)
	got, err := validatedCopyData(42, 7, one, one, true)
	if err != nil || got.UIDValidity != 7 || got.DestinationUID != 42 {
		t.Fatalf("single COPYUID = %#v", got)
	}
	multiple := imap.UIDSetNum(1, 2)
	if _, err := validatedCopyData(42, 7, one, multiple, true); err == nil {
		t.Fatal("multiple COPYUID became singular")
	}
	if _, err := validatedCopyData(42, 7, one, imap.SearchRes(), true); err == nil {
		t.Fatal("dynamic COPYUID became singular")
	}
}

func TestAdapterErrorClassification(t *testing.T) {
	for _, test := range []struct {
		code imap.ResponseCode
		kind ErrorKind
	}{
		{imap.ResponseCodeAuthenticationFailed, ErrorAuthentication},
		{imap.ResponseCodeAuthorizationFailed, ErrorAuthorization},
		{imap.ResponseCodeNoPerm, ErrorAuthorization},
		{imap.ResponseCodeNonExistent, ErrorNotFound},
		{imap.ResponseCodeAlreadyExists, ErrorConflict},
		{imap.ResponseCodeLimit, ErrorRateLimited},
		{imap.ResponseCodeUnavailable, ErrorUnavailable},
		{imap.ResponseCodeTooBig, ErrorTooLarge},
	} {
		err := classify(&imap.Error{Type: imap.StatusResponseTypeNo, Code: test.code}, false)
		if !isAdapterKind(err, test.kind) {
			t.Errorf("response code %q classified as %v", test.code, err)
		}
	}
	if err := classify(errInboundLimit, true); !isAdapterKind(err, ErrorTooLarge) || !err.(*Error).Ambiguous {
		t.Fatalf("inbound limit classification = %#v", err)
	}
	if err := classify(timeoutError{}, true); !isAdapterKind(err, ErrorTimeout) || !err.(*Error).Ambiguous {
		t.Fatalf("timeout classification = %#v", err)
	}
	if err := classify(io.ErrUnexpectedEOF, false); !isAdapterKind(err, ErrorUnavailable) {
		t.Fatalf("EOF classification = %#v", err)
	}
	if err := classify(errors.New("bad protocol"), false); !isAdapterKind(err, ErrorProtocol) {
		t.Fatalf("fallback classification = %#v", err)
	}
	if classify(nil, false) != nil {
		t.Fatal("nil error was classified")
	}
	if (&Error{Kind: ErrorProtocol}).Error() != "sanitized IMAP failure" {
		t.Fatal("adapter error text changed")
	}
}

func TestAdapterListSelectAndSearchProtocol(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 SPECIAL-USE] ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		login, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := fmt.Fprintf(serverConn, "%s OK [CAPABILITY IMAP4rev1 SPECIAL-USE] authenticated\r\n", commandTag(login)); err != nil {
			serverErr <- err
			return
		}
		list, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(strings.ToUpper(list), " LIST ") {
			serverErr <- fmt.Errorf("LIST command = %q, %v", list, err)
			return
		}
		if _, err := fmt.Fprintf(serverConn, "* LIST (\\Archive) \"/\" \"Archive\"\r\n%s OK listed\r\n", commandTag(list)); err != nil {
			serverErr <- err
			return
		}
		examine, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(strings.ToUpper(examine), " EXAMINE ") {
			serverErr <- fmt.Errorf("EXAMINE command = %q, %v", examine, err)
			return
		}
		if _, err := fmt.Fprintf(serverConn, "* FLAGS (\\Seen \\Flagged)\r\n* 0 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n* OK [UIDNEXT 10] next\r\n%s OK [READ-ONLY] selected\r\n", commandTag(examine)); err != nil {
			serverErr <- err
			return
		}
		search, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(strings.ToUpper(search), " UID SEARCH ") {
			serverErr <- fmt.Errorf("UID SEARCH command = %q, %v", search, err)
			return
		}
		if _, err := fmt.Fprintf(serverConn, "* SEARCH 2 9 4\r\n%s OK searched\r\n", commandTag(search)); err != nil {
			serverErr <- err
			return
		}
		logout, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		_, err = fmt.Fprintf(serverConn, "* BYE closing\r\n%s OK logout\r\n", commandTag(logout))
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if !session.Capabilities().SpecialUse || session.SupportsModifiedDetection() {
		t.Fatalf("capabilities = %#v", session.Capabilities())
	}
	mailboxes, err := session.List()
	if err != nil || len(mailboxes) != 1 || mailboxes[0].Name != "Archive" {
		t.Fatalf("List() = %#v, %v", mailboxes, err)
	}
	selected, err := session.Select("INBOX", true)
	if err != nil || selected.UIDValidity != 7 || selected.UIDNext != 10 {
		t.Fatalf("Select() = %#v, %v", selected, err)
	}
	uids, err := session.Search(SearchRequest{
		UIDMin: 1, UIDMax: 9, Query: "needle", From: "from@example.com", To: "to@example.com",
		Subject: "subject", Since: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), UnseenOnly: true, FlaggedOnly: true,
	})
	if err != nil || len(uids) != 3 || uids[0] != 9 || uids[1] != 4 || uids[2] != 2 {
		t.Fatalf("Search() = %v, %v", uids, err)
	}
	_ = session.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAdapterMetadataAndMutationProtocol(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 MOVE UIDPLUS] ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		login, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := fmt.Fprintf(serverConn, "%s OK [CAPABILITY IMAP4rev1 MOVE UIDPLUS] authenticated\r\n", commandTag(login)); err != nil {
			serverErr <- err
			return
		}

		fetch, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(strings.ToUpper(fetch), " UID FETCH ") {
			serverErr <- fmt.Errorf("metadata FETCH command = %q, %v", fetch, err)
			return
		}
		metadata := `* 1 FETCH (UID 2 FLAGS (\Seen) INTERNALDATE "25-Jul-2026 12:00:00 +0000" RFC822.SIZE 12 ENVELOPE ("Sat, 25 Jul 2026 12:00:00 +0000" "subject" (("Sender" NIL "sender" "example.com")) (("Sender" NIL "sender" "example.com")) (("Sender" NIL "sender" "example.com")) ((NIL NIL "recipient" "example.net")) NIL NIL NIL "<message@example.com>") BODYSTRUCTURE ("TEXT" "PLAIN" ("CHARSET" "UTF-8") NIL NIL "7BIT" 12 1))` + "\r\n"
		if _, err := fmt.Fprintf(serverConn, "%s%s OK fetched\r\n", metadata, commandTag(fetch)); err != nil {
			serverErr <- err
			return
		}

		steps := []struct {
			contains string
			reply    func(string) string
		}{
			{" UID STORE ", func(tag string) string { return tag + " OK stored\r\n" }},
			{" UID STORE ", func(tag string) string { return tag + " OK stored\r\n" }},
			{" UID FETCH ", func(tag string) string {
				return "* 1 FETCH (UID 2 FLAGS (\\Seen \\Flagged))\r\n" + tag + " OK fetched\r\n"
			}},
			{" UID MOVE ", func(tag string) string { return tag + " OK [COPYUID 9 2 20] moved\r\n" }},
			{" UID COPY ", func(tag string) string { return tag + " OK [COPYUID 9 2 21] copied\r\n" }},
			{" UID STORE ", func(tag string) string { return tag + " OK deleted\r\n" }},
			{" UID EXPUNGE ", func(tag string) string { return tag + " OK expunged\r\n" }},
		}
		for _, step := range steps {
			command, err := reader.ReadString('\n')
			if err != nil || !strings.Contains(strings.ToUpper(command), step.contains) {
				serverErr <- fmt.Errorf("command = %q, want %q: %v", command, step.contains, err)
				return
			}
			if _, err := fmt.Fprint(serverConn, step.reply(commandTag(command))); err != nil {
				serverErr <- err
				return
			}
		}
		logout, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		_, err = fmt.Fprintf(serverConn, "* BYE closing\r\n%s OK logout\r\n", commandTag(logout))
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := session.FetchMetadata([]uint32{2}, false)
	if err != nil || len(messages) != 1 || messages[0].UID != 2 || messages[0].Envelope.Subject != "subject" || len(messages[0].Parts) != 1 {
		t.Fatalf("FetchMetadata() = %#v, %v", messages, err)
	}
	if err := session.StoreDelta(2, true, []string{"\\Seen"}); err != nil {
		t.Fatal(err)
	}
	if err := session.StoreDelta(2, false, []string{"\\Flagged"}); err != nil {
		t.Fatal(err)
	}
	flags, err := session.FetchFlags(2)
	if err != nil || flags.UID != 2 || len(flags.Flags) != 2 {
		t.Fatalf("FetchFlags() = %#v, %v", flags, err)
	}
	moved, err := session.NativeMove(2, "Archive")
	if err != nil || moved.UIDValidity != 9 || moved.DestinationUID != 20 {
		t.Fatalf("NativeMove() = %#v, %v", moved, err)
	}
	copied, err := session.Copy(2, "Archive")
	if err != nil || copied.UIDValidity != 9 || copied.DestinationUID != 21 {
		t.Fatalf("Copy() = %#v, %v", copied, err)
	}
	if err := session.AddDeleted(2); err != nil {
		t.Fatal(err)
	}
	if err := session.UIDExpunge(2); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAdapterRejectsUnsafeRequestsBeforeCommands(t *testing.T) {
	session := &Session{}
	if messages, err := session.FetchMetadata(nil, false); err != nil || messages != nil {
		t.Fatalf("empty metadata request = %#v, %v", messages, err)
	}
	if _, err := session.FetchMetadata(make([]uint32, maxMetadataMessages+1), false); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("metadata count error = %v", err)
	}
	for _, uids := range [][]uint32{{0}, {1, 1}} {
		if _, err := session.FetchMetadata(uids, false); !isAdapterKind(err, ErrorProtocol) {
			t.Fatalf("metadata UIDs %v error = %v", uids, err)
		}
	}
	for _, request := range []SearchRequest{{}, {UIDMin: 2, UIDMax: 1}} {
		if _, err := session.Search(request); !isAdapterKind(err, ErrorProtocol) {
			t.Fatalf("search request %+v error = %v", request, err)
		}
	}
	if _, err := session.Search(SearchRequest{UIDMin: 1, UIDMax: maxSearchWindowUIDs + 1}); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("wide search error = %v", err)
	}
	if _, err := session.NativeMove(1, "Archive"); !isAdapterKind(err, ErrorProtocol) {
		t.Fatalf("MOVE capability error = %v", err)
	}
	if err := session.UIDExpunge(1); !isAdapterKind(err, ErrorProtocol) {
		t.Fatalf("UID EXPUNGE capability error = %v", err)
	}
}

func isAdapterKind(err error, kind ErrorKind) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Kind == kind
}

type fuzzConn struct {
	data   []byte
	offset int
}

func (c *fuzzConn) Read(p []byte) (int, error) {
	if c.offset >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.offset:])
	c.offset += n
	return n, nil
}

func (*fuzzConn) Write(p []byte) (int, error)      { return len(p), nil }
func (*fuzzConn) Close() error                     { return nil }
func (*fuzzConn) LocalAddr() net.Addr              { return fuzzAddr("local") }
func (*fuzzConn) RemoteAddr() net.Addr             { return fuzzAddr("remote") }
func (*fuzzConn) SetDeadline(time.Time) error      { return nil }
func (*fuzzConn) SetReadDeadline(time.Time) error  { return nil }
func (*fuzzConn) SetWriteDeadline(time.Time) error { return nil }

type fuzzAddr string

func (a fuzzAddr) Network() string { return "fuzz" }
func (a fuzzAddr) String() string  { return string(a) }

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
