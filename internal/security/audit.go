package security

import (
	"io"
	"log/slog"
)

// AuditLogger journalise les mutations (create_event/update_event/delete_event)
// sur stderr : timestamp (ajouté par slog), tool, calendrier, UID, statut.
// JAMAIS de titre, lieu ou notes, pas de PII dans les logs.
type AuditLogger struct {
	logger *slog.Logger
}

// NewAuditLogger construit un AuditLogger écrivant sur w (le RedactingWriter
// de stderr en production, pour que même une fuite accidentelle d'UID ou de
// path contenant un secret soit couverte).
func NewAuditLogger(w io.Writer) *AuditLogger {
	return &AuditLogger{
		logger: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// LogMutation journalise une mutation. status ∈ {"success", "error", "denied"}
// ("denied" = validation d'entrée refusée avant tout appel réseau).
func (a *AuditLogger) LogMutation(tool, calendarPath, uid, status string) {
	a.logger.Info("audit",
		"tool", tool,
		"calendar", calendarPath,
		"uid", uid,
		"status", status,
	)
}
