package mcptools

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestDeleteEventHandler_EchoesDeletedTitle(t *testing.T) {
	svc := &icloud.MockService{DeletedTitle: "Rendez-vous dentiste"}
	handler := deleteEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if res.IsError {
		t.Fatalf("résultat d'erreur inattendu : %s", resultText(t, res))
	}

	var payload deleteEventResponse
	decodeResult(t, res, &payload)
	if !payload.Success || payload.DeletedTitle != "Rendez-vous dentiste" {
		t.Fatalf("payload = %+v", payload)
	}
	if svc.LastDeleteUID != "uid-1" {
		t.Errorf("LastDeleteUID = %q", svc.LastDeleteUID)
	}
}

func TestDeleteEventHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{DeleteErr: fmt.Errorf("événement introuvable")}
	handler := deleteEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-inconnu", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : résultat d'erreur")
	}
}

func TestDeleteEventHandler_AuditNeverContainsTitle(t *testing.T) {
	svc := &icloud.MockService{DeletedTitle: "Titre très privé"}
	var buf bytes.Buffer
	deps := Deps{
		Service:  svc,
		Audit:    security.NewAuditLogger(&buf),
		Redactor: security.NewRedactor("unused-secret-value"),
	}
	handler := deleteEventHandler(deps)

	_, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}

	logLine := buf.String()
	if !strings.Contains(logLine, "tool=delete_event") || !strings.Contains(logLine, "status=success") || !strings.Contains(logLine, "uid=uid-1") {
		t.Errorf("ligne d'audit inattendue : %s", logLine)
	}
	if strings.Contains(logLine, "Titre très privé") {
		t.Fatalf("la ligne d'audit ne doit JAMAIS contenir le titre supprimé : %s", logLine)
	}
}

func TestDeleteEventHandler_InvalidCalendarPath(t *testing.T) {
	svc := &icloud.MockService{}
	handler := deleteEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "chemin-invalide",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : erreur calendar path invalide")
	}
	if svc.DeleteCallCount != 0 {
		t.Errorf("DeleteEvent n'aurait pas dû être appelé (validation refusée)")
	}
}
