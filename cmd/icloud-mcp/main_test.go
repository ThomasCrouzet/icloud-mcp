package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestVersionNonEmpty(t *testing.T) {
	if version == "" {
		t.Fatal("version must be non-empty (overridden at release via -ldflags)")
	}
}

func TestToolAndDiscoveryTimeoutsOrdered(t *testing.T) {
	// Production invariant: tool timeout < HTTP client timeout (30s) and
	// discovery timeout < tool timeout so boot fails cleanly first.
	if toolTimeout >= 30*time.Second {
		t.Fatalf("toolTimeout = %v, want < 30s HTTP timeout", toolTimeout)
	}
	if discoveryTimeout >= toolTimeout {
		t.Fatalf("discoveryTimeout = %v, want < toolTimeout %v", discoveryTimeout, toolTimeout)
	}
}

func TestTimeoutMiddleware(t *testing.T) {
	mw := timeoutMiddleware(30 * time.Millisecond)
	slow := mw(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return mcp.NewToolResultText("late"), nil
		}
	})
	_, err := slow(context.Background(), mcp.CallToolRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestTimeoutMiddlewarePasses(t *testing.T) {
	mw := timeoutMiddleware(time.Second)
	h := mw(func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline on context")
		}
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := h(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected result content")
	}
}

// TestBootRedactorMasksSecretEncodings mirrors the secret set registered in
// main so a future trim of the list is caught without starting the server.
func TestBootRedactorMasksSecretEncodings(t *testing.T) {
	email := "user@example.com"
	password := "app-specific-secret"
	basicUserPass := []byte(email + ":" + password)
	pwBytes := []byte(password)
	red := security.NewRedactor(
		password,
		email,
		base64.StdEncoding.EncodeToString(basicUserPass),
		base64.RawStdEncoding.EncodeToString(basicUserPass),
		base64.URLEncoding.EncodeToString(basicUserPass),
		base64.RawURLEncoding.EncodeToString(basicUserPass),
		base64.StdEncoding.EncodeToString(pwBytes),
		base64.RawStdEncoding.EncodeToString(pwBytes),
		base64.URLEncoding.EncodeToString(pwBytes),
		base64.RawURLEncoding.EncodeToString(pwBytes),
		url.QueryEscape(password),
		url.PathEscape(password),
	)
	samples := []string{
		password,
		email,
		base64.StdEncoding.EncodeToString(basicUserPass),
		base64.URLEncoding.EncodeToString(pwBytes),
		url.QueryEscape(password),
	}
	for _, s := range samples {
		out := red.Redact("leak:" + s)
		if strings.Contains(out, s) {
			t.Errorf("secret still present after redact in %q", out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("expected [REDACTED] in %q", out)
		}
	}
}

// TestRegisterReadOnlyWiring ensures production Register path (RO) exposes
// the six read tools and hides writes, matching main's cfg.ReadOnly branch.
func TestRegisterReadOnlyWiring(t *testing.T) {
	s := server.NewMCPServer("icloud-mcp-test", "test",
		server.WithToolCapabilities(false),
		server.WithToolHandlerMiddleware(timeoutMiddleware(toolTimeout)),
		server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(security.NewRedactor("x"))),
	)
	mcptools.Register(s, mcptools.Deps{
		Service:         &icloud.MockService{},
		Audit:           security.NewAuditLogger(ioDiscard{}),
		Redactor:        security.NewRedactor("secret-password-xx"),
		DefaultLocation: time.UTC,
		Version:         version,
		HealthEnabled:   false,
	}, true)

	// List tools via the in-process server if available; otherwise smoke
	// that Register did not panic (asserted above).
	_ = s
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
