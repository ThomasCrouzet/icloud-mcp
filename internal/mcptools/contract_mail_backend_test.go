package mcptools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

var contractSMTPConfig *tls.Config

// Only this Go test child trusts the synthetic root. The production SMTP client
// still verifies smtp.mail.me.com, STARTTLS and TLS 1.2 or newer.
func TestMain(m *testing.M) {
	if err := contractSMTPTrust(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func contractSMTPTrust() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Contract fixture root"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"smtp.mail.me.com"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		return err
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	if err := os.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1"); err != nil {
		return err
	}
	x509.SetFallbackRoots(roots)
	contractSMTPConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: key}}}
	return nil
}

type contractMailBackend struct {
	mu                                                      sync.Mutex
	imapDials, smtpDials, stores, moves, appends, data, tls int
	commands                                                []string
	payloads, archived                                      [][]byte
	recipients                                              []string
	flags                                                   string
	policy                                                  string
	source, destination                                     string
	validity                                                uint64
	htmlOnly                                                bool
	body                                                    string
	messageCount                                            int
	rejectIMAP, rejectSMTPAuth                              bool
	condstore, malformedStore                               bool
	modseq                                                  uint64
	errors                                                  []string
}

func (b *contractMailBackend) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"imap": b.imapDials, "smtp": b.smtpDials, "stores": b.stores, "moves": b.moves, "appends": b.appends, "data": b.data, "tls": b.tls, "commands": append([]string(nil), b.commands...), "errors": append([]string(nil), b.errors...)}
}

func (b *contractMailBackend) service(t *testing.T) *maildomain.Client {
	t.Helper()
	if b.modseq == 0 {
		b.modseq = 42
	}
	allowed := b.policy
	if allowed == "" {
		allowed = "reply@example.invalid,recipient@example.invalid,copy@example.invalid"
	}
	policy, err := maildomain.ParseRecipientPolicy(allowed)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := maildomain.NewService(maildomain.Config{Address: "fixture@icloud.com", Password: "synthetic-contract-secret", RecipientPolicy: policy, EnableSentCopy: true, ArchiveScope: []string{"Sent"}},
		func(context.Context) (net.Conn, error) {
			b.mu.Lock()
			b.imapDials++
			b.mu.Unlock()
			local, remote := net.Pipe()
			go func() { b.finish(b.serveIMAP(remote)) }()
			return local, nil
		},
		func(context.Context) (net.Conn, error) {
			b.mu.Lock()
			b.smtpDials++
			b.mu.Unlock()
			local, remote := net.Pipe()
			go func() { b.finish(b.serveSMTP(remote)) }()
			return local, nil
		}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (b *contractMailBackend) finish(err error) {
	if err != nil {
		b.mu.Lock()
		b.errors = append(b.errors, err.Error())
		b.mu.Unlock()
	}
}

func (b *contractMailBackend) serveSMTP(conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	wire := conn
	reader := bufio.NewReader(wire)
	write := func(s string) error { _, err := io.WriteString(wire, s); return err }
	if err := write("220 fixture ESMTP\r\n"); err != nil {
		return err
	}
	secure := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return fmt.Errorf("empty SMTP command")
		}
		response := "250 accepted\r\n"
		switch fields[0] {
		case "EHLO":
			response = "250-fixture\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n"
		case "STARTTLS":
			if secure {
				return fmt.Errorf("duplicate STARTTLS")
			}
			if err := write("220 begin TLS\r\n"); err != nil {
				return err
			}
			tlsConn := tls.Server(conn, contractSMTPConfig)
			if err := tlsConn.Handshake(); err != nil {
				return err
			}
			if tlsConn.ConnectionState().Version < tls.VersionTLS12 {
				return fmt.Errorf("unsafe TLS version")
			}
			wire = tlsConn
			reader = bufio.NewReader(wire)
			secure = true
			b.mu.Lock()
			b.tls++
			b.mu.Unlock()
			continue
		case "AUTH":
			if !secure {
				return fmt.Errorf("AUTH before TLS")
			}
			response = "235 authenticated\r\n"
			if b.rejectSMTPAuth {
				response = "535 authentication rejected\r\n"
			}
		case "MAIL":
			if !secure {
				return fmt.Errorf("MAIL before TLS")
			}
		case "RCPT":
			b.mu.Lock()
			b.recipients = append(b.recipients, strings.TrimSpace(strings.TrimPrefix(line, "RCPT TO:")))
			b.mu.Unlock()
		case "DATA":
			if !secure {
				return fmt.Errorf("DATA before TLS")
			}
			if err := write("354 send data\r\n"); err != nil {
				return err
			}
			payload, err := io.ReadAll(textproto.NewReader(reader).DotReader())
			if err != nil {
				return err
			}
			b.mu.Lock()
			b.data++
			b.payloads = append(b.payloads, payload)
			b.mu.Unlock()
		case "QUIT":
			_ = write("221 closing\r\n")
			return nil
		case "RSET":
		case "*":
			if !b.rejectSMTPAuth {
				return fmt.Errorf("unexpected SMTP authentication cancellation")
			}
			response = "501 authentication cancelled\r\n"
		default:
			return fmt.Errorf("unexpected SMTP command %s", fields[0])
		}
		if err := write(response); err != nil {
			return err
		}
	}
}

func (b *contractMailBackend) serveIMAP(conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	write := func(s string) error { _, err := io.WriteString(conn, s); return err }
	if b.rejectIMAP {
		return write("* BYE [AUTHENTICATIONFAILED] rejected\r\n")
	}
	caps := "IMAP4rev1 MOVE UIDPLUS SPECIAL-USE"
	if b.condstore {
		caps += " CONDSTORE"
	}
	if err := write("* OK [CAPABILITY " + caps + "] fixture\r\n"); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return fmt.Errorf("empty IMAP command")
		}
		tag := f[0]
		if f[1] != "LOGIN" {
			b.mu.Lock()
			b.commands = append(b.commands, strings.TrimSpace(line))
			b.mu.Unlock()
		}
		response := tag + " OK done\r\n"
		uid := "2"
		if len(f) > 3 && f[1] == "UID" && (f[2] == "FETCH" || f[2] == "MOVE") {
			if n, err := strconv.ParseUint(f[3], 10, 32); err == nil {
				uid = strconv.FormatUint(n, 10)
			}
		}
		switch {
		case f[1] == "LOGIN":
			response = tag + " OK [CAPABILITY " + caps + "] authenticated\r\n"
		case f[1] == "CAPABILITY":
			response = "* CAPABILITY " + caps + "\r\n" + response
		case f[1] == "LIST":
			source, destination := b.source, b.destination
			if source == "" {
				source = "INBOX"
			}
			if destination == "" {
				destination = "Archive"
			}
			response = "* LIST () \"/\" \"" + source + "\"\r\n* LIST () \"/\" \"" + destination + "\"\r\n* LIST (\\Trash) \"/\" \"Trash\"\r\n* LIST (\\Sent) \"/\" \"Sent\"\r\n" + response
		case f[1] == "SELECT" || f[1] == "EXAMINE":
			validity := b.validity
			if validity == 0 {
				validity = 9
			}
			count := b.messageCount
			if count == 0 {
				count = 1
			}
			response = fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY %d] valid\r\n* OK [UIDNEXT %d] next\r\n", count, validity, count+2) + tag + " OK [READ-" + map[bool]string{true: "ONLY", false: "WRITE"}[f[1] == "EXAMINE"] + "] selected\r\n"
			if b.condstore {
				response = fmt.Sprintf("* OK [HIGHESTMODSEQ %d] highest\r\n", b.modseq) + response
			}
		case strings.Contains(line, "UID SEARCH"):
			count := b.messageCount
			if count == 0 {
				count = 1
			}
			var found strings.Builder
			window := map[string]bool{}
			for i := 3; i+1 < len(f); i++ {
				if f[i] == "UID" {
					for _, n := range contractFixtureUIDs(f[i+1]) {
						window[n] = true
					}
					break
				}
			}
			for n := 2; n < count+2; n++ {
				if len(window) > 0 && !window[strconv.Itoa(n)] {
					continue
				}
				fmt.Fprintf(&found, " %d", n)
			}
			response = "* SEARCH" + found.String() + "\r\n" + response
		case strings.Contains(line, "UID FETCH") && strings.Contains(line, "BODYSTRUCTURE"):
			headers := "Message-ID: <source@example.invalid>\r\nIn-Reply-To: <parent@example.invalid>\r\nReferences: <root@example.invalid>\r\nReply-To: reply@example.invalid\r\n\r\n"
			headerPart := ""
			if strings.Contains(line, "HEADER.FIELDS") {
				headerPart = fmt.Sprintf(" BODY[HEADER.FIELDS (MESSAGE-ID IN-REPLY-TO REFERENCES REPLY-TO)]<0> {%d}\r\n%s", len(headers), headers)
			}
			subtype := "PLAIN"
			body := b.body
			if body == "" {
				body = "source-body"
			}
			if b.htmlOnly {
				subtype = "HTML"
				body = "<p>source-body</p>"
			}
			var rows strings.Builder
			envelope := `(NIL "Synthetic" ((NIL NIL "reply" "example.invalid")) NIL NIL ((NIL NIL "fixture" "icloud.com")) ((NIL NIL "copy" "example.invalid")) NIL NIL "<source@example.invalid>")`
			if b.messageCount > 1 {
				envelope = `(NIL "Synthetic" ((NIL NIL "reply" "example.invalid")) NIL NIL NIL NIL NIL NIL "<source@example.invalid>")`
			}
			for _, id := range contractFixtureUIDs(f[3]) {
				fmt.Fprintf(&rows, "* 1 FETCH (UID %s FLAGS () INTERNALDATE \"01-Jul-2026 10:00:00 +0000\" RFC822.SIZE %d ENVELOPE %s BODYSTRUCTURE (\"TEXT\" \"%s\" (\"CHARSET\" \"UTF-8\") NIL NIL \"7BIT\" %d %d)%s)\r\n", id, len(body)+len(headers)+100, envelope, subtype, len(body), strings.Count(body, "\n")+1, headerPart)
			}
			response = rows.String() + response
		case strings.Contains(line, "UID FETCH") && strings.Contains(line, "BODY.PEEK[1.MIME]"):
			header := "Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 7bit\r\n\r\n"
			body := "source-body"
			if b.body != "" {
				body = b.body
			}
			if b.htmlOnly {
				header = strings.ReplaceAll(header, "text/plain", "text/html")
				body = "<p>source-body</p>"
			}
			response = fmt.Sprintf("* 1 FETCH (UID %s BODY[1.MIME]<0> {%d}\r\n%s BODY[1]<0> {%d}\r\n%s)\r\n%s", uid, len(header), header, len(body), body, response)
		case strings.Contains(line, "UID FETCH"):
			b.mu.Lock()
			flags := b.flags
			b.mu.Unlock()
			response = "* 1 FETCH (UID " + uid + " FLAGS (" + flags + "))\r\n" + response
			if b.condstore {
				response = fmt.Sprintf("* 1 FETCH (UID %s FLAGS (%s) MODSEQ (%d))\r\n", uid, flags, b.modseq) + tag + " OK fetched\r\n"
			}
		case strings.Contains(line, "UID STORE"):
			b.mu.Lock()
			b.stores++
			if strings.Contains(line, "+FLAGS") {
				b.flags = strings.TrimSpace(line[strings.LastIndex(line, "(")+1 : strings.LastIndex(line, ")")])
			} else {
				b.flags = ""
			}
			if b.condstore {
				if b.modseq < ^uint64(0) {
					b.modseq++
				}
				value := b.modseq
				if b.malformedStore {
					value = 0
				}
				response = fmt.Sprintf("* 1 FETCH (UID %s FLAGS (%s) MODSEQ (%d))\r\n", f[3], b.flags, value) + response
			}
			b.mu.Unlock()
		case strings.Contains(line, "UID MOVE"):
			b.mu.Lock()
			b.moves++
			b.mu.Unlock()
			response = tag + " OK [COPYUID 10 " + uid + " 4] moved\r\n"
		case f[1] == "APPEND":
			start := strings.LastIndex(line, "{")
			end := strings.LastIndex(line, "}")
			if start < 0 || end < start {
				return fmt.Errorf("APPEND lacks literal")
			}
			n, err := strconv.Atoi(strings.TrimSuffix(line[start+1:end], "+"))
			if err != nil {
				return err
			}
			if !strings.HasSuffix(line[start+1:end], "+") {
				if err := write("+ continue\r\n"); err != nil {
					return err
				}
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				return err
			}
			if _, err := r.ReadString('\n'); err != nil {
				return err
			}
			b.mu.Lock()
			b.appends++
			b.archived = append(b.archived, bytes.Clone(payload))
			b.mu.Unlock()
			response = tag + " OK [APPENDUID 10 4] appended\r\n"
		case f[1] == "LOGOUT":
			_ = write("* BYE closing\r\n" + response)
			return nil
		default:
			return fmt.Errorf("unexpected IMAP command %s", line)
		}
		if err := write(response); err != nil {
			return err
		}
	}
}

func contractFixtureUIDs(set string) []string {
	var out []string
	for _, part := range strings.Split(set, ",") {
		bounds := strings.Split(part, ":")
		first, err := strconv.ParseUint(bounds[0], 10, 32)
		if err != nil {
			panic("invalid fixture UID set")
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.ParseUint(bounds[1], 10, 32)
			if err != nil || last < first || last-first > 100 {
				panic("unbounded fixture UID set")
			}
		}
		for n := first; n <= last; n++ {
			out = append(out, strconv.FormatUint(n, 10))
		}
	}
	return out
}
