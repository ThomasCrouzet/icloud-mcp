//go:build integration

// Optional integration test against the real iCloud. Skipped by default
// (build tag `integration`), never run in CI. Local usage:
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
		t.Skip("ICLOUD_EMAIL / ICLOUD_PASSWORD not set, integration test skipped")
	}

	httpClient := security.NewICloudHTTPClient(30 * time.Second)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, email, password)
	client := icloud.NewClient(authHTTP, security.ICloudBaseURL, security.IsICloudHost)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cals, err := client.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) == 0 {
		t.Fatal("no calendars returned, unexpected for a real iCloud account")
	}
	t.Logf("%d calendar(s) found", len(cals))

	start := time.Now()
	end := start.AddDate(0, 0, 7)
	res, err := client.SearchEvents(ctx, cals[0].Path, start, end, nil)
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	events := res.Events
	t.Logf("%d event(s) over the next 7 days in %q", len(events), cals[0].Name)
}
