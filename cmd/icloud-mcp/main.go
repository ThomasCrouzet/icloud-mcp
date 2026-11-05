// Command icloud-mcp is a unified stdio MCP server for Apple/iCloud Calendar,
// Contacts, and Mail. See README.md at the repo root for the product spec and
// threat model.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"time"
	_ "time/tzdata" // embed the IANA database: ICLOUD_MCP_DEFAULT_TZ and TZID parsing must not depend on the host/container having zoneinfo installed

	"github.com/emersion/go-webdav"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/health"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/mcptools"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// version prefers the release ldflags override, then Go module build metadata.
var version = "dev"

func init() {
	if version != "" && version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	if version == "" {
		version = "dev"
	}
}

// toolTimeout limits each MCP tool call to 25 seconds. This limit is lower than
// the 30-second HTTP timeout. The tool can therefore fail cleanly before the
// HTTP request reaches its timeout.
const toolTimeout = 25 * time.Second

// toolTimeoutGrace defines how long the middleware waits after cancellation.
// During this period, completed work or cancelled I/O can produce a real
// handler result instead of a synthetic timeout.
const toolTimeoutGrace = 2 * time.Second

// maxInFlightHandlers caps handler goroutines so dependencies that ignore
// cancellation cannot make abandoned work grow without bound.
const maxInFlightHandlers = 32

// handlerSlots bounds all in-flight handler goroutines. A slot remains held
// after a client-visible timeout until the underlying handler actually exits.
var handlerSlots = make(chan struct{}, maxInFlightHandlers)

func main() {
	healthAddr := flag.String("health", "", "HTTP healthcheck address (e.g. 127.0.0.1:8797), disabled if empty")
	auditFormatFlag := flag.String("audit-format", "json", "mutation audit format on stderr: json (default) or text")
	showVersion := flag.Bool("version", false, "print the version and exit")
	doctor := flag.Bool("doctor", false, "run bounded read-only domain probes and exit")
	doctorFormat := flag.String("doctor-format", "text", "doctor output format: text or json")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	auditFormat, err := security.ParseAuditFormat(*auditFormatFlag)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// 1. Configuration: failure = os.Exit(1) BEFORE any network access.
	// config.Load errors must omit the email and password. This path uses the
	// default log sink before it installs the Redactor. See config.Validate and
	// loadCredential.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	if *doctor {
		os.Exit(runDoctor(cfg, *doctorFormat, os.Stdout))
	}

	// Fixed production transports remain separate from the shared lifecycle.
	// Disabled domains construct no client and make no network request.
	capabilities := cfg.Capabilities()
	var ic *icloud.Client
	if capabilities.CalendarRead || capabilities.CalendarWrite || capabilities.CalendarDelete {
		ic = newCalendarClient(cfg)
	}
	contactsService, mailService, err := newOptionalServices(cfg)
	if err == nil {
		err = runServer(cfg, auditFormat, *healthAddr, ic, contactsService, mailService, os.Stdin, os.Stdout)
	}
	if err != nil {
		log.Printf("server failed: %s", newBootRedactor(cfg).Redact(err.Error()))
		os.Exit(1)
	}
}

// runServer shares discovery, registration, redaction, and stdio with the
// fixture executable. Only main constructs production transports.
func runServer(cfg *config.Config, auditFormat security.AuditFormat, healthAddr string, ic *icloud.Client, contactsService contacts.Service, mailService maildomain.Service, stdin io.Reader, stdout io.Writer) error {
	// 2. Redaction: ALL stderr goes through the RedactingWriter from here on.
	// Calendar credentials are always covered. Mail credentials and SASL PLAIN
	// variants are added only when the Mail domain is enabled.
	red := newBootRedactor(cfg)
	stderr := security.NewRedactingWriter(os.Stderr, red)
	defer func() { _ = stderr.Flush() }()
	// Structured JSON logs (one object per line): the MCP host can parse them
	// and route to a log indexer. The level is configurable via
	// ICLOUD_MCP_LOG_LEVEL (debug/info/warn/error); default info. Everything
	// still flows through the redacting writer so secrets never leak.
	slog.SetDefault(slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))
	// The default stdlib `log` logger (and any dependency calling log.Print*)
	// writes to RAW os.Stderr by default. Redirect it explicitly so that NO
	// logging path bypasses the redaction after boot.
	log.SetOutput(stderr)
	audit := security.NewAuditLoggerWithFormat(stderr, auditFormat)
	if cfg.SMTPRecipientPolicy.AllowAll() {
		slog.Warn("SMTP recipient policy is allow-all (*); any syntactically valid address may receive mail after AUTH")
	}

	plan := mcptools.NewCapabilityPlanFromConfig(cfg.ReadOnly, cfg.Capabilities())

	// 4. Calendar transports are constructed only when Calendar capabilities
	// are effective. Discovery is lazy on the first Calendar tool call, so a
	// remote Calendar outage cannot prevent Mail or Contacts from starting.
	var calendarService icloud.Service
	var guardedCalendar *icloud.GuardedService
	if plan.CalendarReadEnabled() || plan.CalendarWritesEnabled() {
		if ic == nil {
			return fmt.Errorf("calendar capabilities are enabled but the Calendar client is missing")
		}
		guardedCalendar = icloud.NewGuardedService(ic, 2, 500*time.Millisecond)
		calendarService = guardedCalendar
	}

	// 5. MCP server.
	s := newMCPServer(red)
	healthEnabled := healthAddr != ""
	mcptools.RegisterUnified(s, mcptools.Deps{
		Service:         calendarService,
		ContactsService: contactsService,
		MailService:     mailService,
		Audit:           audit,
		Redactor:        red,
		DefaultLocation: cfg.DefaultLocation,
		Version:         version,
		HealthEnabled:   healthEnabled,
	}, plan)

	// 6. Optional healthcheck (off by default).
	if healthEnabled {
		domains := map[string]health.DomainStatus{
			"calendar": {Status: startupDomainStatus(plan.CalendarReadEnabled() || plan.CalendarWritesEnabled())},
			"contacts": {Status: startupDomainStatus(plan.ContactsEnabled())},
			"mail":     {Status: startupDomainStatus(plan.MailEnabled())},
		}
		h, err := health.Start(healthAddr, version, domains, func() any {
			return collectRateLimits(guardedCalendar, contactsService, mailService)
		})
		if err != nil {
			return fmt.Errorf("healthcheck startup failed: %w", err)
		}
		defer func() { _ = h.Close() }()
	}

	if cfg.DefaultLocation == nil || cfg.DefaultLocation == time.UTC {
		slog.Warn("ICLOUD_MCP_DEFAULT_TZ is unset or UTC: bare local start/end times are interpreted as UTC; set ICLOUD_MCP_DEFAULT_TZ to the calendar owner's IANA timezone (e.g. Europe/Paris) to avoid offset mistakes by the calling agent")
	}
	slog.Info("server started",
		"readOnly", cfg.ReadOnly,
		"calendarEnabled", plan.CalendarReadEnabled() || plan.CalendarWritesEnabled(),
		"contactsEnabled", plan.ContactsEnabled(),
		"mailEnabled", plan.MailEnabled(),
		"mailMutationsEnabled", plan.MailMutationsEnabled() || plan.MailTrashEnabled(),
		"mailSendEnabled", plan.MailSendEnabled(),
		"healthcheckActive", healthEnabled,
		"toolCount", plan.ToolCount(),
	)

	// 7. Stdio uses mcp-go's custom-reader Listen API so input can be bounded.
	// The error logger MUST use the redacting writer, otherwise transport logs
	// bypass stderr redaction.
	errLogger := log.New(stderr, "", log.LstdFlags)
	return serveBoundedStdio(s, stdin, stdout, errLogger, red)
}

func newMCPServer(red *security.Redactor) *server.MCPServer {
	return server.NewMCPServer("icloud-mcp", version,
		server.WithToolCapabilities(false),
		server.WithInputSchemaValidation(),
		server.WithStrictInputSchemaDefault(),
		// RecoverRedactMiddleware runs closer to the handler and catches panics
		// first. It redacts each error before JSON-RPC serializes it. Keep
		// WithRecovery as a second safety layer.
		server.WithRecovery(),
		server.WithInstructions("Unified Apple/iCloud server. Calendar is always available; optional Contacts and Mail tools appear only when enabled. Call the relevant list tool before using domain-specific resource identifiers."),
		server.WithToolHandlerMiddleware(timeoutMiddleware(toolTimeout)),
		server.WithToolHandlerMiddleware(mcptools.RecoverRedactMiddleware(red)),
	)
}

// timeoutMiddleware bounds the execution time of each tool call. It cancels
// the handler context at the deadline. It then waits briefly for cancelled I/O
// or work that just finished. If no result arrives, it returns a synthetic
// timeout.
// Mutation tools get reconciliation guidance because a late server apply is
// still possible after the client-visible deadline.
func timeoutMiddleware(d time.Duration) server.ToolHandlerMiddleware {
	return timeoutMiddlewareWithLimit(d, toolTimeoutGrace, handlerSlots)
}

func timeoutMiddlewareWithLimit(d, graceDuration time.Duration, slots chan struct{}) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			if ctx.Err() != nil {
				return toolTimeoutResult(req.Params.Name), nil
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return toolTimeoutResult(req.Params.Name), nil
			}
			if ctx.Err() != nil {
				<-slots
				return toolTimeoutResult(req.Params.Name), nil
			}
			type outcome struct {
				res *mcp.CallToolResult
				err error
			}
			ch := make(chan outcome, 1)
			go func() {
				defer func() { <-slots }()
				res, err := next(ctx, req)
				ch <- outcome{res: res, err: err}
			}()
			select {
			case out := <-ch:
				return out.res, out.err
			case <-ctx.Done():
				grace := time.NewTimer(graceDuration)
				select {
				case out := <-ch:
					grace.Stop()
					// Prefer a real handler result after cancel (success or
					// classified error) over a synthetic timeout.
					return out.res, out.err
				case <-grace.C:
					// The buffered channel lets the handler finish without a
					// receiver; its slot is released by the handler goroutine.
					return toolTimeoutResult(req.Params.Name), nil
				}
			}
		}
	}
}

func toolTimeoutResult(toolName string) *mcp.CallToolResult {
	if isMutationToolName(toolName) {
		return mcp.NewToolResultError(`{"code":"timeout","message":"tool deadline exceeded after a mutation may have been dispatched","retryable":false,"reconciliation":"Re-read the target before retrying. Prefer client_uid, idempotency_key, and etag on writes."}`)
	}
	return mcp.NewToolResultError(`{"code":"timeout","message":"tool deadline exceeded","retryable":false}`)
}

func isMutationToolName(name string) bool {
	switch name {
	case "create_event", "update_event", "delete_event",
		"create_contact", "update_contact", "delete_contact",
		"set_message_flags", "move_message", "trash_message", "send_message":
		return true
	default:
		return false
	}
}

func domainStatus(enabled bool) string {
	if enabled {
		return "ok"
	}
	return "disabled"
}

func startupDomainStatus(enabled bool) string {
	if enabled {
		return "not_checked"
	}
	return "disabled"
}

type rateLimitReporter interface {
	RateLimitStatus() map[string]any
}

func collectRateLimits(calendar *icloud.GuardedService, contactsService contacts.Service, mailService maildomain.Service) map[string]any {
	out := map[string]any{}
	if calendar != nil {
		out["calendar"] = calendar.RateLimitStatus()
	}
	if reporter, ok := contactsService.(rateLimitReporter); ok {
		out["contacts"] = reporter.RateLimitStatus()
	}
	if reporter, ok := mailService.(rateLimitReporter); ok {
		out["mail"] = reporter.RateLimitStatus()
	}
	return out
}

func newCalendarClient(cfg *config.Config) *icloud.Client {
	calendarCredentials := security.CredentialPair{
		Username: strings.Clone(cfg.Email),
		Password: strings.Clone(cfg.Password),
	}
	httpClient := security.NewICloudHTTPClient(cfg.Timeout)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, calendarCredentials.Username, calendarCredentials.Password)
	doer := icloud.NewRetryClassifier(authHTTP)
	return icloud.NewClient(doer, security.ICloudBaseURL, security.IsICloudHost)
}

func newBootRedactor(cfg *config.Config) *security.Redactor {
	capabilities := cfg.Capabilities()
	var credentials []security.CredentialPair
	if capabilities.CalendarRead || capabilities.CalendarWrite || capabilities.CalendarDelete ||
		capabilities.ContactsRead || capabilities.ContactsWrite || capabilities.ContactsDelete {
		credentials = append(credentials, security.CredentialPair{
			Username: cfg.Email,
			Password: cfg.Password,
		})
	}
	if capabilities.MailRead || capabilities.MailMutate || capabilities.MailTrash || capabilities.MailSend {
		credentials = append(credentials, security.CredentialPair{
			Username: cfg.MailAddress,
			Password: cfg.MailPassword,
		})
	}
	return security.NewRedactor(security.RedactionVariants(credentials...)...)
}

func newOptionalServices(cfg *config.Config) (contacts.Service, maildomain.Service, error) {
	capabilities := cfg.Capabilities()
	var contactsService contacts.Service
	if capabilities.ContactsRead || capabilities.ContactsWrite || capabilities.ContactsDelete {
		contactsCredentials := security.CredentialPair{
			Username: strings.Clone(cfg.Email),
			Password: strings.Clone(cfg.Password),
		}
		contactsHTTP := security.NewContactsHTTPClient(cfg.Timeout)
		contactsAuth := webdav.HTTPClientWithBasicAuth(
			contactsHTTP,
			contactsCredentials.Username,
			contactsCredentials.Password,
		)
		contactsService = contacts.NewClient(contactsAuth, security.ContactsBaseURL, security.IsContactsHost)
	}

	var mailService maildomain.Service
	if capabilities.MailRead || capabilities.MailMutate || capabilities.MailTrash || capabilities.MailSend {
		// Use the boot-validated recipient list only; do not re-parse the raw env.
		var recipientPolicy maildomain.RecipientPolicy
		var err error
		if cfg.EffectiveMailSend() {
			recipientPolicy, err = maildomain.RecipientPolicyFromExact(cfg.SMTPAllowedRecipients)
			if err != nil {
				return nil, nil, fmt.Errorf("mail recipient policy initialization failed: %w", err)
			}
		}

		imapDial := func(ctx context.Context) (net.Conn, error) {
			return security.DialIMAPContext(ctx, "tcp", security.IMAPAddress)
		}
		var smtpDial maildomain.SMTPDialFunc
		if cfg.EffectiveMailSend() {
			smtpDial = func(ctx context.Context) (net.Conn, error) {
				return security.DialSMTPContext(ctx, "tcp", security.SMTPAddress)
			}
		}
		mailService, err = maildomain.NewService(maildomain.Config{
			Address:         strings.Clone(cfg.MailAddress),
			Password:        strings.Clone(cfg.MailPassword),
			RecipientPolicy: recipientPolicy,
		}, imapDial, smtpDial, cfg.EffectiveMailWrite(), cfg.EffectiveMailSend())
		if err != nil {
			return nil, nil, fmt.Errorf("mail service initialization failed: %w", err)
		}
	}
	return contactsService, mailService, nil
}
