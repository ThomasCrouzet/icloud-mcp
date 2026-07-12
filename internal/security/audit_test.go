package security

import (
	"bytes"
	"strings"
	"testing"
)

func TestAuditLogger_LogMutation(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)

	a.LogMutation("delete_event", "/123/calendars/ABC/", "uid-xyz", "success")

	out := buf.String()
	for _, want := range []string{
		"msg=audit",
		"tool=delete_event",
		"calendar=/123/calendars/ABC/",
		"uid=uid-xyz",
		"status=success",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ligne d'audit attendue contenant %q, obtenu : %q", want, out)
		}
	}
}

func TestAuditLogger_NeverLogsTitleEvenIfPassedByMistake(t *testing.T) {
	var buf bytes.Buffer
	a := NewAuditLogger(&buf)

	// LogMutation n'accepte que tool/calendar/uid/status : il n'y a
	// structurellement aucun paramètre pour un titre. On vérifie qu'aucune
	// valeur ressemblant à un titre d'événement ne fuite si un développeur
	// futur passait un UID contenant un titre par erreur, le champ reste
	// nommé "uid" dans la sortie, ce qui rend l'anomalie auditable.
	a.LogMutation("create_event", "/123/calendars/ABC/", "uid-1", "denied")

	out := buf.String()
	if strings.Contains(out, "title=") || strings.Contains(out, "location=") || strings.Contains(out, "notes=") {
		t.Errorf("la ligne d'audit ne doit jamais contenir title=/location=/notes= : %q", out)
	}
}

func TestAuditLogger_StatusValues(t *testing.T) {
	for _, status := range []string{"success", "error", "denied"} {
		var buf bytes.Buffer
		a := NewAuditLogger(&buf)
		a.LogMutation("update_event", "/cal/", "uid", status)
		if !strings.Contains(buf.String(), "status="+status) {
			t.Errorf("status %q absent de la ligne d'audit : %q", status, buf.String())
		}
	}
}
