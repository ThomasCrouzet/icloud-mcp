// Package imapadapter isolates the beta go-imap API from the Mail service.
package imapadapter

import "time"

type ErrorKind uint8

const (
	ErrorProtocol ErrorKind = iota
	ErrorAuthentication
	ErrorAuthorization
	ErrorNotFound
	ErrorConflict
	ErrorRateLimited
	ErrorTimeout
	ErrorUnavailable
	ErrorTooLarge
)

// Error intentionally omits the underlying protocol text.
type Error struct {
	Kind      ErrorKind
	Ambiguous bool
}

func (e *Error) Error() string { return "sanitized IMAP failure" }

type Capabilities struct {
	CondStore  bool
	Move       bool
	UIDPlus    bool
	Binary     bool
	SpecialUse bool
}

type Mailbox struct {
	Name       string
	Delimiter  rune
	Attributes []string
}

type SelectedMailbox struct {
	UIDValidity uint32
	UIDNext     uint32
}

type SearchRequest struct {
	UIDMin      uint32
	UIDMax      uint32
	Query       string
	From        string
	To          string
	Subject     string
	Since       time.Time
	Before      time.Time
	UnseenOnly  bool
	FlaggedOnly bool
}

type Address struct {
	Name    string
	Address string
}

type Envelope struct {
	Date      time.Time
	Subject   string
	From      []Address
	ReplyTo   []Address
	To        []Address
	Cc        []Address
	MessageID string
	InReplyTo []string
}

type BodyPart struct {
	Path        []int
	ContentType string
	Disposition string
	Filename    string
	ContentID   string
	Size        uint32
	InlinePlain bool
	Attachment  bool
	// AlternativeGroup identifies the nearest multipart/alternative ancestor.
	// Parts in one group are equivalent representations of the same content.
	AlternativeGroup string
	// AlternativeBranch identifies the direct child branch of that ancestor
	// which contains this part. Parts in one group and one branch are
	// independent segments. Parts in one group and different branches compete.
	AlternativeBranch string
}

// Projection names the FETCH data items one read needs. A reader asks for the
// smallest projection it can use, so a MIME tree never blocks a metadata-only
// read.
type Projection uint8

const (
	// ProjectionSummary gets envelope, flag, date, size, and MODSEQ metadata.
	ProjectionSummary Projection = iota + 1
	// ProjectionReplyHeaders adds the four curated threading headers.
	ProjectionReplyHeaders
	// ProjectionFullMessage adds the MIME tree and the curated headers.
	ProjectionFullMessage
)

func (p Projection) valid() bool {
	return p >= ProjectionSummary && p <= ProjectionFullMessage
}

func (p Projection) wantsHeaders() bool {
	return p == ProjectionReplyHeaders || p == ProjectionFullMessage
}

func (p Projection) wantsBodyStructure() bool { return p == ProjectionFullMessage }

func (p Projection) String() string {
	switch p {
	case ProjectionSummary:
		return "summary"
	case ProjectionReplyHeaders:
		return "reply-headers"
	case ProjectionFullMessage:
		return "full-message"
	default:
		return "invalid"
	}
}

type CuratedHeaders struct {
	MessageID  string
	InReplyTo  []string
	References []string
	ReplyTo    []Address
}

type Message struct {
	UID          uint32
	Flags        []string
	Envelope     Envelope
	InternalDate time.Time
	Size         int64
	ModSeq       uint64
	Parts        []BodyPart
	Headers      CuratedHeaders
}

type BodyData struct {
	MIMEHeader []byte
	Body       []byte
	TooLarge   bool
}

type CopyData struct {
	UIDValidity    uint32
	DestinationUID uint32
}

type AppendData struct {
	UIDValidity uint32
	UID         uint32
}
