package mcptools

import (
	"context"
	"fmt"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestUpdateEventHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
		"title":    "Nouveau titre",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if res.IsError {
		t.Fatalf("résultat d'erreur inattendu : %s", resultText(t, res))
	}
	if svc.LastUpdate == nil || svc.LastUpdate.Title == nil || *svc.LastUpdate.Title != "Nouveau titre" {
		t.Fatalf("LastUpdate = %+v", svc.LastUpdate)
	}
}

func TestUpdateEventHandler_DistinguishesAbsentFromEmpty(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	_, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
		"location": "", // fourni et vide → effacement
		// notes absent → inchangé
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	up := svc.LastUpdate
	if up == nil {
		t.Fatal("UpdateEvent non appelé")
	}
	if up.Location == nil || *up.Location != "" {
		t.Errorf("Location = %+v, want pointeur vers chaîne vide (effacement)", up.Location)
	}
	if up.Notes != nil {
		t.Errorf("Notes = %+v, want nil (absent = inchangé)", up.Notes)
	}
	if up.Title != nil {
		t.Errorf("Title = %+v, want nil (absent = inchangé)", up.Title)
	}
}

func TestUpdateEventHandler_NoFieldsProvided(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid":      "uid-1",
		"calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : erreur quand aucun champ n'est fourni")
	}
	if svc.UpdateCallCount != 0 {
		t.Errorf("UpdateEvent n'aurait pas dû être appelé")
	}
}

func TestUpdateEventHandler_StartAfterEndRejected(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/",
		"start": "2026-07-01T12:00:00Z",
		"end":   "2026-07-01T11:00:00Z",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : erreur start >= end")
	}
	if svc.UpdateCallCount != 0 {
		t.Errorf("UpdateEvent n'aurait pas dû être appelé")
	}
}

func TestUpdateEventHandler_InvalidUID(t *testing.T) {
	svc := &icloud.MockService{}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "../../etc/passwd", "calendar": "/cal/home/", "title": "x",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : erreur UID invalide")
	}
}

func TestUpdateEventHandler_ServiceError(t *testing.T) {
	svc := &icloud.MockService{UpdateErr: fmt.Errorf("événement introuvable (uid=uid-1)")}
	handler := updateEventHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"uid": "uid-1", "calendar": "/cal/home/", "title": "x",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : résultat d'erreur")
	}
}
