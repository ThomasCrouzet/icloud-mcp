package security_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type forgedIMAPDiagnostic struct{}

func (forgedIMAPDiagnostic) Error() string { return "tls eof private-diagnostic-marker" }
func (forgedIMAPDiagnostic) Diagnostic() (string, string) {
	return "tls", "private-diagnostic-marker"
}

func TestIssue77DiagnosticProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, stage, reason, code string
		attempts, logins, lists   int32
	}{
		{"dns", "connect", "dns", "unavailable", 2, 0, 0},
		{"dial", "connect", "connection_failed", "unavailable", 2, 0, 0},
		{"unknown", "connect", "unknown", "unavailable", 1, 0, 0},
		{"tls-eof", "tls", "eof", "unavailable", 2, 0, 0},
		{"untrusted", "tls", "certificate_verification", "unavailable", 1, 0, 0},
		{"hostname", "tls", "certificate_verification", "unavailable", 1, 0, 0},
		{"connect-deadline", "connect", "deadline", "timeout", 1, 0, 0},
		{"connect-canceled", "connect", "canceled", "timeout", 1, 0, 0},
		{"tls-deadline", "tls", "deadline", "timeout", 1, 0, 0},
		{"tls-canceled", "tls", "canceled", "timeout", 1, 0, 0},
		{"rejected", "authentication", "rejected", "authentication", 1, 2, 0},
		{"greeting-eof", "", "", "protocol_error", 1, 0, 0},
		{"login-eof", "", "", "unavailable", 2, 2, 0},
		{"list-authentication", "", "", "authentication", 1, 1, 1},
		{"success", "", "", "", 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certificate, roots := diagnosticCertificate(t, tc.name == "hostname")
			if tc.name == "untrusted" {
				roots = x509.NewCertPool()
			}
			var attempts, tlsOK, logins, lists, examines, smtp atomic.Int32
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if tc.name == "connect-deadline" || tc.name == "tls-deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			dial := security.NewIMAPFixtureDialer(func(ctx context.Context, network, address string) (net.Conn, error) {
				attempts.Add(1)
				if network != "tcp" || address != security.IMAPAddress {
					t.Error("fixed destination changed")
				}
				switch tc.name {
				case "dns":
					return nil, &net.DNSError{Err: "private-diagnostic-marker", Name: "private-account.invalid", Server: "192.0.2.9:53", IsNotFound: true}
				case "dial":
					return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 993}, Err: errors.New("private-diagnostic-marker")}
				case "unknown":
					return nil, forgedIMAPDiagnostic{}
				case "connect-deadline", "connect-canceled":
					if tc.name == "connect-canceled" {
						cancel()
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}
				local, remote := net.Pipe()
				_ = remote.SetDeadline(time.Now().Add(3 * time.Second))
				go func() {
					defer func() { _ = remote.Close() }()
					if tc.name == "tls-eof" || tc.name == "tls-deadline" || tc.name == "tls-canceled" {
						header := make([]byte, 5)
						if _, err := io.ReadFull(remote, header); err != nil {
							return
						}
						_, _ = io.CopyN(io.Discard, remote, int64(header[3])<<8|int64(header[4]))
						if tc.name == "tls-canceled" {
							cancel()
							<-ctx.Done()
						} else if tc.name == "tls-deadline" {
							<-ctx.Done()
						}
						return
					}
					server := tls.Server(remote, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
					if err := server.Handshake(); err != nil {
						return
					}
					tlsOK.Add(1)
					serveDiagnosticIMAP(server, tc.name, &logins, &lists, &examines)
				}()
				return local, nil
			}, &tls.Config{RootCAs: roots})
			policy, err := maildomain.ParseRecipientPolicy("to@example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			svc, err := maildomain.NewService(maildomain.Config{Address: "private-account@example.invalid", Password: "private-diagnostic-password", RecipientPolicy: policy},
				func(ctx context.Context) (net.Conn, error) { return dial(ctx, "tcp", security.IMAPAddress) },
				func(context.Context) (net.Conn, error) {
					smtp.Add(1)
					return nil, errors.New("SMTP forbidden in probe")
				}, false, true)
			if err != nil {
				t.Fatal(err)
			}
			_, probeErr := svc.Probe(ctx)
			var stage, reason, code string
			if public := maildomain.AsError(probeErr); public != nil {
				code = string(public.Code)
				if diagnostic, ok := any(public).(interface{ Diagnostic() (string, string) }); ok {
					stage, reason = diagnostic.Diagnostic()
				}
				encoded, _ := json.Marshal(public)
				for _, secret := range []string{"private-diagnostic", "private-account", "192.0.2.9", "CERTIFICATE"} {
					if strings.Contains(string(encoded), secret) || strings.Contains(public.Error(), secret) {
						t.Error("private failure text escaped sanitization")
					}
				}
			}
			if code != tc.code || stage != tc.stage || reason != tc.reason {
				t.Errorf("code/stage/reason=%q/%q/%q want %q/%q/%q", code, stage, reason, tc.code, tc.stage, tc.reason)
			}
			if tc.code == "" && probeErr != nil {
				t.Error("successful protocol probe failed")
			}
			if attempts.Load() != tc.attempts || logins.Load() != tc.logins || lists.Load() != tc.lists || smtp.Load() != 0 {
				t.Errorf("dial/login/list/smtp=%d/%d/%d/%d want %d/%d/%d/0", attempts.Load(), logins.Load(), lists.Load(), smtp.Load(), tc.attempts, tc.logins, tc.lists)
			}
			if (tc.stage == "tls" || tc.stage == "connect") && (tlsOK.Load() != 0 || logins.Load() != 0 || examines.Load() != 0) {
				t.Error("failed connection or TLS reached authenticated IMAP")
			}
			t.Logf("code=%s stage=%s reason=%s dial/tls/login/list/examine/smtp=%d/%d/%d/%d/%d/%d", code, stage, reason, attempts.Load(), tlsOK.Load(), logins.Load(), lists.Load(), examines.Load(), smtp.Load())
		})
	}
}

func serveDiagnosticIMAP(conn net.Conn, mode string, logins, lists, examines *atomic.Int32) {
	if mode == "greeting-eof" {
		return
	}
	write := func(s string) bool { _, err := io.WriteString(conn, s); return err == nil }
	if !write("* OK [CAPABILITY IMAP4rev1] fixture\r\n") {
		return
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return
		}
		tag := fields[0]
		var reply string
		switch fields[1] {
		case "LOGIN":
			logins.Add(1)
			if mode == "login-eof" {
				return
			}
			reply = tag + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
			if mode == "rejected" {
				reply = tag + " NO [AUTHENTICATIONFAILED] private-diagnostic-marker\r\n"
			}
		case "LIST":
			lists.Add(1)
			reply = "* LIST () \"/\" \"private-mailbox\"\r\n" + tag + " OK listed\r\n"
			if mode == "list-authentication" {
				reply = tag + " NO [AUTHENTICATIONFAILED] private-diagnostic-marker\r\n"
			}
		case "EXAMINE":
			examines.Add(1)
			reply = "* 0 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n* OK [UIDNEXT 1] next\r\n" + tag + " OK [READ-ONLY] examined\r\n"
		case "LOGOUT":
			write("* BYE fixture\r\n" + tag + " OK logout\r\n")
			return
		default:
			reply = fmt.Sprintf("%s BAD unexpected\r\n", tag)
		}
		if !write(reply) {
			return
		}
	}
}

func diagnosticCertificate(t *testing.T, wrongName bool) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	name := "imap.mail.me.com"
	if wrongName {
		name = "private-certificate.invalid"
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(77), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}
