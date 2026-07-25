package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
)

var auditTokenKey = newAuditTokenKey()

// AuditLogger logs unified mutation records as structured NDJSON. Resource
// identifiers are represented only by process-local opaque tokens; untrusted
// labels and statuses are sanitized before they are emitted.
type AuditLogger struct {
	logger *slog.Logger
}

// NewAuditLogger builds an AuditLogger writing to w. Production uses the
// stderr RedactingWriter as defense in depth. The level is pinned to Info so
// mutation events are emitted regardless of the server log level.
func NewAuditLogger(w io.Writer) *AuditLogger {
	return &AuditLogger{
		logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// LogDomainMutation logs a domain mutation without emitting the raw resource
// value. The resource token is stable within one process and unlinkable across
// process restarts because its HMAC key is generated at process initialization.
func (a *AuditLogger) LogDomainMutation(tool, domain, resourceType, resource, status string) {
	a.logger.Info("audit",
		"tool", auditLabel(tool),
		"domain", auditLabel(domain),
		"resourceType", auditLabel(resourceType),
		"resourceToken", resourceToken(domain, resourceType, resource),
		"status", auditStatus(status),
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
