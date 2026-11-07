package main

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
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type timeoutAudit struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (a *timeoutAudit) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.b.Write(p)
}

func (a *timeoutAudit) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.b.String()
}

func timeoutAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("controlled protocol phase did not complete")
		var zero T
		return zero
	}
}

func timeoutSlotsDrained(t *testing.T, slots chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range cap(slots) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			t.Fatal("handler slots were not released after actual completion")
		}
	}
}

// The reply bridge skips the separate source Message-ID parser defect. It uses
// the production send cache, recipient policy and SMTP adapter, not fake deduplication.
type timeoutReplyBridge struct {
	maildomain.Service
	calls      atomic.Int32
	ready      chan maildomain.SendResult
	cancelSeen chan struct{}
	duplicate  chan struct{}
	release    chan struct{}
}

func (b *timeoutReplyBridge) ReplyMessage(ctx context.Context, input maildomain.ReplyInput) (maildomain.SendResult, error) {
	n := b.calls.Add(1)
	if n == 2 {
		close(b.duplicate)
	}
	result, err := b.Service.SendMessage(ctx, maildomain.SendInput{
		To: []string{"recipient@example.invalid"}, Subject: "Synthetic reply bridge",
		Body: input.Body, IdempotencyKey: input.IdempotencyKey,
	})
	if n == 1 {
		b.ready <- result
		<-ctx.Done()
		close(b.cancelSeen)
		<-b.release
	}
	return result, err
}

func newTimeoutReplyProtocol(t *testing.T, svc maildomain.Service, ownerCtx context.Context, slots chan struct{}, audit io.Writer) *client.Client {
	t.Helper()
	red := security.NewRedactor("synthetic-timeout-secret")
	var requests atomic.Int32
	mw := timeoutMiddlewareWithLimit(toolTimeout, toolTimeoutGrace, slots)
	s := server.NewMCPServer("reply-timeout", "test", server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(), server.WithStrictInputSchemaDefault(),
		server.WithToolHandlerMiddleware(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
			bounded := mw(next)
			return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if requests.Add(1) == 1 {
					ctx = ownerCtx
				}
				return bounded(ctx, req)
			}
		}), server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(red)))
	mcptools.RegisterUnified(s, mcptools.Deps{MailService: svc, Redactor: red, Audit: security.NewAuditLogger(audit)},
		mcptools.NewCapabilityPlanFromConfig(false, timeoutMailCapabilities()))
	return timeoutProtocolClient(t, s)
}

func timeoutReplyRequest(body string) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Name = "reply_message"
	req.Params.Arguments = map[string]any{
		"mailbox": "Private source", "uid_validity": 9, "uid": 2,
		"body": body, "idempotency_key": "private-timeout-key",
	}
	return req
}

type timeoutCallOutcome struct {
	res *mcp.CallToolResult
	err error
}

func timeoutCallAsync(c *client.Client, ctx context.Context, req mcp.CallToolRequest) <-chan timeoutCallOutcome {
	ch := make(chan timeoutCallOutcome, 1)
	go func() {
		res, err := c.CallTool(ctx, req)
		ch <- timeoutCallOutcome{res, err}
	}()
	return ch
}

func timeoutCallPayload(t *testing.T, out timeoutCallOutcome) map[string]any {
	t.Helper()
	if out.err != nil {
		t.Fatal(out.err)
	}
	return timeoutPayload(t, out.res)
}

func assertMailTimeoutHint(t *testing.T, payload map[string]any) {
	t.Helper()
	hint, _ := payload["reconciliation"].(string)
	for _, required := range []string{"Do not resend automatically", "Sent", "recipients", "idempotency_key", "absence"} {
		if !strings.Contains(hint, required) {
			t.Errorf("mail timeout lacks %q guidance: %v", required, payload)
		}
	}
	if payload["code"] != "timeout" || payload["retryable"] != false || len(payload) != 4 {
		t.Errorf("unsafe synthetic mail timeout: %v", payload)
	}
	for _, private := range []string{"Private source", "recipient@example.invalid", "private-timeout-key", "synthetic-timeout-secret", "etag", "client_uid"} {
		if strings.Contains(fmt.Sprint(payload), private) {
			t.Errorf("inappropriate mail timeout content: %v", payload)
		}
	}
}

// A child process confines the synthetic CA to these tests. The production
// adapter still verifies smtp.mail.me.com and never uses InsecureSkipVerify.
func TestProtocolReplyTimeoutSMTP(t *testing.T) {
	const child = "GO_TEST_REPLY_TIMEOUT_CHILD"
	if os.Getenv(child) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProtocolReplyTimeoutSMTP$", "-test.v", "-test.timeout=50s")
		cmd.Env = append(os.Environ(), child+"=1")
		output, err := cmd.CombinedOutput()
		t.Logf("isolated SMTP protocol tests:\n%s", output)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	serverTLS := timeoutSMTPTrust(t)
	for _, phase := range []string{"before-dispatch", "data-ambiguous", "accepted-within-grace", "rejected-within-grace", "accepted-beyond-grace"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			ownerCtx, cancelOwner := context.WithCancel(ctx)
			defer cancelOwner()
			var dials, data atomic.Int32
			dataSeen := make(chan struct{}, 1)
			wireDone := make(chan error, 1)
			policy, err := maildomain.ParseRecipientPolicy("recipient@example.invalid")
			if err != nil {
				t.Fatal(err)
			}
			svc, err := maildomain.NewService(maildomain.Config{
				Address: "fixture@icloud.com", Password: "synthetic-timeout-secret", RecipientPolicy: policy,
			}, func(context.Context) (net.Conn, error) {
				return nil, fmt.Errorf("reply bridge must not access IMAP")
			}, func(ctx context.Context) (net.Conn, error) {
				dials.Add(1)
				local, remote := net.Pipe()
				go func() { wireDone <- timeoutSMTPPeer(ctx, remote, serverTLS, phase, &data, dataSeen) }()
				return local, nil
			}, false, true)
			if err != nil {
				t.Fatal(err)
			}
			b := &timeoutReplyBridge{Service: svc, ready: make(chan maildomain.SendResult, 1), cancelSeen: make(chan struct{}), duplicate: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(b.release) }) }
			defer release()
			slots := make(chan struct{}, maxInFlightHandlers)
			audit := &timeoutAudit{}
			if phase == "before-dispatch" {
				cancelOwner()
			}
			c := newTimeoutReplyProtocol(t, b, ownerCtx, slots, audit)
			req := timeoutReplyRequest("Synthetic reply body")
			first := timeoutCallAsync(c, ctx, req)
			if phase == "before-dispatch" {
				assertMailTimeoutHint(t, timeoutCallPayload(t, timeoutAwait(t, first)))
				if b.calls.Load() != 0 || dials.Load() != 0 || data.Load() != 0 || len(slots) != 0 {
					t.Fatal("pre-cancelled request dispatched or retained a slot")
				}
				return
			}
			timeoutAwait(t, dataSeen)
			t.Log("observed SMTP DATA payload and dot terminator over verified STARTTLS")
			var pendingDuplicate <-chan timeoutCallOutcome
			if phase == "data-ambiguous" {
				pendingDuplicate = timeoutCallAsync(c, ctx, req)
				timeoutAwait(t, b.duplicate)
				cancelOwner()
			}
			ready := timeoutAwait(t, b.ready)
			if phase != "data-ambiguous" {
				cancelOwner()
			}
			timeoutAwait(t, b.cancelSeen)
			wantStatus := string(maildomain.SendAccepted)
			wantCode := ""
			if phase == "data-ambiguous" {
				wantStatus, wantCode = string(maildomain.SendOutcomeUnknown), "outcome_unknown"
			} else if phase == "rejected-within-grace" {
				wantStatus, wantCode = string(maildomain.SendRejected), "authorization"
			}
			if string(ready.Status) != wantStatus {
				t.Fatalf("real SMTP outcome before handler release = %s, want %s", ready.Status, wantStatus)
			}
			synthetic := phase == "data-ambiguous" || phase == "accepted-beyond-grace"
			if !synthetic {
				release()
			}
			out := timeoutAwait(t, first)
			payload := timeoutCallPayload(t, out)
			if synthetic {
				assertMailTimeoutHint(t, payload)
				if len(slots) != 1 {
					t.Fatalf("stalled handler holds %d slots, want 1", len(slots))
				}
			} else if payload["status"] != wantStatus || (wantCode != "" && payload["code"] != wantCode) || out.res.IsError != (wantCode != "") {
				t.Fatalf("definitive result lost within grace: %v", payload)
			}
			if pendingDuplicate != nil {
				duplicate := timeoutCallPayload(t, timeoutAwait(t, pendingDuplicate))
				if duplicate["status"] != wantStatus || duplicate["code"] != wantCode {
					t.Fatalf("pending duplicate lost uncertain outcome: %v", duplicate)
				}
			}
			duplicate := timeoutCallPayload(t, timeoutAwait(t, timeoutCallAsync(c, ctx, req)))
			if duplicate["status"] != wantStatus || (wantCode != "" && duplicate["code"] != wantCode) || duplicate["messageId"] != ready.MessageID {
				t.Errorf("cached duplicate changed outcome or Message-ID: %v", duplicate)
			}
			conflict := timeoutCallPayload(t, timeoutAwait(t, timeoutCallAsync(c, ctx, timeoutReplyRequest("Changed body"))))
			if conflict["code"] != "conflict" {
				t.Errorf("changed keyed content did not conflict: %v", conflict)
			}
			release()
			timeoutSlotsDrained(t, slots)
			if err := timeoutAwait(t, wireDone); err != nil {
				t.Fatal(err)
			}
			if dials.Load() != 1 || data.Load() != 1 {
				t.Fatalf("duplicate SMTP submission: dials=%d complete_DATA=%d", dials.Load(), data.Load())
			}
			logs := audit.String()
			for _, private := range []string{"Private source", "recipient@example.invalid", ready.MessageID, "private-timeout-key", "synthetic-timeout-secret"} {
				if private != "" && strings.Contains(logs, private) {
					t.Errorf("private value in audit: %s", logs)
				}
			}
			if !strings.Contains(logs, `"resourceToken":`) || !strings.Contains(logs, `"domain":"mail"`) {
				t.Errorf("missing opaque Mail audit: %s", logs)
			}
			t.Logf("SMTP_dials=%d complete_DATA=%d held_slots_after_drain=%d audit=%s", dials.Load(), data.Load(), len(slots), logs)
		})
	}
}

func timeoutSMTPPeer(ctx context.Context, conn net.Conn, config *tls.Config, phase string, count *atomic.Int32, seen chan<- struct{}) error {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	wire := conn
	r := bufio.NewReader(wire)
	write := func(s string) error { _, err := io.WriteString(wire, s); return err }
	if err := write("220 fixture ESMTP\r\n"); err != nil {
		return err
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if count.Load() > 0 {
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
			if err := write("220 begin TLS\r\n"); err != nil {
				return err
			}
			wire = tls.Server(conn, config)
			r = bufio.NewReader(wire)
			continue
		case "AUTH":
			response = "235 authenticated\r\n"
		case "MAIL", "RCPT", "RSET":
		case "DATA":
			if err := write("354 send data\r\n"); err != nil {
				return err
			}
			payload, err := io.ReadAll(textproto.NewReader(r).DotReader())
			if err != nil {
				return err
			}
			if !bytes.Contains(payload, []byte("Synthetic reply body")) {
				return fmt.Errorf("unexpected synthetic SMTP payload")
			}
			count.Add(1)
			seen <- struct{}{}
			if phase == "data-ambiguous" {
				<-ctx.Done()
				return nil
			}
			if phase == "rejected-within-grace" {
				response = "554 rejected\r\n"
			}
		default:
			return fmt.Errorf("unexpected SMTP command")
		}
		if err := write(response); err != nil {
			return err
		}
	}
}

func timeoutSMTPTrust(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Timeout fixture root"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{"smtp.mail.me.com"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", path)
	t.Setenv("SSL_CERT_DIR", dir)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: key}}}
}
