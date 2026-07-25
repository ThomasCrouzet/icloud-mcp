// Package mail implements bounded iCloud Mail access over IMAP and SMTP.
package mail

import (
	"context"
	"time"
)

const (
	MaxMailboxes       = 200
	MaxSearchResults   = 50
	MaxSearchValue     = 512
	MaxUIDsScanned     = 50_000
	DefaultBodyBytes   = 100 * 1024
	MaxBodyBytes       = 200 * 1024
	MaxWireBodyBytes   = 512 * 1024
	MaxHeaderBytes     = 64 * 1024
	MaxMIMEParts       = 200
	MaxMIMEDepth       = 20
	MaxAddresses       = 100
	MaxAttachments     = 100
	MaxMetadataString  = 4 * 1024
	MaxRecipients      = 50
	MaxSubjectBytes    = 998
	MaxSendBodyBytes   = 100 * 1024
	MaxEncodedMessage  = 256 * 1024
	MaxResultBytes     = 256 * 1024
	MaxIMAPSessionRead = 4 * 1024 * 1024
)

// Config is copied by NewService and is immutable for the service lifetime.
type Config struct {
	Address         string          `json:"-"`
	Password        string          `json:"-"`
	RecipientPolicy RecipientPolicy `json:"-"`
}

// Mailbox is one bounded LIST response item. Delimiter is empty when the
// server returned NIL.
type Mailbox struct {
	Name       string   `json:"name"`
	Delimiter  string   `json:"delimiter,omitempty"`
	Attributes []string `json:"attributes,omitempty"`
}

type ListMailboxesResult struct {
	Mailboxes []Mailbox `json:"mailboxes"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Address is a modeled envelope address. Group markers are never returned.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// MessageFlag is one exposed system flag.
type MessageFlag string

const (
	FlagSeen     MessageFlag = "seen"
	FlagFlagged  MessageFlag = "flagged"
	FlagAnswered MessageFlag = "answered"
)

type MessageSummary struct {
	Mailbox      string        `json:"mailbox"`
	UIDValidity  uint32        `json:"uidValidity"`
	UID          uint32        `json:"uid"`
	Flags        []MessageFlag `json:"flags,omitempty"`
	From         []Address     `json:"from,omitempty"`
	To           []Address     `json:"to,omitempty"`
	Cc           []Address     `json:"cc,omitempty"`
	Subject      string        `json:"subject,omitempty"`
	InternalDate time.Time     `json:"internalDate"`
	HeaderDate   *time.Time    `json:"headerDate,omitempty"`
	Size         int64         `json:"size,omitempty"`
	MessageID    string        `json:"messageId,omitempty"`
	ModSeq       uint64        `json:"modSeq,omitempty"`
}

type SearchInput struct {
	Mailbox     string    `json:"mailbox"`
	Query       string    `json:"query,omitempty"`
	From        string    `json:"from,omitempty"`
	To          string    `json:"to,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Since       time.Time `json:"since,omitempty"`
	Before      time.Time `json:"before,omitempty"`
	UnseenOnly  bool      `json:"unseen_only,omitempty"`
	FlaggedOnly bool      `json:"flagged_only,omitempty"`
	BeforeUID   uint32    `json:"before_uid,omitempty"`
	UIDValidity uint32    `json:"uid_validity,omitempty"`
	Limit       int       `json:"limit,omitempty"`
}

type SearchResult struct {
	Messages         []MessageSummary `json:"messages"`
	UIDValidity      uint32           `json:"uidValidity"`
	NextBeforeUID    uint32           `json:"nextBeforeUid,omitempty"`
	ScanLimitReached bool             `json:"scanLimitReached,omitempty"`
	Truncated        bool             `json:"truncated,omitempty"`
}

type GetMessageInput struct {
	Mailbox      string `json:"mailbox"`
	UIDValidity  uint32 `json:"uid_validity"`
	UID          uint32 `json:"uid"`
	MaxBodyBytes int    `json:"max_body_bytes,omitempty"`
}

type Attachment struct {
	PartID      string `json:"partId"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"contentType"`
	Size        uint32 `json:"size,omitempty"`
	ContentID   string `json:"contentId,omitempty"`
}

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Message struct {
	MessageSummary
	ReplyTo               []Address    `json:"replyTo,omitempty"`
	InReplyTo             []string     `json:"inReplyTo,omitempty"`
	References            []string     `json:"references,omitempty"`
	Body                  string       `json:"body,omitempty"`
	BodyOmitted           bool         `json:"bodyOmitted,omitempty"`
	BodyUnavailableReason string       `json:"bodyUnavailableReason,omitempty"`
	Attachments           []Attachment `json:"attachments,omitempty"`
	Warnings              []Warning    `json:"warnings,omitempty"`
}

type FlagOperation string

const (
	FlagOperationAdd    FlagOperation = "add"
	FlagOperationRemove FlagOperation = "remove"
)

type SetFlagsInput struct {
	Mailbox        string        `json:"mailbox"`
	UIDValidity    uint32        `json:"uid_validity"`
	UID            uint32        `json:"uid"`
	Operation      FlagOperation `json:"operation"`
	Flags          []MessageFlag `json:"flags"`
	ExpectedModSeq uint64        `json:"expected_modseq,omitempty"`
}

type SetFlagsResult struct {
	Mailbox           string        `json:"mailbox"`
	UIDValidity       uint32        `json:"uidValidity"`
	UID               uint32        `json:"uid"`
	Flags             []MessageFlag `json:"flags,omitempty"`
	ModSeq            uint64        `json:"modSeq,omitempty"`
	ConditionalUpdate bool          `json:"conditionalUpdate"`
	ResultIncomplete  bool          `json:"resultIncomplete,omitempty"`
}

type MoveInput struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	UID         uint32 `json:"uid"`
	Destination string `json:"destination"`
}

type TrashInput struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	UID         uint32 `json:"uid"`
}

type MoveResult struct {
	Mailbox                string `json:"mailbox"`
	UIDValidity            uint32 `json:"uidValidity"`
	UID                    uint32 `json:"uid"`
	Destination            string `json:"destination"`
	DestinationUIDValidity uint32 `json:"destinationUidValidity,omitempty"`
	DestinationUID         uint32 `json:"destinationUid,omitempty"`
	Method                 string `json:"method"`
}

type SendInput struct {
	To      []string `json:"to"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Body    string   `json:"body"`
}

type SendStatus string

const (
	SendAccepted       SendStatus = "accepted"
	SendRejected       SendStatus = "rejected"
	SendOutcomeUnknown SendStatus = "outcome_unknown"
)

type RecipientStatus struct {
	Index    int    `json:"index"`
	Accepted bool   `json:"accepted"`
	Category string `json:"category,omitempty"`
}

type SendResult struct {
	Status     SendStatus        `json:"status"`
	MessageID  string            `json:"messageId"`
	Recipients []RecipientStatus `json:"recipients,omitempty"`
	// SentCopyUnavailable is true because SMTP submission does not APPEND a
	// copy to Sent and no server-side Sent-copy behavior is assumed.
	SentCopyUnavailable bool   `json:"sentCopyUnavailable,omitempty"`
	Reconciliation      string `json:"reconciliation,omitempty"`
}

// Service is the Mail API consumed by MCP handlers.
type Service interface {
	ListMailboxes(ctx context.Context) (ListMailboxesResult, error)
	SearchMessages(ctx context.Context, input SearchInput) (SearchResult, error)
	GetMessage(ctx context.Context, input GetMessageInput) (Message, error)
	SetMessageFlags(ctx context.Context, input SetFlagsInput) (SetFlagsResult, error)
	MoveMessage(ctx context.Context, input MoveInput) (MoveResult, error)
	TrashMessage(ctx context.Context, input TrashInput) (MoveResult, error)
	SendMessage(ctx context.Context, input SendInput) (SendResult, error)
}
