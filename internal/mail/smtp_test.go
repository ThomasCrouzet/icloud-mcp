package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSMTPStartTLSPostTLSHelloAndAuthOrder(t *testing.T) {
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	clientConn, events, serverErr := startScriptedSMTP(serverTLS,
		"250-localhost\r\n250 AUTH PLAIN\r\n", "235 2.7.0 authenticated\r\n", true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	concrete := session.(*smtpClientSession)
	if concrete.client.CommandTimeout <= 0 || concrete.client.CommandTimeout > 2*time.Second {
		t.Fatalf("command timeout exceeds context: %v", concrete.client.CommandTimeout)
	}
	if concrete.client.SubmissionTimeout <= 0 || concrete.client.SubmissionTimeout > 2*time.Second {
		t.Fatalf("submission timeout exceeds context: %v", concrete.client.SubmissionTimeout)
	}
	if err := session.Auth("sender@icloud.com", "test-password"); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if got, want := readSMTPEvents(t, events, 5), []string{"EHLO", "STARTTLS", "TLS", "EHLO", "AUTH"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SMTP setup order = %v, want %v", got, want)
	}
}

func TestSMTPAuthUsesPostTLSCapabilities(t *testing.T) {
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	clientConn, events, serverErr := startScriptedSMTP(serverTLS, "250 localhost\r\n", "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	authErr := session.Auth("sender@icloud.com", "test-password")
	if !errors.Is(authErr, errSMTPAuthUnavailable) {
		t.Fatalf("post-TLS AUTH omission = %v", authErr)
	}
	_ = session.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if got, want := readSMTPEvents(t, events, 4), []string{"EHLO", "STARTTLS", "TLS", "EHLO"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SMTP setup order = %v, want %v", got, want)
	}
}

func TestSMTPPostTLSHelloErrorIsNotAuthUnavailable(t *testing.T) {
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	clientConn, events, serverErr := startScriptedSMTP(serverTLS, "421 4.3.0 unavailable\r\n", "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if session != nil || err == nil {
		t.Fatalf("post-TLS EHLO failure returned session=%T, err=%v", session, err)
	}
	if errors.Is(err, errSMTPAuthUnavailable) {
		t.Fatal("post-TLS EHLO failure was mislabeled as unavailable PLAIN AUTH")
	}
	if got := errorCode(t, mapSMTPError(err, ctx, false, "SMTP TLS or greeting negotiation failed")); got != CodeUnavailable {
		t.Fatalf("post-TLS EHLO classification = %s", got)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if got, want := readSMTPEvents(t, events, 4), []string{"EHLO", "STARTTLS", "TLS", "EHLO"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SMTP setup order = %v, want %v", got, want)
	}
}

func TestSMTPRejectsUnverifiedTLSBeforeAuth(t *testing.T) {
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	clientTLS.RootCAs = x509.NewCertPool()
	clientConn, events, serverErr := startScriptedSMTP(serverTLS,
		"250-localhost\r\n250 AUTH PLAIN\r\n", "235 2.7.0 authenticated\r\n", true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if session != nil || err == nil {
		t.Fatalf("unverified TLS returned session=%T, err=%v", session, err)
	}
	if errors.Is(err, errSMTPAuthUnavailable) {
		t.Fatal("TLS verification failure was mislabeled as unavailable PLAIN AUTH")
	}
	if err := <-serverErr; err == nil {
		t.Fatal("server handshake unexpectedly succeeded with an untrusted certificate")
	}
	if got, want := readSMTPEvents(t, events, 2), []string{"EHLO", "STARTTLS"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("commands before TLS verification = %v, want %v", got, want)
	}
}

func TestSMTPAuthReplyClassifications(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		code  Code
	}{
		{name: "temporary 454", reply: "454 4.7.0 secret-marker\r\n", code: CodeUnavailable},
		{name: "credentials rejected", reply: "535 5.7.8 secret-marker\r\n", code: CodeAuthentication},
		{name: "definitive rejection", reply: "534 5.7.9 secret-marker\r\n", code: CodeAuthentication},
		{name: "unexpected class", reply: "399 secret-marker\r\n", code: CodeProtocolError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientTLS, serverTLS := smtpTestTLSConfigs(t)
			clientConn, events, serverErr := startScriptedSMTP(serverTLS,
				"250-localhost\r\n250 AUTH PLAIN\r\n", test.reply, true)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
			if err != nil {
				t.Fatal(err)
			}
			authErr := session.Auth("sender@icloud.com", "test-password")
			mapped := mapSMTPAuthError(authErr, ctx)
			if got := errorCode(t, mapped); got != test.code {
				t.Fatalf("AUTH reply classification = %s, want %s", got, test.code)
			}
			if strings.Contains(mapped.Error(), "secret-marker") {
				t.Fatalf("server response leaked through sanitized error: %v", mapped)
			}
			_ = session.Close()
			if err := <-serverErr; err != nil {
				t.Fatal(err)
			}
			if got := readSMTPEvents(t, events, 5); strings.Join(got, ",") != "EHLO,STARTTLS,TLS,EHLO,AUTH" {
				t.Fatalf("SMTP AUTH order = %v", got)
			}
		})
	}
}

func TestSMTPDeadlineClampZeroSemantics(t *testing.T) {
	t.Parallel()
	limit := time.Now().Add(time.Minute)
	earlier := limit.Add(-time.Second)
	later := limit.Add(time.Second)
	clamped := &deadlineConn{deadline: limit}
	for name, test := range map[string]struct {
		requested time.Time
		want      time.Time
	}{
		"clear":   {want: limit},
		"earlier": {requested: earlier, want: earlier},
		"later":   {requested: later, want: limit},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clamped.clamp(test.requested); !got.Equal(test.want) {
				t.Fatalf("clamp() = %v, want %v", got, test.want)
			}
		})
	}
	requested := time.Now().Add(time.Second)
	if got := (&deadlineConn{}).clamp(requested); !got.Equal(requested) {
		t.Fatalf("zero absolute deadline changed requested deadline: %v", got)
	}
}

func TestSMTPInboundBudgetRejectsOversizedGreetingWithoutWaitingForDeadline(t *testing.T) {
	clientTLS, _ := smtpTestTLSConfigs(t)
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer func() { _ = serverConn.Close() }()
		line := "220-" + strings.Repeat("x", 1000) + "\r\n"
		for {
			if _, err := io.WriteString(serverConn, line); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if session != nil || !errors.Is(err, errSMTPInboundLimit) {
		t.Fatalf("oversized greeting returned session=%T, err=%v", session, err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("oversized greeting waited for the operation deadline")
	}
	if got := errorCode(t, mapSMTPError(err, ctx, false, "SMTP greeting failed")); got != CodePayloadTooLarge {
		t.Fatalf("oversized greeting classification = %s", got)
	}
	<-serverDone
}

func TestSMTPInboundBudgetContinuesAcrossSTARTTLS(t *testing.T) {
	clientTLS, serverTLS := smtpTestTLSConfigs(t)
	line := "250-" + strings.Repeat("x", 1000) + "\r\n"
	postTLSReply := strings.Repeat(line, int(maxSMTPInboundBytes)/len(line)+100)
	clientConn, events, serverErr := startScriptedSMTP(serverTLS, postTLSReply, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	session, err := newSMTPSessionWithTLS(ctx, clientConn, clientTLS)
	if session != nil || !errors.Is(err, errSMTPInboundLimit) {
		t.Fatalf("oversized post-TLS EHLO returned session=%T, err=%v", session, err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("oversized post-TLS EHLO waited for the operation deadline")
	}
	if got := errorCode(t, mapSMTPError(err, ctx, false, "SMTP EHLO failed")); got != CodePayloadTooLarge {
		t.Fatalf("oversized post-TLS classification = %s", got)
	}
	if got, want := readSMTPEvents(t, events, 4), []string{"EHLO", "STARTTLS", "TLS", "EHLO"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SMTP setup order = %v, want %v", got, want)
	}
	<-serverErr
}

func TestWriteSMTPPayloadDetectsShortWrite(t *testing.T) {
	t.Parallel()
	err := writeSMTPPayload(shortWriter{}, []byte("message"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(payload []byte) (int, error) { return len(payload) - 1, nil }

func startScriptedSMTP(serverTLS *tls.Config, postTLSReply, authReply string, expectAuth bool) (net.Conn, <-chan string, <-chan error) {
	clientConn, serverConn := net.Pipe()
	events := make(chan string, 5)
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "220 smtp.test ESMTP ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		events <- smtpCommandName(line)
		if _, err := fmt.Fprint(serverConn, "250-localhost\r\n250-AUTH PLAIN\r\n250 STARTTLS\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err = reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		events <- smtpCommandName(line)
		if _, err := fmt.Fprint(serverConn, "220 2.0.0 begin TLS\r\n"); err != nil {
			serverErr <- err
			return
		}
		tlsConn := tls.Server(serverConn, serverTLS)
		if err := tlsConn.Handshake(); err != nil {
			serverErr <- err
			return
		}
		events <- "TLS"
		reader = bufio.NewReader(tlsConn)
		line, err = reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		events <- smtpCommandName(line)
		if _, err := fmt.Fprint(tlsConn, postTLSReply); err != nil {
			serverErr <- err
			return
		}
		if !strings.HasPrefix(postTLSReply, "250") {
			serverErr <- nil
			return
		}
		if !expectAuth {
			line, err = reader.ReadString('\n')
			if err == nil {
				serverErr <- fmt.Errorf("unexpected command without post-TLS AUTH capability: %q", line)
				return
			}
			serverErr <- nil
			return
		}
		line, err = reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		events <- smtpCommandName(line)
		_, err = fmt.Fprint(tlsConn, authReply)
		serverErr <- err
	}()
	return clientConn, events, serverErr
}

func smtpCommandName(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

func readSMTPEvents(t *testing.T, events <-chan string, count int) []string {
	t.Helper()
	out := make([]string, 0, count)
	for len(out) < count {
		select {
		case event := <-events:
			out = append(out, event)
		case <-time.After(time.Second):
			t.Fatalf("timed out after SMTP events %v", out)
		}
	}
	return out
}

func smtpTestTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "SMTP test root"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: smtpHost},
		DNSNames:     []string{smtpHost},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCert)
	clientTLS := &tls.Config{ServerName: smtpHost, RootCAs: roots, MinVersion: tls.VersionTLS12}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: leafKey}},
		MinVersion:   tls.VersionTLS12,
	}
	return clientTLS, serverTLS
}
