package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestFixedDialersRejectBeforeInjectedDial(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wrap    func(DialContextFunc) DialContextFunc
		address string
	}{
		{name: "IMAP host", wrap: NewIMAPDialer, address: "evil.example:993"},
		{name: "IMAP port", wrap: NewIMAPDialer, address: "imap.mail.me.com:994"},
		{name: "SMTP host", wrap: NewSMTPDialer, address: "evil.example:587"},
		{name: "SMTP port", wrap: NewSMTPDialer, address: "smtp.mail.me.com:25"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			injected := func(context.Context, string, string) (net.Conn, error) {
				calls++
				return nil, nil
			}
			wrapped := tt.wrap(injected)
			if _, err := wrapped(context.Background(), "tcp", tt.address); err == nil {
				t.Fatal("expected fixed-address rejection")
			}
			if calls != 0 {
				t.Errorf("injected dialer called %d times before destination rejection", calls)
			}
		})
	}
}

func TestFixedDialersRejectNonTCPBeforeInjectedDial(t *testing.T) {
	for name, dialer := range map[string]struct {
		wrapped DialContextFunc
		address string
	}{
		"IMAP": {wrapped: NewIMAPDialer(func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("injected IMAP dialer called")
			return nil, nil
		}), address: IMAPAddress},
		"SMTP": {wrapped: NewSMTPDialer(func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("injected SMTP dialer called")
			return nil, nil
		}), address: SMTPAddress},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := dialer.wrapped(context.Background(), "udp", dialer.address); err == nil {
				t.Fatal("expected network rejection")
			}
		})
	}
}

func TestSMTPDialerInjectsOnlyFixedAddress(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	var gotNetwork, gotAddress string
	dialer := NewSMTPDialer(func(_ context.Context, network, address string) (net.Conn, error) {
		gotNetwork, gotAddress = network, address
		return client, nil
	})

	conn, err := dialer(context.Background(), "tcp", SMTPAddress)
	if err != nil {
		t.Fatalf("SMTP dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if gotNetwork != "tcp" || gotAddress != SMTPAddress {
		t.Errorf("injected dial target = %s %s", gotNetwork, gotAddress)
	}
}

func TestIMAPDialerUsesVerifiedTLSWithFixedServerName(t *testing.T) {
	certificate, roots := testIMAPCertificate(t)
	client, server := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		tlsServer := tls.Server(server, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		})
		serverErr <- tlsServer.Handshake()
		_ = server.Close()
	}()

	var gotNetwork, gotAddress string
	dialer := newIMAPDialer(func(_ context.Context, network, address string) (net.Conn, error) {
		gotNetwork, gotAddress = network, address
		return client, nil
	}, &tls.Config{RootCAs: roots})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialer(ctx, "tcp", IMAPAddress)
	if err != nil {
		t.Fatalf("IMAP TLS dial: %v", err)
	}
	state := conn.(*tls.Conn).ConnectionState()
	_ = conn.Close()
	if err := <-serverErr; err != nil {
		t.Fatalf("server TLS handshake: %v", err)
	}
	if gotNetwork != "tcp" || gotAddress != IMAPAddress {
		t.Errorf("injected dial target = %s %s", gotNetwork, gotAddress)
	}
	if state.ServerName != "imap.mail.me.com" {
		t.Errorf("verified TLS ServerName = %q", state.ServerName)
	}
	if state.Version < tls.VersionTLS12 {
		t.Errorf("TLS version = %x, want TLS 1.2 or newer", state.Version)
	}
}

func testIMAPCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "imap.mail.me.com"},
		DNSNames:              []string{"imap.mail.me.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}, roots
}
