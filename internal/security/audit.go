package security

import (
	"io"
	"log/slog"
)

// AuditLogger logs mutations (create_event/update_event/delete_event) to
// stderr as STRUCTURED JSON: timestamp (added by slog), level, msg="audit",
// tool, calendar, UID, status. NEVER any title, location or notes; no PII in
// the logs. One JSON object per line (NDJSON), easy to ship to a log indexer.
type AuditLogger struct {
	logger *slog.Logger
}

// NewAuditLogger builds an AuditLogger writing to w (the stderr
// RedactingWriter in production, so that even an accidental leak of a UID or
// a path containing a secret is covered). The format is JSON so the audit
// trail is structured and machine-parseable; the level is pinned to Info so
// mutation events are always emitted regardless of the server log level.
func NewAuditLogger(w io.Writer) *AuditLogger {
	return &AuditLogger{
		logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})),
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
