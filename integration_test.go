//go:build integration

// Optional integration test against the real iCloud. Skipped by default
// (build tag `integration`), never run in CI. Local usage:
//
//	ICLOUD_EMAIL=... ICLOUD_PASSWORD=... go test -tags integration -count=1 -v -timeout=120s .
//
// Prefer file:// secrets (Docker-secret layout):
//
//	ICLOUD_EMAIL=file:///path/to/email ICLOUD_PASSWORD=file:///path/to/app-password \
//	  go test -tags integration -count=1 -v -timeout=120s .
package icloud_mcp_integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-webdav"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func loadIntegrationCreds(t *testing.T) (email, password string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		email = os.Getenv("ICLOUD_EMAIL")
		password = os.Getenv("ICLOUD_PASSWORD")
		if email == "" || password == "" || strings.HasPrefix(email, "file://") {
			t.Skipf("ICLOUD_EMAIL / ICLOUD_PASSWORD not set or invalid (%v)", err)
		}
		return email, password
	}
	return cfg.Email, cfg.Password
}

func newIntegrationClient(t *testing.T) *icloud.Client {
	t.Helper()
	email, password := loadIntegrationCreds(t)
	httpClient := security.NewICloudHTTPClient(30 * time.Second)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, email, password)
	doer := icloud.NewRetryClassifier(authHTTP)
	return icloud.NewClient(doer, security.ICloudBaseURL, security.IsICloudHost)
}

func TestIntegration_DiscoverListSearchGet(t *testing.T) {
	client := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := client.Discover(ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cals, err := client.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) == 0 {
		t.Fatal("no calendars returned, unexpected for a real iCloud account")
	}
	t.Logf("%d calendar(s) found", len(cals))
	for _, c := range cals {
		if c.Path == "" || !strings.HasPrefix(c.Path, "/") {
			t.Errorf("calendar path not path-absolute: %+v", c)
		}
		if strings.Contains(c.Path, "://") {
			t.Errorf("calendar path must not be absolute URL: %q", c.Path)
		}
	}

	start := time.Now().UTC().Truncate(time.Hour)
	end := start.AddDate(0, 0, 7)
	res, err := client.SearchEvents(ctx, cals[0].Path, start, end, nil)
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	t.Logf("%d event(s) over the next 7 days in %q (truncatedByExpansion=%v)",
		len(res.Events), cals[0].Name, res.TruncatedByExpansion)

	if len(res.Events) > 0 {
		uid := res.Events[0].UID
		detail, gerr := client.GetEvent(ctx, cals[0].Path, uid)
		if gerr != nil {
			t.Fatalf("GetEvent(%s): %v", uid, gerr)
		}
		if detail.UID != uid {
			t.Errorf("GetEvent UID = %q, want %q", detail.UID, uid)
		}
		t.Logf("GetEvent ok uid=%s etag_set=%v isRecurring=%v overrides=%d",
			detail.UID, detail.ETag != "", detail.IsRecurring, detail.OverrideCount)
	}
}

func TestIntegration_ValidateAndFreeSlotsLocal(t *testing.T) {
	start := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	res := icloud.ValidateEventInput(&icloud.EventInput{
		Title:     "integration-validate",
		StartTime: start,
		EndTime:   end,
		Status:    "CONFIRMED",
	}, time.UTC)
	if !res.OK {
		t.Fatalf("ValidateEventInput: %+v", res.Errors)
	}
	busy := []icloud.Interval{{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour)}}
	slots, err := icloud.FindFreeSlots(busy, icloud.FreeSlotOptions{
		RangeStart: start,
		RangeEnd:   start.Add(8 * time.Hour),
		Duration:   time.Hour,
	})
	if err != nil {
		t.Fatalf("FindFreeSlots: %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one free slot")
	}
}
