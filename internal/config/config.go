// Package config handles the icloud-mcp server configuration: reading
// environment variables, resolving file:// secrets, and validating at boot.
// No external dependency (no godotenv; the env comes from the MCP host that
// launches this binary as a stdio child process).
package config

import (
	"fmt"
	"net/mail"
	"os"
	"strings"
	"time"
)

// icloudTimeout is the fixed HTTP timeout for all CalDAV requests.
// Frozen by the spec (no dedicated environment variable).
const icloudTimeout = 30 * time.Second

// Config holds the configuration validated at startup.
type Config struct {
	Email      string        // ICLOUD_EMAIL (file:// supported)
	Password   string        // ICLOUD_PASSWORD (file:// supported), NEVER log it
	ReadOnly   bool          // ICLOUD_MCP_READ_ONLY=1
	HealthAddr string        // -health flag (e.g. "127.0.0.1:8797"), "" = off
	Timeout    time.Duration // 30s constant
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

	cfg := &Config{
		Email:    email,
		Password: password,
		ReadOnly: parseBool(os.Getenv("ICLOUD_MCP_READ_ONLY")),
		Timeout:  icloudTimeout,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the email format and the minimum password length. Error
// messages NEVER contain the password (not even an excerpt); the email is
// tolerated in the format error message.
func (c *Config) Validate() error {
	if _, err := mail.ParseAddress(c.Email); err != nil {
		return fmt.Errorf("invalid ICLOUD_EMAIL (%q): %w", c.Email, err)
	}
	if len(c.Password) < 8 {
		return fmt.Errorf("ICLOUD_PASSWORD must be at least 8 characters: use an app-specific password generated on appleid.apple.com")
	}
	return nil
}

// loadCredential reads an environment variable. If its value starts with
// "file://", the secret is read from the referenced file (Docker secrets
// pattern); this is the ONLY disk read the program is allowed to perform.
func loadCredential(envVar string) (string, error) {
	val := os.Getenv(envVar)
	if strings.HasPrefix(val, "file://") {
		path := strings.TrimPrefix(val, "file://")
		data, err := os.ReadFile(path) // #nosec G304 -- path controlled by the operator via env
		if err != nil {
			return "", fmt.Errorf("reading %s from file %s: %w", envVar, path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return val, nil
}

// parseBool interprets "1" and "true" (case-insensitive) as true;
// anything else (including unset) as false.
func parseBool(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true"
}
