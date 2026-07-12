//go:build integration

// Test d'intégration optionnel contre le vrai iCloud. Skippé par défaut
// (build tag `integration`), jamais exécuté en CI. Usage local :
//
//	ICLOUD_EMAIL=... ICLOUD_PASSWORD=... go test -tags integration ./...
package icloud_mcp_integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-webdav"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestIntegration_ListAndSearchRealICloud(t *testing.T) {
	email := os.Getenv("ICLOUD_EMAIL")
	password := os.Getenv("ICLOUD_PASSWORD")
	if email == "" || password == "" {
		t.Skip("ICLOUD_EMAIL / ICLOUD_PASSWORD absents, test d'intégration ignoré")
	}

	httpClient := security.NewICloudHTTPClient(30 * time.Second)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, email, password)
	client := icloud.NewClient(authHTTP, security.ICloudBaseURL, security.IsICloudHost)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cals, err := client.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars : %v", err)
	}
	if len(cals) == 0 {
		t.Fatal("aucun calendrier retourné, inattendu pour un compte iCloud réel")
	}
	t.Logf("%d calendrier(s) trouvé(s)", len(cals))

	start := time.Now()
	end := start.AddDate(0, 0, 7)
	events, err := client.SearchEvents(ctx, cals[0].Path, start, end)
	if err != nil {
		t.Fatalf("SearchEvents : %v", err)
	}
	t.Logf("%d événement(s) sur les 7 prochains jours dans %q", len(events), cals[0].Name)
}
