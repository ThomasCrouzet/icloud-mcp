// Package config handles the icloud-mcp server configuration: reading
// environment variables, resolving file:// secrets, and validating at boot.
// No external dependency (no godotenv; the env comes from the MCP host that
// launches this binary as a stdio child process).
package config

import (
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"strings"
	"time"
)

// icloudTimeout is the fixed HTTP timeout for all CalDAV requests.
// Frozen by the spec (no dedicated environment variable).
const icloudTimeout = 30 * time.Second

// defaultTZEnvVar names the environment variable used to resolve
// DefaultLocation. See Config.DefaultLocation for why it exists.
const defaultTZEnvVar = "ICLOUD_MCP_DEFAULT_TZ"

// Config holds the configuration validated at startup.
type Config struct {
	Email      string        // ICLOUD_EMAIL (file:// supported)
	Password   string        // ICLOUD_PASSWORD (file:// supported), NEVER log it
	ReadOnly   bool          // ICLOUD_MCP_READ_ONLY=1
	HealthAddr string        // -health flag (e.g. "127.0.0.1:8797"), "" = off
	Timeout    time.Duration // 30s constant
	LogLevel   slog.Level    // ICLOUD_MCP_LOG_LEVEL (debug/info/warn/error), default info

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

	cfg := &Config{
		Email:           email,
		Password:        password,
		ReadOnly:        parseBool(os.Getenv("ICLOUD_MCP_READ_ONLY")),
		Timeout:         icloudTimeout,
		LogLevel:        parseLogLevel(os.Getenv("ICLOUD_MCP_LOG_LEVEL")),
		DefaultLocation: loc,
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
		return nil, fmt.Errorf("invalid %s (%q): %w", defaultTZEnvVar, tz, err)
	}
	return loc, nil
}

// Validate checks the email format and the minimum password length. Error
// messages NEVER contain the password or the email (not even an excerpt):
// boot failures are logged before the production Redactor is installed, so
// every config error string must be free of credentials and account identity.
func (c *Config) Validate() error {
	if _, err := mail.ParseAddress(c.Email); err != nil {
		return fmt.Errorf("invalid ICLOUD_EMAIL: must be a valid email address")
	}
	if len(c.Password) < 8 {
		return fmt.Errorf("ICLOUD_PASSWORD must be at least 8 characters: use an app-specific password generated on appleid.apple.com")
	}
	return nil
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
		data, err := os.ReadFile(path) // #nosec G304 -- path controlled by the operator via env
		if err != nil {
			// Do not wrap the OS error: it embeds the path, and boot logging
			// happens before the Redactor is ready. Reason codes only.
			return "", fmt.Errorf("reading %s from file:// failed (%s)", envVar, fileReadReason(err))
		}
		return strings.TrimSpace(string(data)), nil
	}
	return val, nil
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
	case os.IsNotExist(err):
		return "not_found"
	case os.IsPermission(err):
		return "permission_denied"
	default:
		return "unreadable"
	}
}

// parseBool interprets "1" and "true" (case-insensitive) as true;
// anything else (including unset) as false.
func parseBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true"
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
