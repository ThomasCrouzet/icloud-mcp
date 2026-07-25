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
