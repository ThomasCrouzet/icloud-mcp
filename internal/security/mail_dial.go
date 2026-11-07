package security

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
)

const (
	// IMAPAddress is the only permitted IMAP endpoint.
	IMAPAddress = "imap.mail.me.com:993"
	// SMTPAddress is the only permitted SMTP submission endpoint.
	SMTPAddress = "smtp.mail.me.com:587"

	imapServerName = "imap.mail.me.com"
)

// DialContextFunc is compatible with net.Dialer.DialContext and permits test
// injection without making the production destination configurable.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

type imapFailureStage uint8

const (
	imapConnect imapFailureStage = iota
	imapTLS
)

type imapFailure struct {
	stage imapFailureStage
	cause error
}

func (e *imapFailure) Error() string { return "IMAP connection setup failed" }
func (e *imapFailure) Unwrap() error { return e.cause }

// IMAPFailureDetails projects only failures observed by the fixed IMAP dialer.
// Wrapped causes remain private and preserve existing timeout and retry checks.
func IMAPFailureDetails(err error) (stage, reason string) {
	var failure *imapFailure
	if !errors.As(err, &failure) || failure == nil {
		return "", ""
	}
	switch failure.stage {
	case imapConnect:
		stage = "connect"
	case imapTLS:
		stage = "tls"
	default:
		return "", ""
	}
	reason = "unknown"
	var dns *net.DNSError
	var socket *net.OpError
	var verification *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.Is(failure.cause, context.Canceled):
		reason = "canceled"
	case errors.Is(failure.cause, context.DeadlineExceeded):
		reason = "deadline"
	case errors.As(failure.cause, &netErr) && netErr.Timeout():
		reason = "deadline"
	case stage == "connect" && errors.As(failure.cause, &dns):
		reason = "dns"
	case stage == "connect" && errors.As(failure.cause, &socket):
		reason = "connection_failed"
	case stage == "tls" && errors.As(failure.cause, &verification):
		reason = "certificate_verification"
	case stage == "tls" && (errors.Is(failure.cause, io.EOF) || errors.Is(failure.cause, io.ErrUnexpectedEOF)):
		reason = "eof"
	}
	return stage, reason
}

// DialIMAPContext dials the fixed IMAP endpoint and completes verified implicit
// TLS before returning. The caller-supplied address must match the fixed target
// and is rejected before the default dialer can perform DNS resolution.
func DialIMAPContext(ctx context.Context, network, address string) (net.Conn, error) {
	return NewIMAPDialer((&net.Dialer{}).DialContext)(ctx, network, address)
}

// NewIMAPDialer wraps an injected context dialer with the fixed endpoint and
// verified implicit-TLS policy.
func NewIMAPDialer(dial DialContextFunc) DialContextFunc {
	return newIMAPDialer(dial, nil)
}

func newIMAPDialer(dial DialContextFunc, tlsConfig *tls.Config) DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != IMAPAddress {
			return nil, fmt.Errorf("network allowlist: IMAP destination rejected")
		}
		if dial == nil {
			return nil, fmt.Errorf("network allowlist: IMAP dialer is unavailable")
		}

		raw, err := dial(ctx, "tcp", IMAPAddress)
		if err != nil {
			return nil, &imapFailure{stage: imapConnect, cause: err}
		}
		config := &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: imapServerName,
		}
		if tlsConfig != nil {
			config.RootCAs = tlsConfig.RootCAs
			config.Time = tlsConfig.Time
		}
		conn := tls.Client(raw, config)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, &imapFailure{stage: imapTLS, cause: err}
		}
		return conn, nil
	}
}

// DialSMTPContext dials the fixed SMTP submission endpoint over TCP. TLS is
// intentionally not started here; the SMTP client must require STARTTLS after
// EHLO using a fixed smtp.mail.me.com ServerName.
func DialSMTPContext(ctx context.Context, network, address string) (net.Conn, error) {
	return NewSMTPDialer((&net.Dialer{}).DialContext)(ctx, network, address)
}

// NewSMTPDialer wraps an injected context dialer with the fixed SMTP endpoint
// policy. Arbitrary addresses are rejected before invoking the dialer.
func NewSMTPDialer(dial DialContextFunc) DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != SMTPAddress {
			return nil, fmt.Errorf("network allowlist: SMTP destination rejected")
		}
		if dial == nil {
			return nil, fmt.Errorf("network allowlist: SMTP dialer is unavailable")
		}
		return dial(ctx, "tcp", SMTPAddress)
	}
}
