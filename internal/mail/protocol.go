package mail

import (
	"context"
	"net"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

const smtpHost = "smtp.mail.me.com"

// IMAPDialFunc and SMTPDialFunc are injected dial functions. Production wires
// the fixed-destination implementations from the security package.
type IMAPDialFunc func(context.Context) (net.Conn, error)
type SMTPDialFunc func(context.Context) (net.Conn, error)

type imapSession interface {
	Capabilities() imapadapter.Capabilities
	SupportsModifiedDetection() bool
	List() ([]imapadapter.Mailbox, error)
	Select(mailbox string, readOnly bool) (imapadapter.SelectedMailbox, error)
	Search(request imapadapter.SearchRequest) ([]uint32, error)
	FetchMetadata(uids []uint32, includeHeaders bool) ([]imapadapter.Message, error)
	FetchBodyPart(uid uint32, path []int) (imapadapter.BodyData, error)
	StoreDelta(uid uint32, add bool, flags []string) error
	FetchFlags(uid uint32) (imapadapter.Message, error)
	NativeMove(uid uint32, destination string) (imapadapter.CopyData, error)
	Copy(uid uint32, destination string) (imapadapter.CopyData, error)
	AddDeleted(uid uint32) error
	UIDExpunge(uid uint32) error
	Close() error
}

type imapSessionFactory func(context.Context, net.Conn, string, string) (imapSession, error)

type smtpSession interface {
	Auth(address, password string) error
	Mail(from string, size int64, utf8 bool) error
	Rcpt(address string) error
	Reset() error
	Data(message []byte, phase *smtpDataPhase) error
	Close() error
}

type smtpSessionFactory func(context.Context, net.Conn) (smtpSession, error)

type smtpDataPhase struct {
	started bool
}
