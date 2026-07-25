// Package config handles the icloud-mcp server configuration: reading
// environment variables, resolving file:// secrets, and validating at boot.
// No external dependency (no godotenv; the env comes from the MCP host that
// launches this binary as a stdio child process).
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
)

// icloudTimeout is the fixed HTTP timeout for all CalDAV requests.
// Frozen by the spec (no dedicated environment variable).
const icloudTimeout = 30 * time.Second

const (
	maxCredentialFileBytes     = 4 << 10
	minRedactableIdentityBytes = 4
)

var (
	errCredentialFileNotRegular = errors.New("credential file is not a regular file")
	errCredentialFileTooLarge   = errors.New("credential file exceeds byte limit")
)

// defaultTZEnvVar names the environment variable used to resolve
// DefaultLocation. See Config.DefaultLocation for why it exists.
const defaultTZEnvVar = "ICLOUD_MCP_DEFAULT_TZ"

const (
	readOnlyEnvVar              = "ICLOUD_MCP_READ_ONLY"
	enableContactsEnvVar        = "ICLOUD_MCP_ENABLE_CONTACTS"
	enableMailEnvVar            = "ICLOUD_MCP_ENABLE_MAIL"
	enableMailWriteEnvVar       = "ICLOUD_MCP_ENABLE_MAIL_WRITE"
	enableMailSendEnvVar        = "ICLOUD_MCP_ENABLE_MAIL_SEND"
	mailAddressEnvVar           = "ICLOUD_MAIL_ADDRESS"
	mailPasswordEnvVar          = "ICLOUD_MAIL_PASSWORD"
	smtpAllowedRecipientsEnvVar = "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS"
)

// RecipientPolicy is an immutable exact-address SMTP recipient policy. Its
// zero value denies every recipient.
type RecipientPolicy struct {
	allowAll bool
	exact    map[string]struct{}
}

// ParseRecipientPolicy parses a non-empty recipient allowlist. A literal "*"
// permits every syntactically valid address; otherwise only exact plain
// addr-spec values are accepted.
func ParseRecipientPolicy(value string) (RecipientPolicy, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return RecipientPolicy{}, fmt.Errorf("%s must not be empty", smtpAllowedRecipientsEnvVar)
	}
	if value == "*" {
		return RecipientPolicy{allowAll: true}, nil
	}

	parts := strings.Split(value, ",")
	exact := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		if strings.Contains(part, "*") {
			return RecipientPolicy{}, fmt.Errorf("invalid %s: wildcard is valid only as the complete value", smtpAllowedRecipientsEnvVar)
		}
		address, err := normalizePlainAddress(part)
		if err != nil {
			return RecipientPolicy{}, fmt.Errorf("invalid %s: expected exact email addresses or literal *", smtpAllowedRecipientsEnvVar)
		}
		if _, duplicate := exact[address]; duplicate {
			return RecipientPolicy{}, fmt.Errorf("invalid %s: duplicate email address", smtpAllowedRecipientsEnvVar)
		}
		exact[address] = struct{}{}
	}
	return RecipientPolicy{exact: exact}, nil
}

// Allows reports whether address is permitted by this exact-address policy.
// Invalid addresses are always denied.
func (p RecipientPolicy) Allows(address string) bool {
	normalized, err := normalizePlainAddress(address)
	if err != nil {
		return false
	}
	if p.allowAll {
		return true
	}
	_, ok := p.exact[normalized]
	return ok
}

// AllowAll reports whether the complete configured policy was literal "*".
func (p RecipientPolicy) AllowAll() bool {
	return p.allowAll
}

// Configured reports whether the policy contains a wildcard or exact address.
func (p RecipientPolicy) Configured() bool {
	return p.allowAll || len(p.exact) != 0
}

// Recipients returns a sorted copy of the normalized policy representation.
// A wildcard policy is represented as []string{"*"}.
func (p RecipientPolicy) Recipients() []string {
	if p.allowAll {
		return []string{"*"}
	}
	recipients := make([]string, 0, len(p.exact))
	for address := range p.exact {
		recipients = append(recipients, address)
	}
	sort.Strings(recipients)
	return recipients
}

// Config holds the configuration validated at startup.
type Config struct {
	Email                 string          // ICLOUD_EMAIL (file:// supported)
	Password              string          // ICLOUD_PASSWORD (file:// supported), NEVER log it
	ReadOnly              bool            // ICLOUD_MCP_READ_ONLY
	EnableContacts        bool            // ICLOUD_MCP_ENABLE_CONTACTS
	EnableMail            bool            // ICLOUD_MCP_ENABLE_MAIL
	EnableMailWrite       bool            // ICLOUD_MCP_ENABLE_MAIL_WRITE, configured value
	EnableMailSend        bool            // ICLOUD_MCP_ENABLE_MAIL_SEND, configured value
	MailAddress           string          // ICLOUD_MAIL_ADDRESS (file:// supported)
	MailPassword          string          // ICLOUD_MAIL_PASSWORD (file:// supported), NEVER log it
	SMTPAllowedRecipients []string        // normalized exact addresses, or a single literal "*"
	SMTPRecipientPolicy   RecipientPolicy // immutable exact-match policy for SMTP authorization
	Timeout               time.Duration   // 30s constant
	LogLevel              slog.Level      // ICLOUD_MCP_LOG_LEVEL (debug/info/warn/error), default info

	// DefaultLocation is the timezone used to interpret a start/end value
	// supplied WITHOUT an explicit RFC3339 offset (e.g. "2026-07-01T14:00:00",
	// no "Z", no "+02:00"). Set via ICLOUD_MCP_DEFAULT_TZ (IANA name, e.g.
	// "Europe/Paris"); defaults to UTC if unset, which keeps the previous
	// strict behavior (a bare RFC3339 offset is still respected literally,
	// this only affects the offset-less fallback). See
	// internal/icloud.ParseDateTime for the parsing rules and the incident
	// that motivated this: an agent echoing a local hour back as "...Z"
	// (UTC) instead of converting it, shifting events by the local UTC
	// offset (2h during CEST).
	DefaultLocation *time.Location
}

// Load reads the configuration from the environment, resolves any file://
// prefixes and validates the result.
func Load() (*Config, error) {
	readOnly, err := parseBool(readOnlyEnvVar, os.Getenv(readOnlyEnvVar))
	if err != nil {
		return nil, err
	}
	enableContacts, err := parseBool(enableContactsEnvVar, os.Getenv(enableContactsEnvVar))
	if err != nil {
		return nil, err
	}
	enableMail, err := parseBool(enableMailEnvVar, os.Getenv(enableMailEnvVar))
	if err != nil {
		return nil, err
	}
	enableMailWrite, err := parseBool(enableMailWriteEnvVar, os.Getenv(enableMailWriteEnvVar))
	if err != nil {
		return nil, err
	}
	enableMailSend, err := parseBool(enableMailSendEnvVar, os.Getenv(enableMailSendEnvVar))
	if err != nil {
		return nil, err
	}

	email, err := loadCredential("ICLOUD_EMAIL")
	if err != nil {
		return nil, err
	}
	password, err := loadCredential("ICLOUD_PASSWORD")
	if err != nil {
		return nil, err
	}

	loc, err := loadDefaultLocation(os.Getenv(defaultTZEnvVar))
	if err != nil {
		return nil, err
	}

	var mailAddress, mailPassword string
	if enableMail {
		mailAddress, err = loadCredential(mailAddressEnvVar)
		if err != nil {
			return nil, err
		}
		if os.Getenv(mailPasswordEnvVar) == "" {
			mailPassword = password
		} else {
			mailPassword, err = loadCredential(mailPasswordEnvVar)
			if err != nil {
				return nil, err
			}
		}
	}

	var recipientPolicy RecipientPolicy
	if strings.TrimSpace(os.Getenv(smtpAllowedRecipientsEnvVar)) != "" {
		recipientPolicy, err = ParseRecipientPolicy(os.Getenv(smtpAllowedRecipientsEnvVar))
		if err != nil {
			return nil, err
		}
	}

	cfg := &Config{
		Email:                 email,
		Password:              password,
		ReadOnly:              readOnly,
		EnableContacts:        enableContacts,
		EnableMail:            enableMail,
		EnableMailWrite:       enableMailWrite,
		EnableMailSend:        enableMailSend,
		MailAddress:           mailAddress,
		MailPassword:          mailPassword,
		SMTPAllowedRecipients: recipientPolicy.Recipients(),
		SMTPRecipientPolicy:   recipientPolicy,
		Timeout:               icloudTimeout,
		LogLevel:              parseLogLevel(os.Getenv("ICLOUD_MCP_LOG_LEVEL")),
		DefaultLocation:       loc,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadDefaultLocation resolves ICLOUD_MCP_DEFAULT_TZ to a *time.Location,
// defaulting to UTC when unset. Failing fast here (before any network
// access, alongside the other config validation) is deliberate: a typo in
// the IANA name would otherwise surface much later as a silently wrong
// event time instead of a clear boot error.
func loadDefaultLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: must be a valid IANA timezone name", defaultTZEnvVar)
	}
	return loc, nil
}

// Validate checks credential formats and minimum lengths. Error
// messages NEVER contain the password or the email (not even an excerpt):
// boot failures are logged before the production Redactor is installed, so
// every config error string must be free of credentials and account identity.
func (c *Config) Validate() error {
	if len(c.Email) < minRedactableIdentityBytes || hasCredentialControl(c.Email) {
		return fmt.Errorf("invalid ICLOUD_EMAIL: must be a valid email address")
	}
	normalizedEmail, err := normalizePlainAddress(c.Email)
	if err != nil || len(normalizedEmail) < minRedactableIdentityBytes {
		return fmt.Errorf("invalid ICLOUD_EMAIL: must be a valid email address")
	}
	c.Email = normalizedEmail
	if hasCredentialControl(c.Password) {
		return fmt.Errorf("invalid ICLOUD_PASSWORD: must not contain CR, LF, or NUL")
	}
	if len(c.Password) < 8 {
		return fmt.Errorf("ICLOUD_PASSWORD must be at least 8 characters: use an app-specific password generated on appleid.apple.com")
	}
	if c.EnableMailWrite && !c.EnableMail {
		return fmt.Errorf("%s requires %s=true", enableMailWriteEnvVar, enableMailEnvVar)
	}
	if c.EnableMailSend && !c.EnableMail {
		return fmt.Errorf("%s requires %s=true", enableMailSendEnvVar, enableMailEnvVar)
	}
	if c.EnableMail {
		if hasCredentialControl(c.MailAddress) {
			return fmt.Errorf("invalid %s: must be a full email address", mailAddressEnvVar)
		}
		normalized, err := normalizePlainAddress(c.MailAddress)
		if err != nil || len(normalized) < minRedactableIdentityBytes {
			return fmt.Errorf("invalid %s: must be a full email address", mailAddressEnvVar)
		}
		c.MailAddress = normalized
		if c.MailPassword == "" {
			c.MailPassword = c.Password
		}
		if hasCredentialControl(c.MailPassword) {
			return fmt.Errorf("invalid %s: must not contain CR, LF, or NUL", mailPasswordEnvVar)
		}
		if len(c.MailPassword) < 8 {
			return fmt.Errorf("%s must be at least 8 characters", mailPasswordEnvVar)
		}
	}

	policy, err := recipientPolicyFromAddresses(c.SMTPAllowedRecipients)
	if err != nil {
		return err
	}
	c.SMTPRecipientPolicy = policy
	c.SMTPAllowedRecipients = policy.Recipients()
	if c.EnableMailSend && !policy.Configured() {
		return fmt.Errorf("%s is required when %s=true", smtpAllowedRecipientsEnvVar, enableMailSendEnvVar)
	}
	return nil
}

// EffectiveContactsWrite reports whether Contacts mutation tools may be
// registered after applying the global read-only switch.
func (c *Config) EffectiveContactsWrite() bool {
	return c.EnableContacts && !c.ReadOnly
}

// EffectiveMailWrite reports whether Mail mutation tools may be registered.
func (c *Config) EffectiveMailWrite() bool {
	return c.EnableMail && c.EnableMailWrite && !c.ReadOnly
}

// EffectiveMailSend reports whether SMTP submission may be registered.
func (c *Config) EffectiveMailSend() bool {
	return c.EnableMail && c.EnableMailSend && !c.ReadOnly && c.SMTPRecipientPolicy.Configured()
}

func recipientPolicyFromAddresses(addresses []string) (RecipientPolicy, error) {
	if len(addresses) == 0 {
		return RecipientPolicy{}, nil
	}
	return ParseRecipientPolicy(strings.Join(addresses, ","))
}

func normalizePlainAddress(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("invalid address")
	}
	value = strings.TrimSpace(value)
	if value == "" || !isASCII(value) {
		return "", fmt.Errorf("invalid address")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return "", fmt.Errorf("invalid address")
	}
	return asciiLower(value), nil
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiLower(value string) string {
	buf := []byte(value)
	for i, b := range buf {
		if b >= 'A' && b <= 'Z' {
			buf[i] = b + ('a' - 'A')
		}
	}
	return string(buf)
}

func hasCredentialControl(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

// loadCredential reads an environment variable. If its value starts with
// "file://", the secret is read from the referenced file (Docker secrets
// pattern); this is the ONLY disk read the program is allowed to perform.
// The path is fully trusted to the operator who set the env (no chroot): a
// process that can set ICLOUD_* can already read the same files. A path
// segment equal to ".." is rejected as a footgun guard, not as a security
// boundary (no chroot, no symlink resolution).
func loadCredential(envVar string) (string, error) {
	val := os.Getenv(envVar)
	if strings.HasPrefix(val, "file://") {
		path := strings.TrimPrefix(val, "file://")
		if path == "" {
			return "", fmt.Errorf("reading %s: file:// path is empty", envVar)
		}
		if hasDotDotSegment(path) {
			return "", fmt.Errorf("reading %s: file:// path must not contain '..'", envVar)
		}
		data, err := readCredentialFile(path)
		if err != nil {
			// Do not wrap the OS error: it embeds the path, and boot logging
			// happens before the Redactor is ready. Reason codes only.
			return "", fmt.Errorf("reading %s from file:// failed (%s)", envVar, fileReadReason(err))
		}
		return strings.TrimSpace(string(data)), nil
	}
	return val, nil
}

// readCredentialFile accepts only a small regular file. The checks around the
// nonblocking open prevent a path swap from turning the read into a FIFO or
// device wait while still allowing symlinks to regular secret files.
func readCredentialFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errCredentialFileNotRegular
	}
	if info.Size() > maxCredentialFileBytes {
		return nil, errCredentialFileTooLarge
	}

	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- operator-controlled boot-only secret path
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errCredentialFileNotRegular
	}
	if info.Size() > maxCredentialFileBytes {
		return nil, errCredentialFileTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCredentialFileBytes {
		return nil, errCredentialFileTooLarge
	}
	return data, nil
}

// hasDotDotSegment reports whether path has a ".." component (slash-separated).
// Substring matches like "app..pwd" are allowed; only a full segment is rejected.
func hasDotDotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// fileReadReason maps an OS read error to a stable, path-free reason code.
func fileReadReason(err error) string {
	switch {
	case errors.Is(err, errCredentialFileNotRegular):
		return "not_regular"
	case errors.Is(err, errCredentialFileTooLarge):
		return "too_large"
	case os.IsNotExist(err):
		return "not_found"
	case os.IsPermission(err):
		return "permission_denied"
	default:
		return "unreadable"
	}
}

// parseBool accepts only the strict boolean forms from the environment
// contract. The invalid value is deliberately omitted from the error.
func parseBool(envVar, value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("invalid %s: expected 0, false, 1, or true", envVar)
	}
}

// parseLogLevel maps ICLOUD_MCP_LOG_LEVEL (debug/info/warn/error, plus the
// case variants and numeric aliases) to a slog.Level. Unset or unrecognized =
// info (the production default: enough to see the start banner and audit
// mutations, no chatter).
func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug", "-4":
		return slog.LevelDebug
	case "warn", "warning", "2":
		return slog.LevelWarn
	case "error", "4":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
