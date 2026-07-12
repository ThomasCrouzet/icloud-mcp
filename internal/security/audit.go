package security

import (
	"io"
	"log/slog"
)

// AuditLogger logs mutations (create_event/update_event/delete_event) to
// stderr: timestamp (added by slog), tool, calendar, UID, status.
// NEVER any title, location or notes; no PII in the logs.
type AuditLogger struct {
	logger *slog.Logger
}

// NewAuditLogger builds an AuditLogger writing to w (the stderr
// RedactingWriter in production, so that even an accidental leak of a UID or
// a path containing a secret is covered).
func NewAuditLogger(w io.Writer) *AuditLogger {
	return &AuditLogger{
		logger: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// LogMutation logs a mutation. status is one of "success", "error", "denied"
// ("denied" = input validation refused before any network call).
func (a *AuditLogger) LogMutation(tool, calendarPath, uid, status string) {
	a.logger.Info("audit",
		"tool", tool,
		"calendar", calendarPath,
		"uid", uid,
		"status", status,
	)
}
