package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
)

func TestSetupProfilesDisabledOptionalConstruction(t *testing.T) {
	for _, name := range []string{"calendar-read-only", "calendar-contacts-read-only", "mail-read-only", "calendar-scoped-write", "mail-exact-send"} {
		t.Run(name, func(t *testing.T) {
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(key, "ICLOUD_") {
					t.Setenv(key, "")
				}
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			data, err := os.ReadFile(filepath.Join("..", "..", "examples", "profiles", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var host struct {
				Servers map[string]struct {
					Env map[string]string `json:"env"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &host); err != nil {
				t.Fatal(err)
			}
			for key, value := range host.Servers["icloud"].Env {
				if strings.HasPrefix(value, "file://") {
					file := filepath.Join(home, key)
					synthetic := "synthetic-profile-password"
					if key == "ICLOUD_EMAIL" || key == "ICLOUD_MAIL_ADDRESS" {
						synthetic = "profile@example.invalid"
					}
					if err := os.WriteFile(file, []byte(synthetic), 0o600); err != nil {
						t.Fatal(err)
					}
					value = "file://" + file
				}
				t.Setenv(key, value)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			people, mail, err := newOptionalServices(cfg)
			if err != nil {
				t.Fatal(err)
			}
			set := cfg.Capabilities()
			if (people != nil) != set.ContactsRead || (mail != nil) != (set.MailRead || set.MailSend) {
				t.Fatal("production factory constructed a disabled optional backend")
			}
			if strings.HasPrefix(name, "mail-") && (cfg.Email != "" || cfg.Password != "") {
				t.Fatal("Mail-only startup loaded unused Calendar credentials")
			}
			// The disabled doctor branch must return before its service factory.
			for _, domain := range []string{"calendar", "contacts", "mail"} {
				enabled := map[string]bool{"calendar": set.CalendarRead || set.CalendarWrite, "contacts": set.ContactsRead, "mail": set.MailRead || set.MailSend}[domain]
				if !enabled {
					result := doctorProbeWithServices(cfg, domain, nil)
					if result.Status != "disabled" {
						t.Fatalf("disabled doctor domain %s: %+v", domain, result)
					}
				}
			}
			t.Logf("profile=%s Calendar_credentials_loaded=%t Contacts_constructed=%t Mail_constructed=%t", name, cfg.Email != "", people != nil, mail != nil)
		})
	}
}
