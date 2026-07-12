// Command icloud-mcp: stdio MCP server exposing the iCloud calendar over
// CalDAV. See README.md at the repo root for the product spec and threat
// model.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/health"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// toolTimeout bounds the execution of each MCP tool call, strictly below the
// HTTP timeout (30s) so the tool fails cleanly before the underlying HTTP
// request times out on its own.
const toolTimeout = 25 * time.Second

// discoveryTimeout bounds the iCloud discovery performed at boot to validate
// the credentials before starting the MCP server.
const discoveryTimeout = 20 * time.Second

func main() {
	healthAddr := flag.String("health", "", "HTTP healthcheck address (e.g. 127.0.0.1:8797), disabled if empty")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// 1. Configuration: failure = os.Exit(1) BEFORE any network access.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// 2. Redaction: ALL stderr goes through the RedactingWriter from here on.
	// The security requirements mandate redacting the password and the email,
	// so cfg.Email is included on the same footing (defense in depth; this
	// does not affect the boot-time config error above, raised BEFORE this
	// Redactor is created).
	red := security.NewRedactor(
		cfg.Password,
		cfg.Email,
		base64.StdEncoding.EncodeToString([]byte(cfg.Email+":"+cfg.Password)), // Basic Authorization header value
		url.QueryEscape(cfg.Password), // URL-encoded form
	)
	stderr := security.NewRedactingWriter(os.Stderr, red)
	// Structured JSON logs (one object per line): the MCP host can parse them
	// and route to a log indexer. The level is configurable via
	// ICLOUD_MCP_LOG_LEVEL (debug/info/warn/error); default info. Everything
	// still flows through the redacting writer so secrets never leak.
	slog.SetDefault(slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))
	// The default stdlib `log` logger (log.Fatalf/log.Printf, used before
	// this point for the config, and by any dependency calling log.Print*
	// directly) writes to RAW os.Stderr by default, which is not covered by
	// the redaction. Redirect it explicitly to the redacting writer so that
	// NO logging path bypasses the redaction.
	log.SetOutput(stderr)
	audit := security.NewAuditLogger(stderr)

	// 3. Hardened HTTP client (network allowlist + verified TLS) + Basic Auth.
	httpClient := security.NewICloudHTTPClient(cfg.Timeout)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, cfg.Email, cfg.Password)
	// Retry (429/502/503/504 with Retry-After + backoff + jitter) and error
	// classification (stable codes + Apple-aware messages) sit ON TOP of the
	// allowlist+auth doer, so every CalDAV request, whether hand-rolled
	// (discovery, REPORT, conditional PUT) or via go-webdav, goes through
	// both. See internal/icloud/retry.go.
	doer := icloud.NewRetryClassifier(authHTTP)

	// 4. iCloud service + boot-time discovery (validates the credentials
	// before starting the MCP server).
	ic := icloud.NewClient(doer, security.ICloudBaseURL, security.IsICloudHost)
	discoverCtx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	err = ic.Discover(discoverCtx)
	cancel()
	if err != nil {
		slog.Error("iCloud discovery failed (check ICLOUD_EMAIL and the app-specific password)", "err", err)
		os.Exit(1)
	}
	svc := icloud.NewGuardedService(ic, 2, 500*time.Millisecond)

	// 5. MCP server.
	s := server.NewMCPServer("icloud-mcp", version,
		server.WithToolCapabilities(false),
		// WithRecovery remains as an extra safety net, but it is
		// mcptools.RecoverRedactMiddleware (registered below, hence closer
		// to the handler in the stack) that intercepts a panic first and
		// produces a REDACTED response; otherwise WithRecovery alone would
		// serialize the raw (unredacted) error onto the JSON-RPC channel.
		server.WithRecovery(),
		server.WithInstructions("iCloud calendar server (CalDAV). Call list_calendars first to get the calendar paths."),
		server.WithToolHandlerMiddleware(timeoutMiddleware(toolTimeout)),
		server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(red)),
	)
	mcptools.Register(s, mcptools.Deps{Service: svc, Audit: audit, Redactor: red}, cfg.ReadOnly)

	// 6. Optional healthcheck (off by default).
	if *healthAddr != "" {
		h, err := health.Start(*healthAddr, version, func() any { return svc.RateLimitStatus() })
		if err != nil {
			slog.Error("healthcheck startup failed", "err", err)
			os.Exit(1)
		}
		defer func() { _ = h.Close() }()
	}

	slog.Info("server started", "version", version, "readOnly", cfg.ReadOnly)

	// 7. Stdio: ServeStdio handles SIGTERM/SIGINT itself. The transport
	// error logger MUST be wired to the redacting writer, otherwise mcp-go
	// logs to raw os.Stderr and bypasses the redaction.
	errLogger := log.New(stderr, "", log.LstdFlags)
	if err := server.ServeStdio(s, server.WithErrorLogger(errLogger)); err != nil {
		slog.Error("server stopped with an error", "err", err)
		os.Exit(1)
	}
}

// timeoutMiddleware bounds the execution time of each tool call.
func timeoutMiddleware(d time.Duration) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, req)
		}
	}
}
