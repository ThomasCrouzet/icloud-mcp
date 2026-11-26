package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime/quotedprintable"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

// Attachment limits. The wire limit stays below the 4 MiB IMAP session
// inbound guard, so one session can fetch every part of one outbound message.
// The inline limit keeps one tool result below the 1 MiB stdio frame after
// base64 encoding.
const (
	MaxAttachmentWireBytes     = 3 * 1024 * 1024
	MaxAttachmentInlineBytes   = 600 * 1024
	MaxOutboundAttachments     = 10
	MaxOutboundAttachmentBytes = 2 * 1024 * 1024
	MaxEncodedWithAttachments  = 3*1024*1024 + 512*1024
	DefaultAttachmentTextBytes = 100 * 1024
	MaxAttachmentTextBytes     = 200 * 1024
	MaxPartIDLength            = 64
	MaxAttachmentFilename      = 255
	MaxAttachmentContentType   = 255
	MaxStageChunkBase64        = 512 * 1024
	MaxStagedFiles             = 8
	MaxStagedTotalBytes        = 4 * 1024 * 1024
	StagedAttachmentTTL        = 30 * time.Minute
)

// Attachment delivery kinds reported by GetAttachment.
const (
	DeliveryImage        = "image"
	DeliveryImageResized = "image_resized"
	DeliveryText         = "text"
	DeliveryResource     = "resource"
)

// AttachmentRef identifies one MIME part of one message on the IMAP server.
type AttachmentRef struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	UID         uint32 `json:"uid"`
	PartID      string `json:"part_id"`
}

// GetAttachmentInput selects one part. Format is "auto" (default) or
// "original". MaxTextBytes bounds text extraction.
type GetAttachmentInput struct {
	AttachmentRef
	Format       string `json:"format,omitempty"`
	MaxTextBytes int    `json:"max_text_bytes,omitempty"`
}

// AttachmentResult is the bounded metadata of one fetched attachment. Data
// holds the bytes for an image or resource delivery and is never serialized.
type AttachmentResult struct {
	Mailbox       string    `json:"mailbox"`
	UIDValidity   uint32    `json:"uidValidity"`
	UID           uint32    `json:"uid"`
	PartID        string    `json:"partId"`
	Filename      string    `json:"filename,omitempty"`
	ContentType   string    `json:"contentType"`
	Size          int       `json:"size"`
	SHA256        string    `json:"sha256"`
	Delivery      string    `json:"delivery"`
	Text          string    `json:"text,omitempty"`
	TextTruncated bool      `json:"textTruncated,omitempty"`
	Width         int       `json:"width,omitempty"`
	Height        int       `json:"height,omitempty"`
	Warnings      []Warning `json:"warnings,omitempty"`
	Data          []byte    `json:"-"`
	DataMIME      string    `json:"-"`
}

// StageInput appends one base64 chunk to a staged file. An empty Handle
// starts a new file and requires Filename and ContentType.
type StageInput struct {
	Handle      string `json:"handle,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Data        string `json:"data"`
}

// StageResult reports the staged file state after one chunk.
type StageResult struct {
	Handle      string    `json:"handle"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	Size        int       `json:"size"`
	SHA256      string    `json:"sha256"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// DraftInput describes one draft. Staged lists handles from StageAttachment.
// Attachments lists parts that are already on the IMAP server.
type DraftInput struct {
	To          []string        `json:"to,omitempty"`
	Cc          []string        `json:"cc,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Body        string          `json:"body"`
	Staged      []string        `json:"staged,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

// DraftAttachment is one attachment part of a created draft.
type DraftAttachment struct {
	PartID      string `json:"partId"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
}

// DraftResult is the identity of the appended draft.
type DraftResult struct {
	Status         string            `json:"status"`
	Mailbox        string            `json:"mailbox"`
	UIDValidity    uint32            `json:"uidValidity,omitempty"`
	UID            uint32            `json:"uid,omitempty"`
	MessageID      string            `json:"messageId"`
	Attachments    []DraftAttachment `json:"attachments,omitempty"`
	Reconciliation string            `json:"reconciliation,omitempty"`
}

// AttachmentService is implemented by Client. Handlers receive it only when
// an attachment or draft capability is effective.
type AttachmentService interface {
	GetAttachment(ctx context.Context, input GetAttachmentInput) (AttachmentResult, error)
	StageAttachment(ctx context.Context, input StageInput) (StageResult, error)
	CreateDraft(ctx context.Context, input DraftInput) (DraftResult, error)
}

var _ AttachmentService = (*Client)(nil)

// attachmentSession is the optional IMAP surface for attachment content and
// draft APPEND. The production adapter implements it.
type attachmentSession interface {
	FetchAttachmentPart(uid uint32, path []int, maxWire int) (imapadapter.BodyData, error)
	AppendDraft(mailbox string, message []byte) (imapadapter.AppendData, error)
}

// outboundAttachment is one decoded attachment for a new message.
type outboundAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

func (a outboundAttachment) digest() string {
	sum := sha256.Sum256(a.Data)
	return hex.EncodeToString(sum[:])
}

func parsePartID(value string) ([]int, error) {
	if value == "" || len(value) > MaxPartIDLength {
		return nil, validationError("part_id must be a dotted MIME part number such as 2 or 1.2")
	}
	fields := strings.Split(value, ".")
	if len(fields) > MaxMIMEDepth {
		return nil, validationError("part_id is nested too deeply")
	}
	path := make([]int, len(fields))
	for i, field := range fields {
		if field == "" || len(field) > 4 || field[0] == '0' || strings.Trim(field, "0123456789") != "" {
			return nil, validationError("part_id must be a dotted MIME part number such as 2 or 1.2")
		}
		number, err := strconv.Atoi(field)
		if err != nil || number < 1 || number > MaxMIMEParts {
			return nil, validationError("part_id must be a dotted MIME part number such as 2 or 1.2")
		}
		path[i] = number
	}
	return path, nil
}

func validateAttachmentRef(ref AttachmentRef) ([]int, error) {
	if err := validateIdentity(ref.Mailbox, ref.UIDValidity, ref.UID); err != nil {
		return nil, err
	}
	return parsePartID(ref.PartID)
}

func samePath(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchedPart is one decoded part with its BODYSTRUCTURE metadata.
type fetchedPart struct {
	Filename    string
	ContentType string
	Charset     string
	Data        []byte
}

// loadPart reads one leaf part from the selected mailbox. The caller has
// already selected the mailbox and checked UIDVALIDITY.
func loadPart(session imapSession, uid uint32, path []int, maxWire int) (fetchedPart, error) {
	fetcher, ok := session.(attachmentSession)
	if !ok {
		return fetchedPart{}, newError(CodeInternalError, "mail session cannot fetch attachment content")
	}
	metadata, err := session.FetchMetadata([]uint32{uid}, imapadapter.ProjectionFullMessage)
	if err != nil {
		return fetchedPart{}, err
	}
	if len(metadata) != 1 || metadata[0].UID != uid {
		return fetchedPart{}, &imapadapter.Error{Kind: imapadapter.ErrorNotFound}
	}
	var found *imapadapter.BodyPart
	for i := range metadata[0].Parts {
		if samePath(metadata[0].Parts[i].Path, path) {
			found = &metadata[0].Parts[i]
			break
		}
	}
	if found == nil {
		return fetchedPart{}, newError(CodeNotFound, "part_id does not name a leaf MIME part of this message")
	}
	if strings.HasPrefix(found.ContentType, "multipart/") || found.ContentType == "message/rfc822" {
		return fetchedPart{}, validationError("part_id names a container part, not a file")
	}
	// A size that BODYSTRUCTURE already reports as too large needs no fetch.
	if int64(found.Size) > int64(maxWire) {
		return fetchedPart{}, partTooLarge(maxWire)
	}
	data, err := fetcher.FetchAttachmentPart(uid, path, maxWire)
	if err != nil {
		return fetchedPart{}, err
	}
	if data.TooLarge {
		return fetchedPart{}, partTooLarge(maxWire)
	}
	decoded, charset, err := decodeTransferEncoding(data.MIMEHeader, data.Body, MaxAttachmentWireBytes)
	if err != nil {
		return fetchedPart{}, err
	}
	return fetchedPart{Filename: found.Filename, ContentType: strings.ToLower(found.ContentType), Charset: charset, Data: decoded}, nil
}

// partTooLarge names the limit that a part exceeds. A budget below the wire
// limit is what remains of the total size limit for one request.
func partTooLarge(maxWire int) error {
	if maxWire < MaxAttachmentWireBytes {
		return newError(CodePayloadTooLarge, "attachments exceed the 2 MiB total size limit")
	}
	return newError(CodePayloadTooLarge, "attachment exceeds the 3 MiB encoded size limit")
}

// decodeTransferEncoding removes the Content-Transfer-Encoding only. It never
// converts the charset, so the returned bytes match the original file.
func decodeTransferEncoding(header, body []byte, limit int) ([]byte, string, error) {
	if len(header) > MaxHeaderBytes || len(body) > limit {
		return nil, "", newError(CodePayloadTooLarge, "attachment exceeds its decode limit")
	}
	headerText := append(append([]byte(nil), header...), '\r', '\n', '\r', '\n')
	mimeHeader, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(headerText))).ReadMIMEHeader()
	if err != nil && len(mimeHeader) == 0 {
		return nil, "", newError(CodeProtocolError, "attachment MIME header could not be parsed")
	}
	charset := ""
	if _, params, ok := parseContentType(mimeHeader.Get("Content-Type")); ok {
		charset = params["charset"]
	}
	encoding := strings.ToLower(strings.TrimSpace(mimeHeader.Get("Content-Transfer-Encoding")))
	switch encoding {
	case "", "7bit", "8bit", "binary":
		return append([]byte(nil), body...), charset, nil
	case "base64":
		cleaned := make([]byte, 0, len(body))
		for _, b := range body {
			switch b {
			case '\r', '\n', ' ', '\t':
				continue
			}
			cleaned = append(cleaned, b)
		}
		trimmed := bytes.TrimRight(cleaned, "=")
		decoded := make([]byte, base64.RawStdEncoding.DecodedLen(len(trimmed)))
		n, err := base64.RawStdEncoding.Decode(decoded, trimmed)
		if err != nil {
			return nil, "", newError(CodeProtocolError, "attachment base64 data is malformed")
		}
		return decoded[:n], charset, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(io.LimitReader(quotedprintable.NewReader(bytes.NewReader(body)), int64(limit)+1))
		if err != nil {
			return nil, "", newError(CodeProtocolError, "attachment quoted-printable data is malformed")
		}
		if len(decoded) > limit {
			return nil, "", newError(CodePayloadTooLarge, "attachment exceeds its decode limit")
		}
		return decoded, charset, nil
	default:
		return nil, "", newError(CodeProtocolError, "attachment uses an unsupported transfer encoding")
	}
}

// GetAttachment reads one part with BODY.PEEK and renders it for the model.
// It keeps no copy after the call.
func (s *Client) GetAttachment(ctx context.Context, input GetAttachmentInput) (AttachmentResult, error) {
	if !s.config.EnableAttachmentRead {
		return AttachmentResult{}, newError(CodeAuthorization, "mail attachment reads are disabled")
	}
	path, err := validateAttachmentRef(input.AttachmentRef)
	if err != nil {
		return AttachmentResult{}, err
	}
	format := input.Format
	if format == "" {
		format = "auto"
	}
	if format != "auto" && format != "original" {
		return AttachmentResult{}, validationError("format must be auto or original")
	}
	maxText := input.MaxTextBytes
	if maxText == 0 {
		maxText = DefaultAttachmentTextBytes
	}
	if maxText < 1 || maxText > MaxAttachmentTextBytes {
		return AttachmentResult{}, validationError("max_text_bytes must be between 1 and 204800")
	}
	value, err := s.read(ctx, func(session imapSession) (any, error) {
		selected, err := session.Select(input.Mailbox, true)
		if err != nil {
			return nil, err
		}
		if selected.UIDValidity != input.UIDValidity {
			return nil, newError(CodeConcurrentModification, "mailbox UIDVALIDITY changed; search for the message again")
		}
		return loadPart(session, input.UID, path, MaxAttachmentWireBytes)
	})
	if err != nil {
		return AttachmentResult{}, err
	}
	part := value.(fetchedPart)
	sum := sha256.Sum256(part.Data)
	result := AttachmentResult{
		Mailbox: input.Mailbox, UIDValidity: input.UIDValidity, UID: input.UID, PartID: input.PartID,
		Filename: part.Filename, ContentType: effectiveContentType(part.ContentType, part.Filename),
		Size: len(part.Data), SHA256: hex.EncodeToString(sum[:]),
	}
	if err := renderAttachment(ctx, &result, part, format, maxText); err != nil {
		return AttachmentResult{}, err
	}
	return result, nil
}

// fetchOutbound resolves attachment references before any SMTP connection or
// APPEND. It uses one read session per mailbox and bounds the total size.
func (s *Client) fetchOutbound(ctx context.Context, refs []AttachmentRef) ([]outboundAttachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > MaxOutboundAttachments {
		return nil, validationError("at most 10 attachments are allowed")
	}
	type item struct {
		index int
		ref   AttachmentRef
		path  []int
	}
	byMailbox := make(map[string][]item)
	var order []string
	seen := make(map[string]struct{})
	for index, ref := range refs {
		path, err := validateAttachmentRef(ref)
		if err != nil {
			return nil, err
		}
		key := ref.Mailbox + "\x00" + strconv.FormatUint(uint64(ref.UIDValidity), 10) + "\x00" + strconv.FormatUint(uint64(ref.UID), 10) + "\x00" + ref.PartID
		if _, duplicate := seen[key]; duplicate {
			return nil, validationError("attachments contain a duplicate part reference")
		}
		seen[key] = struct{}{}
		if _, ok := byMailbox[ref.Mailbox]; !ok {
			order = append(order, ref.Mailbox)
		}
		byMailbox[ref.Mailbox] = append(byMailbox[ref.Mailbox], item{index: index, ref: ref, path: path})
	}
	out := make([]outboundAttachment, len(refs))
	total := 0
	for _, mailbox := range order {
		items := byMailbox[mailbox]
		value, err := s.read(ctx, func(session imapSession) (any, error) {
			selected, err := session.Select(mailbox, true)
			if err != nil {
				return nil, err
			}
			parts := make([]fetchedPart, len(items))
			budget := MaxAttachmentWireBytes
			for i, entry := range items {
				if selected.UIDValidity != entry.ref.UIDValidity {
					return nil, newError(CodeConcurrentModification, "attachment mailbox UIDVALIDITY changed; search for the message again")
				}
				if budget <= 0 {
					return nil, newError(CodePayloadTooLarge, "attachments exceed the 2 MiB total size limit")
				}
				part, err := loadPart(session, entry.ref.UID, entry.path, budget)
				if err != nil {
					return nil, err
				}
				budget -= len(part.Data)
				parts[i] = part
			}
			return parts, nil
		})
		if err != nil {
			return nil, err
		}
		for i, part := range value.([]fetchedPart) {
			total += len(part.Data)
			if total > MaxOutboundAttachmentBytes {
				return nil, newError(CodePayloadTooLarge, "attachments exceed the 2 MiB total size limit")
			}
			out[items[i].index] = outboundAttachment{
				Filename:    outboundFilename(part.Filename),
				ContentType: outboundContentType(part.ContentType, part.Filename),
				Data:        part.Data,
			}
		}
	}
	return out, nil
}

// outboundContentType keeps a received type only when it is a plain
// type/subtype that a staged file could also use. Any other value becomes
// application/octet-stream before the extension table applies.
func outboundContentType(contentType, filename string) string {
	if validateStagedContentType(contentType) != nil {
		contentType = "application/octet-stream"
	}
	return effectiveContentType(contentType, filename)
}

// outboundFilename keeps a received name when it is safe and replaces
// separators and controls otherwise.
func outboundFilename(name string) string {
	name = strings.ToValidUTF8(name, "_")
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r < 0x20 || r == 0x7f:
			builder.WriteRune('_')
		default:
			builder.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(builder.String())
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "attachment"
	}
	if len(cleaned) > MaxAttachmentFilename {
		cleaned = truncateUTF8(cleaned, MaxAttachmentFilename)
	}
	return cleaned
}

func validateStagedFilename(name string) error {
	if name == "" || len(name) > MaxAttachmentFilename || !utf8.ValidString(name) || name == "." || name == ".." {
		return validationError("filename must be a non-empty UTF-8 name of at most 255 bytes")
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return validationError("filename must not contain path separators or control characters")
		}
	}
	if strings.TrimSpace(name) != name {
		return validationError("filename must not start or end with white space")
	}
	return nil
}

func validateStagedContentType(value string) error {
	if len(value) > MaxAttachmentContentType {
		return validationError("content_type is too long")
	}
	mediaType, params, ok := parseContentType(value)
	if !ok || len(params) > 0 || mediaType != strings.ToLower(value) || strings.HasPrefix(mediaType, "multipart/") || strings.HasPrefix(mediaType, "message/") {
		return validationError("content_type must be a plain type/subtype such as application/pdf")
	}
	return nil
}

// stagingStore keeps unfinished outbound files in memory only. Nothing is
// written to disk, and expired or consumed files are dropped.
type stagingStore struct {
	mu    sync.Mutex
	files map[string]*stagedFile
	total int
}

type stagedFile struct {
	filename    string
	contentType string
	data        []byte
	expires     time.Time
}

func newStagingStore() *stagingStore {
	return &stagingStore{files: make(map[string]*stagedFile)}
}

func (st *stagingStore) expireLocked(now time.Time) {
	for handle, file := range st.files {
		if !now.Before(file.expires) {
			st.total -= len(file.data)
			delete(st.files, handle)
		}
	}
}

// StageAttachment appends one base64 chunk to an in-memory staged file.
func (s *Client) StageAttachment(_ context.Context, input StageInput) (StageResult, error) {
	if !s.config.EnableDrafts {
		return StageResult{}, newError(CodeAuthorization, "mail drafts are disabled")
	}
	if input.Data == "" || len(input.Data) > MaxStageChunkBase64 {
		return StageResult{}, validationError("data must be a non-empty base64 chunk of at most 524288 characters")
	}
	chunk, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		return StageResult{}, validationError("data must be standard padded base64")
	}
	st := s.staging
	st.mu.Lock()
	defer st.mu.Unlock()
	now := s.now()
	st.expireLocked(now)
	var handle string
	var file *stagedFile
	if input.Handle == "" {
		if err := validateStagedFilename(input.Filename); err != nil {
			return StageResult{}, err
		}
		if err := validateStagedContentType(input.ContentType); err != nil {
			return StageResult{}, err
		}
		if len(st.files) >= MaxStagedFiles {
			return StageResult{}, newRetryableError(CodeRateLimited, "too many staged files; create a draft or wait for expiry")
		}
		handle, err = newStageHandle()
		if err != nil {
			return StageResult{}, newError(CodeInternalError, "could not create a staging handle")
		}
		file = &stagedFile{filename: input.Filename, contentType: strings.ToLower(input.ContentType)}
	} else {
		handle = input.Handle
		file = st.files[handle]
		if file == nil {
			return StageResult{}, newError(CodeNotFound, "staging handle is unknown or expired")
		}
		if input.Filename != "" && input.Filename != file.filename || input.ContentType != "" && !strings.EqualFold(input.ContentType, file.contentType) {
			return StageResult{}, validationError("a later chunk must not change filename or content_type")
		}
	}
	if len(file.data)+len(chunk) > MaxOutboundAttachmentBytes {
		return StageResult{}, newError(CodePayloadTooLarge, "staged file exceeds the 2 MiB size limit")
	}
	if st.total+len(chunk) > MaxStagedTotalBytes {
		return StageResult{}, newRetryableError(CodeRateLimited, "staging memory is full; create a draft or wait for expiry")
	}
	file.data = append(file.data, chunk...)
	file.expires = now.Add(StagedAttachmentTTL)
	st.total += len(chunk)
	st.files[handle] = file
	sum := sha256.Sum256(file.data)
	return StageResult{
		Handle: handle, Filename: file.filename, ContentType: file.contentType,
		Size: len(file.data), SHA256: hex.EncodeToString(sum[:]), ExpiresAt: file.expires.UTC(),
	}, nil
}

func newStageHandle() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	return "stg_" + hex.EncodeToString(buffer[:]), nil
}

// takeStaged copies the named staged files without removing them. Removal
// happens only after a definitive APPEND.
func (st *stagingStore) snapshot(handles []string, now time.Time) ([]outboundAttachment, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expireLocked(now)
	seen := make(map[string]struct{}, len(handles))
	out := make([]outboundAttachment, 0, len(handles))
	for _, handle := range handles {
		if _, duplicate := seen[handle]; duplicate {
			return nil, validationError("staged contains a duplicate handle")
		}
		seen[handle] = struct{}{}
		file := st.files[handle]
		if file == nil {
			return nil, newError(CodeNotFound, "staging handle is unknown or expired")
		}
		out = append(out, outboundAttachment{Filename: file.filename, ContentType: file.contentType, Data: append([]byte(nil), file.data...)})
	}
	return out, nil
}

func (st *stagingStore) release(handles []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, handle := range handles {
		if file := st.files[handle]; file != nil {
			st.total -= len(file.data)
			delete(st.files, handle)
		}
	}
}

// resolveDraftsDestination finds the Drafts mailbox. A SPECIAL-USE \Drafts
// mailbox wins. Without one, only the operator-configured name is used, and
// the server never infers the purpose of a mailbox from its display name.
func (s *Client) resolveDraftsDestination(session imapSession) (string, error) {
	mailboxes, err := session.List()
	if err != nil {
		return "", err
	}
	var drafts []string
	var configured *imapadapter.Mailbox
	for i, mailbox := range mailboxes {
		if !isSelectable(mailbox) {
			continue
		}
		if s.config.DraftsMailbox != "" && mailbox.Name == s.config.DraftsMailbox {
			configured = &mailboxes[i]
		}
		for _, attribute := range mailbox.Attributes {
			if strings.EqualFold(attribute, "\\Drafts") {
				drafts = append(drafts, mailbox.Name)
				break
			}
		}
	}
	switch {
	case len(drafts) > 1:
		return "", newError(CodeProtocolError, "more than one SPECIAL-USE Drafts mailbox exists")
	case len(drafts) == 1:
		if s.config.DraftsMailbox != "" && drafts[0] != s.config.DraftsMailbox {
			return "", newError(CodeConflict, "the configured Drafts mailbox differs from the SPECIAL-USE Drafts mailbox")
		}
	case s.config.DraftsMailbox == "":
		return "", newError(CodeProtocolError, "the server marks no SPECIAL-USE Drafts mailbox; set ICLOUD_MCP_DRAFTS_MAILBOX")
	case configured == nil:
		return "", newError(CodeNotFound, "the configured Drafts mailbox does not exist or is not selectable")
	default:
		if strings.EqualFold(configured.Name, "INBOX") {
			return "", newError(CodeAuthorization, "INBOX cannot hold drafts")
		}
		for _, attribute := range configured.Attributes {
			switch strings.ToLower(attribute) {
			case "\\sent", "\\trash", "\\junk", "\\all", "\\flagged", "\\archive":
				return "", newError(CodeAuthorization, "the configured Drafts mailbox has another SPECIAL-USE role")
			}
		}
		drafts = []string{configured.Name}
	}
	if err := validateMailbox(drafts[0]); err != nil {
		return "", newError(CodeProtocolError, "Drafts mailbox name is invalid")
	}
	if len(s.config.DraftsScope) > 0 && !scopeAllows(s.config.DraftsScope, drafts[0]) {
		return "", newError(CodeAuthorization, "Drafts mailbox is outside the configured resource scope")
	}
	return drafts[0], nil
}

// CreateDraft appends one draft with attachments to the Drafts mailbox. It
// never contacts SMTP. An ambiguous APPEND reports outcome_unknown.
func (s *Client) CreateDraft(ctx context.Context, input DraftInput) (DraftResult, error) {
	if !s.config.EnableDrafts {
		return DraftResult{}, newError(CodeAuthorization, "mail drafts are disabled")
	}
	if len(input.Staged)+len(input.Attachments) > MaxOutboundAttachments {
		return DraftResult{}, validationError("at most 10 attachments are allowed")
	}
	staged, err := s.staging.snapshot(input.Staged, s.now())
	if err != nil {
		return DraftResult{}, err
	}
	referenced, err := s.fetchOutbound(ctx, input.Attachments)
	if err != nil {
		return DraftResult{}, err
	}
	attachments := append(staged, referenced...)
	total := 0
	for _, attachment := range attachments {
		total += len(attachment.Data)
	}
	if total > MaxOutboundAttachmentBytes {
		return DraftResult{}, newError(CodePayloadTooLarge, "attachments exceed the 2 MiB total size limit")
	}
	prepared, err := s.composeMessage(composeInput{
		To: input.To, Cc: input.Cc, Subject: input.Subject, Body: input.Body, Attachments: attachments,
	}, false)
	if err != nil {
		return DraftResult{}, err
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if !s.mutationLimiter.Allow() {
		return DraftResult{}, newRetryableError(CodeRateLimited, "local mail mutation rate limit reached")
	}
	if err := acquire(ctx, s.mutationSem); err != nil {
		return DraftResult{}, err
	}
	defer func() { <-s.mutationSem }()
	phase := &imapMutationPhase{}
	destination := ""
	value, err := s.imapAttempt(ctx, phase, func(session imapSession) (any, error) {
		appender, ok := session.(attachmentSession)
		if !ok {
			return nil, newError(CodeInternalError, "mail session cannot append drafts")
		}
		name, err := s.resolveDraftsDestination(session)
		if err != nil {
			return nil, err
		}
		destination = name
		phase.dispatched = true
		return appender.AppendDraft(name, prepared.bytes)
	})
	result := DraftResult{Status: "rejected", Mailbox: destination, MessageID: prepared.messageID}
	if err != nil {
		mapped := mapIMAPError(err, ctx, phase.dispatched, "Check the Drafts mailbox for the Message-ID before creating the draft again.")
		if public := AsError(mapped); public != nil && public.Code == CodeOutcomeUnknown {
			result.Status = "outcome_unknown"
			result.Reconciliation = public.Reconciliation
		}
		return result, mapped
	}
	appended := value.(imapadapter.AppendData)
	s.staging.release(input.Staged)
	result.Status = "stored"
	result.UIDValidity, result.UID = appended.UIDValidity, appended.UID
	for i, attachment := range attachments {
		result.Attachments = append(result.Attachments, DraftAttachment{
			PartID: strconv.Itoa(i + 2), Filename: attachment.Filename, ContentType: attachment.ContentType,
			Size: len(attachment.Data), SHA256: attachment.digest(),
		})
	}
	return result, nil
}

// attachmentFingerprint lists references in a stable form for idempotency.
func attachmentFingerprint(refs []AttachmentRef) []string {
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = ref.Mailbox + "\x00" + strconv.FormatUint(uint64(ref.UIDValidity), 10) + "\x00" + strconv.FormatUint(uint64(ref.UID), 10) + "\x00" + ref.PartID
	}
	return out
}
