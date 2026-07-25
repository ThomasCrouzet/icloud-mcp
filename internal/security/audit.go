package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

var auditTokenKey = newAuditTokenKey()

// AuditFormat selects the stderr mutation audit encoding.
type AuditFormat string

const (
	// AuditFormatJSON emits one slog JSON object per mutation (default).
	AuditFormatJSON AuditFormat = "json"
	// AuditFormatText emits one plain key=value line per mutation.
	AuditFormatText AuditFormat = "text"
)

// ParseAuditFormat accepts json or text (case-insensitive). Empty defaults to json.
func ParseAuditFormat(value string) (AuditFormat, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "json":
		return AuditFormatJSON, nil
	case "text":
		return AuditFormatText, nil
	default:
		return "", fmt.Errorf("audit format must be json or text")
	}
}

// AuditLogger logs unified mutation records. Resource identifiers are
// represented only by process-local opaque tokens; untrusted labels and
// statuses are sanitized before they are emitted.
type AuditLogger struct {
	format AuditFormat
	logger *slog.Logger
	w      io.Writer
}

// NewAuditLogger builds a JSON AuditLogger writing to w.
func NewAuditLogger(w io.Writer) *AuditLogger {
	return NewAuditLoggerWithFormat(w, AuditFormatJSON)
}

// NewAuditLoggerWithFormat builds an AuditLogger with the chosen format.
// Production uses the stderr RedactingWriter as defense in depth. The level is
// pinned to Info so mutation events are emitted regardless of the server log
// level.
func NewAuditLoggerWithFormat(w io.Writer, format AuditFormat) *AuditLogger {
	if format == "" {
		format = AuditFormatJSON
	}
	a := &AuditLogger{format: format, w: w}
	if format == AuditFormatJSON {
		a.logger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return a
}

// LogDomainMutation logs a domain mutation without emitting the raw resource
// value. The resource token is stable within one process and unlinkable across
// process restarts because its HMAC key is generated at process initialization.
func (a *AuditLogger) LogDomainMutation(tool, domain, resourceType, resource, status string) {
	if a == nil {
		return
	}
	tool = auditLabel(tool)
	domain = auditLabel(domain)
	resourceType = auditLabel(resourceType)
	status = auditStatus(status)
	token := resourceToken(domain, resourceType, resource)
	if a.format == AuditFormatText {
		line := fmt.Sprintf(
			"timestamp=%s level=INFO msg=audit tool=%s domain=%s resourceType=%s resourceToken=%s status=%s\n",
			time.Now().UTC().Format(time.RFC3339),
			tool, domain, resourceType, token, status,
		)
		_, _ = io.WriteString(a.w, line)
		return
	}
	a.logger.Info("audit",
		"tool", tool,
		"domain", domain,
		"resourceType", resourceType,
		"resourceToken", token,
		"status", status,
	)
}

func newAuditTokenKey() [sha256.Size]byte {
	var key [sha256.Size]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("security: audit token key generation failed")
	}
	return key
}

func resourceToken(domain, resourceType, resource string) string {
	mac := hmac.New(sha256.New, auditTokenKey[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(resourceType))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(resource))
	return hex.EncodeToString(mac.Sum(nil))
}

func auditLabel(value string) string {
	if value == "" || len(value) > 64 {
		return "unknown"
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "unknown"
		}
	}
	return value
}

func auditStatus(status string) string {
	switch status {
	case "success", "error", "denied", "dry_run", "outcome_unknown":
		return status
	default:
		return "error"
	}
}
