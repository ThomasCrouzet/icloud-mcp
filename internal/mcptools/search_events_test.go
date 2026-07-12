package mcptools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestSearchEventsHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{Events: []icloud.Event{
		{UID: "1", Title: "Réunion", StartTime: time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)},
		{UID: "2", Title: "Déjeuner", StartTime: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)},
	}}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start":    "2026-07-01T00:00:00Z",
		"end":      "2026-07-08T00:00:00Z",
		"calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if res.IsError {
		t.Fatalf("résultat d'erreur inattendu : %s", resultText(t, res))
	}

	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 2 || payload.Count != 2 {
		t.Fatalf("payload = %+v", payload)
	}
	// Tri par StartTime : Déjeuner (07-01) avant Réunion (07-02).
	if payload.Events[0].Title != "Déjeuner" || payload.Events[1].Title != "Réunion" {
		t.Errorf("ordre inattendu : %+v", payload.Events)
	}
	if svc.LastSearchPath != "/cal/home/" {
		t.Errorf("LastSearchPath = %q", svc.LastSearchPath)
	}
}

func TestSearchEventsHandler_MissingCalendarSearchesAll(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		Events:    []icloud.Event{{UID: "1", Title: "x", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)}},
	}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z",
		"end":   "2026-07-08T00:00:00Z",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if res.IsError {
		t.Fatalf("résultat d'erreur inattendu : %s", resultText(t, res))
	}
	if svc.ListCallCount != 1 {
		t.Errorf("ListCallCount = %d, want 1", svc.ListCallCount)
	}
	if svc.SearchCallCount != 2 {
		t.Errorf("SearchCallCount = %d, want 2 (une recherche par calendrier)", svc.SearchCallCount)
	}
}

func TestSearchEventsHandler_QueryFilter(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/home/"}},
		Events: []icloud.Event{
			{UID: "1", Title: "Réunion équipe", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)},
			{UID: "2", Title: "Dentiste", Location: "Cabinet médical", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)},
		},
	}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z",
		"end":   "2026-07-08T00:00:00Z",
		"query": "RÉUNION",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 1 || payload.Events[0].Title != "Réunion équipe" {
		t.Fatalf("filtre query inefficace : %+v", payload)
	}
}

func TestSearchEventsHandler_Pagination(t *testing.T) {
	events := make([]icloud.Event, 450)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range events {
		events[i] = icloud.Event{
			UID:       fmt.Sprintf("uid-%d", i),
			Title:     fmt.Sprintf("Event %d", i),
			StartTime: base.Add(time.Duration(i) * time.Hour),
			EndTime:   base.Add(time.Duration(i)*time.Hour + 30*time.Minute),
		}
	}
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/home/"}}, Events: events}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-01-01T00:00:00Z",
		"end":   "2026-12-31T00:00:00Z",
		"limit": float64(400),
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if res.IsError {
		t.Fatalf("résultat d'erreur inattendu : %s", resultText(t, res))
	}

	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 450 {
		t.Errorf("Total = %d, want 450", payload.Total)
	}
	if len(payload.Events) != 400 {
		t.Errorf("len(Events) = %d, want 400 (borne dure)", len(payload.Events))
	}
	if !payload.Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestSearchEventsHandler_InvalidDates(t *testing.T) {
	svc := &icloud.MockService{}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "pas une date",
		"end":   "2026-07-08T00:00:00Z",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : résultat d'erreur pour une date invalide")
	}
}

func TestSearchEventsHandler_RangeTooLarge(t *testing.T) {
	svc := &icloud.MockService{}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-01-01T00:00:00Z",
		"end":   "2028-01-01T00:00:00Z",
	}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : résultat d'erreur pour une plage > 366 jours")
	}
	if !strings.Contains(resultText(t, res), "366") {
		t.Errorf("message d'erreur attendu mentionnant 366 jours : %s", resultText(t, res))
	}
}

func TestSearchEventsHandler_MissingRequiredParams(t *testing.T) {
	svc := &icloud.MockService{}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{"end": "2026-07-08T00:00:00Z"}))
	if err != nil {
		t.Fatalf("erreur protocole inattendue : %v", err)
	}
	if !res.IsError {
		t.Fatal("attendu : erreur pour start manquant")
	}
}
