package mail

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-message"
	msgmail "github.com/emersion/go-message/mail"
)

// The attachment protocol tests run the production IMAP adapter over the wire
// against the go-imap in-memory server, and the production SMTP client against
// the scripted SMTP fixture. Like iCloud, the in-memory server marks no
// SPECIAL-USE Drafts mailbox, so the fixture configures the Drafts name.

const attachmentFixtureAddress = "fixture@icloud.com"

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn, 16), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return fakeAddr("pipe") }

type attachmentFixture struct {
	client *Client
	user   *imapmemserver.User
	smtp   *archiveProtocol
	files  map[string][]byte
	uid    uint32
	uidVal uint32
}

type attachmentFixtureOptions struct {
	read, send, drafts bool
	draftsMailbox      string
	extraParts         []fixturePart
	// noRecipientPolicy leaves the recipient allowlist unset. SMTP send is
	// then off, as in a drafts-only profile.
	noRecipientPolicy bool
}

type fixturePart struct {
	filename, contentType, encoding string
	data                            []byte
}

func fixtureDocx(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var document strings.Builder
	document.WriteString(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, paragraph := range paragraphs {
		document.WriteString("<w:p><w:r><w:t>" + paragraph + "</w:t></w:r></w:p>")
	}
	document.WriteString("</w:body></w:document>")
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, document.String()); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func fixturePNG(t *testing.T, width, height int, noise bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewSource(7))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if noise {
				img.SetNRGBA(x, y, color.NRGBA{uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)), 0xff})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 128, 0xff})
			}
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// fixtureMessage builds multipart/mixed with a text body and the given parts.
func fixtureMessage(t *testing.T, parts []fixturePart) []byte {
	t.Helper()
	var header msgmail.Header
	header.SetAddressList("From", []*msgmail.Address{{Address: "sender@example.invalid"}})
	header.SetAddressList("To", []*msgmail.Address{{Address: attachmentFixtureAddress}})
	header.SetSubject("synthetic attachments")
	header.SetMessageID("fixture-attachments@example.invalid")
	header.SetDate(time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC))
	var buffer bytes.Buffer
	writer, err := msgmail.CreateWriter(&buffer, header)
	if err != nil {
		t.Fatal(err)
	}
	var inline msgmail.InlineHeader
	inline.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	body, err := writer.CreateSingleInline(inline)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(body, "See the attached files.")
	_ = body.Close()
	for _, part := range parts {
		var attachmentHeader msgmail.AttachmentHeader
		attachmentHeader.Header = message.Header{}
		attachmentHeader.Set("Content-Type", part.contentType)
		attachmentHeader.SetFilename(part.filename)
		if part.encoding != "" {
			attachmentHeader.Set("Content-Transfer-Encoding", part.encoding)
		}
		file, err := writer.CreateAttachment(attachmentHeader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(part.data); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func newAttachmentFixture(t *testing.T, options attachmentFixtureOptions) *attachmentFixture {
	t.Helper()
	f := &attachmentFixture{files: map[string][]byte{
		"Offerte & prijs \u00e9t\u00e9.docx": fixtureDocx(t, "Offerte voor de klant", "btw &amp; levering", "Caf\u00e9 cr\u00e8me"),
		"grafiek.png":                        fixturePNG(t, 64, 48, false),
		"notes.txt":                          []byte("caf\xe9 au lait\n"),
	}}
	parts := []fixturePart{
		{filename: "Offerte & prijs \u00e9t\u00e9.docx", contentType: docxMediaType, data: f.files["Offerte & prijs \u00e9t\u00e9.docx"]},
		{filename: "grafiek.png", contentType: "image/png", data: f.files["grafiek.png"]},
		{filename: "notes.txt", contentType: "text/x-fixture-latin1", encoding: "quoted-printable", data: f.files["notes.txt"]},
	}
	for _, extra := range options.extraParts {
		f.files[extra.filename] = extra.data
		parts = append(parts, extra)
	}
	memory := imapmemserver.New()
	f.user = imapmemserver.NewUser(attachmentFixtureAddress, "synthetic-attachment-secret")
	for _, name := range []string{"INBOX", "Drafts"} {
		if err := f.user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	// go-message cannot write a Latin-1 part, so the fixture writes a neutral
	// type and then sets the charset in the raw message.
	raw := bytes.ReplaceAll(fixtureMessage(t, parts), []byte("text/x-fixture-latin1"), []byte("text/plain; charset=iso-8859-1"))
	appended, err := f.user.Append("INBOX", imapLiteral(raw), &imap.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.uid, f.uidVal = uint32(appended.UID), appended.UIDValidity
	memory.AddUser(f.user)
	listener := newPipeListener()
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memory.NewSession(), &imapserver.GreetingData{}, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}},
		InsecureAuth: true,
		Logger:       discardIMAPLog{},
	})
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	// Closing the listener ends Serve without racing its setup.
	t.Cleanup(func() {
		_ = listener.Close()
		<-served
	})

	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	f.smtp = &archiveProtocol{}
	var policy RecipientPolicy
	if !options.noRecipientPolicy {
		policy, err = ParseRecipientPolicy("to@example.invalid")
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		Address: attachmentFixtureAddress, Password: "synthetic-attachment-secret", RecipientPolicy: policy,
		EnableAttachmentRead: options.read, EnableAttachmentSend: options.send, EnableDrafts: options.drafts,
		DraftsMailbox: options.draftsMailbox,
	}
	f.client, err = newService(cfg, func(context.Context) (net.Conn, error) {
		local, remote := net.Pipe()
		listener.conns <- remote
		return local, nil
	}, func(context.Context) (net.Conn, error) {
		local, remote := net.Pipe()
		go f.smtp.serveSMTP(remote, serverTLS)
		return local, nil
	}, false, !options.noRecipientPolicy, func(ctx context.Context, conn net.Conn, address, password string) (imapSession, error) {
		return imapadapter.NewSession(ctx, conn, address, password)
	}, func(ctx context.Context, conn net.Conn) (smtpSession, error) {
		return newSMTPSessionWithTLS(ctx, conn, clientTLS)
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type discardIMAPLog struct{}

func (discardIMAPLog) Printf(string, ...any) {}

type literal struct {
	*bytes.Reader
	size int64
}

func (l literal) Size() int64 { return l.size }

func imapLiteral(data []byte) imap.LiteralReader {
	return literal{Reader: bytes.NewReader(data), size: int64(len(data))}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (f *attachmentFixture) ref(part string) AttachmentRef {
	return AttachmentRef{Mailbox: "INBOX", UIDValidity: f.uidVal, UID: f.uid, PartID: part}
}

// partFor finds the partId of a filename through get_message metadata, as an
// agent would.
func (f *attachmentFixture) partFor(t *testing.T, filename string) string {
	t.Helper()
	message, err := f.client.GetMessage(t.Context(), GetMessageInput{Mailbox: "INBOX", UIDValidity: f.uidVal, UID: f.uid})
	if err != nil {
		t.Fatal(err)
	}
	for _, attachment := range message.Attachments {
		if attachment.Filename == filename {
			return attachment.PartID
		}
	}
	t.Fatalf("attachment %q not listed in %+v", filename, message.Attachments)
	return ""
}

func (f *attachmentFixture) unseen(t *testing.T) bool {
	t.Helper()
	result, err := f.client.SearchMessages(t.Context(), SearchInput{Mailbox: "INBOX", UnseenOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(result.Messages) == 1
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	public := AsError(err)
	if public == nil || public.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestProtocolGetAttachment(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true})
	docx := f.partFor(t, "Offerte & prijs \u00e9t\u00e9.docx")
	pngPart := f.partFor(t, "grafiek.png")
	notes := f.partFor(t, "notes.txt")

	text, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref(docx)})
	if err != nil {
		t.Fatal(err)
	}
	if text.Delivery != DeliveryText || !strings.Contains(text.Text, "btw & levering") || !strings.Contains(text.Text, "Caf\u00e9 cr\u00e8me") {
		t.Fatalf("docx text = %+v", text)
	}
	if text.SHA256 != digest(f.files["Offerte & prijs \u00e9t\u00e9.docx"]) || text.Filename != "Offerte & prijs \u00e9t\u00e9.docx" {
		t.Fatalf("docx identity = %s %q", text.SHA256, text.Filename)
	}

	original, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref(docx), Format: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if original.Delivery != DeliveryResource || !bytes.Equal(original.Data, f.files["Offerte & prijs \u00e9t\u00e9.docx"]) {
		t.Fatalf("docx original delivery = %s, identical = %v", original.Delivery, bytes.Equal(original.Data, f.files["Offerte & prijs \u00e9t\u00e9.docx"]))
	}

	img, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref(pngPart)})
	if err != nil {
		t.Fatal(err)
	}
	if img.Delivery != DeliveryImage || img.DataMIME != "image/png" || !bytes.Equal(img.Data, f.files["grafiek.png"]) || img.Width != 64 || img.Height != 48 {
		t.Fatalf("png delivery = %s %s %dx%d", img.Delivery, img.DataMIME, img.Width, img.Height)
	}

	latin, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref(notes)})
	if err != nil {
		t.Fatal(err)
	}
	// Quoted-printable text uses canonical CRLF line ends on the wire.
	if latin.Text != "caf\u00e9 au lait\r\n" || latin.SHA256 != digest([]byte("caf\xe9 au lait\r\n")) {
		t.Fatalf("quoted-printable latin-1 text = %q sha %s", latin.Text, latin.SHA256)
	}

	_, err = f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref("9")})
	wantCode(t, err, CodeNotFound)
	stale := f.ref(docx)
	stale.UIDValidity++
	_, err = f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: stale})
	wantCode(t, err, CodeConcurrentModification)
	_, err = f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref("0.1")})
	wantCode(t, err, CodeValidation)
	if !f.unseen(t) {
		t.Fatal("get_attachment changed the Seen flag")
	}
}

func TestProtocolGetAttachmentImageLimits(t *testing.T) {
	large := fixturePNG(t, 700, 500, true)
	if len(large) <= MaxAttachmentInlineBytes {
		t.Fatalf("fixture image is %d bytes, want above the inline limit", len(large))
	}
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, extraParts: []fixturePart{{filename: "noise.png", contentType: "image/png", data: large}}})
	part := f.partFor(t, "noise.png")
	resized, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref(part)})
	if err != nil {
		t.Fatal(err)
	}
	if resized.Delivery != DeliveryImageResized || resized.DataMIME != "image/jpeg" || len(resized.Data) > MaxAttachmentInlineBytes || resized.SHA256 != digest(large) {
		t.Fatalf("resized = %s %s %d bytes", resized.Delivery, resized.DataMIME, len(resized.Data))
	}
	if _, _, err := image.Decode(bytes.NewReader(resized.Data)); err != nil {
		t.Fatalf("resized image does not decode: %v", err)
	}
	_, err = f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref(part), Format: "original"})
	wantCode(t, err, CodePayloadTooLarge)
}

func TestProtocolSendWithAttachmentReferences(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, send: true})
	docx, pngPart := f.partFor(t, "Offerte & prijs \u00e9t\u00e9.docx"), f.partFor(t, "grafiek.png")
	input := SendInput{To: []string{"to@example.invalid"}, Subject: "forward", Body: "Two files.", IdempotencyKey: "attachment-key",
		Attachments: []AttachmentRef{f.ref(docx), f.ref(pngPart)}}
	result, err := f.client.SendMessage(t.Context(), input)
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send = %+v, %v", result, err)
	}
	f.smtp.mu.Lock()
	submitted := append([]byte(nil), f.smtp.submitted...)
	f.smtp.mu.Unlock()
	reader, err := msgmail.CreateReader(bytes.NewReader(submitted))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header, ok := part.Header.(*msgmail.AttachmentHeader); ok {
			name, _ := header.Filename()
			got[name], _ = io.ReadAll(part.Body)
		}
	}
	for _, name := range []string{"Offerte & prijs \u00e9t\u00e9.docx", "grafiek.png"} {
		if !bytes.Equal(got[name], f.files[name]) {
			t.Fatalf("submitted attachment %q differs or is missing; got names %v", name, len(got))
		}
	}
	// The same key with a changed attachment list must conflict, not replay.
	input.Attachments = input.Attachments[:1]
	_, err = f.client.SendMessage(t.Context(), input)
	wantCode(t, err, CodeConflict)
	if !f.unseen(t) {
		t.Fatal("attachment fetch changed the Seen flag")
	}
}

func TestProtocolStageDraftAndSendByReference(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, send: true, drafts: true, draftsMailbox: "Drafts"})
	file := fixtureDocx(t, "Nieuw document")
	first, err := f.client.StageAttachment(t.Context(), StageInput{Filename: "nieuw r\u00e9sum\u00e9.docx", ContentType: docxMediaType, Data: base64.StdEncoding.EncodeToString(file[:100])})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.client.StageAttachment(t.Context(), StageInput{Handle: first.Handle, Data: base64.StdEncoding.EncodeToString(file[100:])})
	if err != nil || second.SHA256 != digest(file) {
		t.Fatalf("staged = %+v, %v", second, err)
	}
	draft, err := f.client.CreateDraft(t.Context(), DraftInput{To: []string{"to@example.invalid"}, Subject: "draft", Body: "Draft body.", Staged: []string{first.Handle}, Attachments: []AttachmentRef{f.ref(f.partFor(t, "grafiek.png"))}})
	if err != nil || draft.Status != "stored" || draft.Mailbox != "Drafts" || len(draft.Attachments) != 2 {
		t.Fatalf("draft = %+v, %v", draft, err)
	}
	if draft.Attachments[0].SHA256 != digest(file) || draft.Attachments[1].SHA256 != digest(f.files["grafiek.png"]) {
		t.Fatalf("draft digests = %+v", draft.Attachments)
	}
	_, err = f.client.StageAttachment(t.Context(), StageInput{Handle: first.Handle, Data: "AAAA"})
	wantCode(t, err, CodeNotFound)

	draftRef := AttachmentRef{Mailbox: "Drafts", UIDValidity: draft.UIDValidity, UID: draft.UID, PartID: draft.Attachments[0].PartID}
	stored, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: draftRef, Format: "original"})
	if err != nil || !bytes.Equal(stored.Data, file) || stored.Filename != "nieuw r\u00e9sum\u00e9.docx" {
		t.Fatalf("draft attachment = %q identical=%v, %v", stored.Filename, bytes.Equal(stored.Data, file), err)
	}
	result, err := f.client.SendMessage(t.Context(), SendInput{To: []string{"to@example.invalid"}, Body: "Sent from the draft.", Attachments: []AttachmentRef{draftRef}})
	if err != nil || result.Status != SendAccepted {
		t.Fatalf("send from draft = %+v, %v", result, err)
	}
	// The recipient allowlist still applies to sending.
	_, err = f.client.SendMessage(t.Context(), SendInput{To: []string{"anyone@example.invalid"}, Body: "x", Attachments: []AttachmentRef{draftRef}})
	wantCode(t, err, CodeAuthorization)
}

func TestProtocolDraftMailboxResolution(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{read: true, drafts: true})
	_, err := f.client.CreateDraft(t.Context(), DraftInput{Body: "x"})
	wantCode(t, err, CodeProtocolError)
	f = newAttachmentFixture(t, attachmentFixtureOptions{read: true, drafts: true, draftsMailbox: "Missing"})
	_, err = f.client.CreateDraft(t.Context(), DraftInput{Body: "x"})
	wantCode(t, err, CodeNotFound)
	f = newAttachmentFixture(t, attachmentFixtureOptions{read: true, drafts: true, draftsMailbox: "INBOX"})
	_, err = f.client.CreateDraft(t.Context(), DraftInput{Body: "x"})
	wantCode(t, err, CodeAuthorization)
}

func TestProtocolAttachmentCapabilitiesAreOffByDefault(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{})
	_, err := f.client.GetAttachment(t.Context(), GetAttachmentInput{AttachmentRef: f.ref("2")})
	wantCode(t, err, CodeAuthorization)
	_, err = f.client.SendMessage(t.Context(), SendInput{To: []string{"to@example.invalid"}, Body: "x", Attachments: []AttachmentRef{f.ref("2")}})
	wantCode(t, err, CodeAuthorization)
	_, err = f.client.StageAttachment(t.Context(), StageInput{Filename: "a.txt", ContentType: "text/plain", Data: "AAAA"})
	wantCode(t, err, CodeAuthorization)
	_, err = f.client.CreateDraft(t.Context(), DraftInput{Body: "x"})
	wantCode(t, err, CodeAuthorization)
}

func TestStageAttachmentLimits(t *testing.T) {
	f := newAttachmentFixture(t, attachmentFixtureOptions{drafts: true, draftsMailbox: "Drafts"})
	for _, input := range []StageInput{
		{Filename: "../etc/passwd", ContentType: "text/plain", Data: "AAAA"},
		{Filename: "a\x00b", ContentType: "text/plain", Data: "AAAA"},
		{Filename: "a.txt", ContentType: "text/plain; charset=utf-8", Data: "AAAA"},
		{Filename: "a.txt", ContentType: "multipart/mixed", Data: "AAAA"},
		{Filename: "a.txt", ContentType: "text/plain", Data: "not base64!"},
	} {
		_, err := f.client.StageAttachment(t.Context(), input)
		wantCode(t, err, CodeValidation)
	}
	chunk := base64.StdEncoding.EncodeToString(make([]byte, 384*1024))
	first, err := f.client.StageAttachment(t.Context(), StageInput{Filename: "big.bin", ContentType: "application/octet-stream", Data: chunk})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err = f.client.StageAttachment(t.Context(), StageInput{Handle: first.Handle, Data: chunk}); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	_, err = f.client.StageAttachment(t.Context(), StageInput{Handle: first.Handle, Data: chunk})
	wantCode(t, err, CodePayloadTooLarge)
	_, err = f.client.StageAttachment(t.Context(), StageInput{Handle: first.Handle, Filename: "other.bin", Data: "AAAA"})
	wantCode(t, err, CodeValidation)

	f.client.now = func() time.Time { return time.Now().Add(StagedAttachmentTTL + time.Minute) }
	_, err = f.client.StageAttachment(t.Context(), StageInput{Handle: first.Handle, Data: "AAAA"})
	wantCode(t, err, CodeNotFound)
	if f.client.staging.total != 0 {
		t.Fatalf("expired staging memory = %d bytes, want 0", f.client.staging.total)
	}
}
