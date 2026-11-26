package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func draftsTestConfig(t *testing.T, permissions, scopes string, recipients []string) *config.Config {
	t.Helper()
	cfg := config.Config{
		Email: "calendar-user@example.com", Password: "calendar-app-secret",
		MailAddress: "mail-user@icloud.com", MailPassword: "mail-app-secret",
		EnableMail: true, Timeout: time.Second, DraftsMailbox: "Drafts",
		SMTPAllowedRecipients: recipients,
	}
	var err error
	if cfg.Permissions, err = config.ParsePermissions(permissions); err != nil {
		t.Fatal(err)
	}
	if cfg.ResourceScopes, err = config.ParseResourceScopes(scopes); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func TestMailServiceConfigAppliesAllowlistToDrafts(t *testing.T) {
	// A drafts-only profile with an allowlist gets that allowlist, although
	// SMTP send stays off.
	cfg := draftsTestConfig(t, "mail.read,mail.drafts", "", []string{"allowed@example.com"})
	got, err := mailServiceConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := maildomain.RecipientPolicyFromExact([]string{"allowed@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.EnableDrafts || cfg.EffectiveMailSend() || !reflect.DeepEqual(got.RecipientPolicy, want) {
		t.Fatalf("drafts config = drafts:%v send:%v policy:%+v", got.EnableDrafts, cfg.EffectiveMailSend(), got.RecipientPolicy)
	}
	// Without an allowlist, drafts keep an unset policy.
	got, err = mailServiceConfig(draftsTestConfig(t, "mail.read,mail.drafts", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RecipientPolicy, maildomain.RecipientPolicy{}) {
		t.Fatalf("drafts without an allowlist have a policy: %+v", got.RecipientPolicy)
	}
}
