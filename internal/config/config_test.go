package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ICLOUD_EMAIL",
		"ICLOUD_PASSWORD",
		"ICLOUD_MCP_READ_ONLY",
		"ICLOUD_MCP_LOG_LEVEL",
		"ICLOUD_MCP_DEFAULT_TZ",
		"ICLOUD_MCP_ENABLE_CONTACTS",
		"ICLOUD_MCP_ENABLE_MAIL",
		"ICLOUD_MAIL_ADDRESS",
		"ICLOUD_MAIL_PASSWORD",
		"ICLOUD_MCP_ENABLE_MAIL_WRITE",
		"ICLOUD_MCP_ENABLE_MAIL_SEND",
		"ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Email != "user@example.com" {
		t.Errorf("Email = %q", cfg.Email)
	}
	if cfg.Password != "app-specific-password" {
		t.Errorf("wrong Password")
	}
	if cfg.ReadOnly {
		t.Errorf("ReadOnly should default to false")
	}
	if cfg.Timeout.Seconds() != 30 {
		t.Errorf("Timeout = %v, want 30s", cfg.Timeout)
	}
	if cfg.DefaultLocation != time.UTC {
		t.Errorf("DefaultLocation = %v, want UTC when ICLOUD_MCP_DEFAULT_TZ is unset", cfg.DefaultLocation)
	}
}

func TestLoad_DefaultTZExplicit(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")
	t.Setenv("ICLOUD_MCP_DEFAULT_TZ", "Europe/Paris")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.DefaultLocation == nil || cfg.DefaultLocation.String() != "Europe/Paris" {
		t.Errorf("DefaultLocation = %v, want Europe/Paris", cfg.DefaultLocation)
	}
}

func TestLoad_DefaultTZInvalid(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")
	invalid := "/private/timezone-path-sentinel"
	t.Setenv("ICLOUD_MCP_DEFAULT_TZ", invalid)

	_, err := Load()
	if err == nil {
		t.Fatal("expected: invalid ICLOUD_MCP_DEFAULT_TZ error")
	}
	if !strings.Contains(err.Error(), "ICLOUD_MCP_DEFAULT_TZ") {
		t.Errorf("expected error mentioning ICLOUD_MCP_DEFAULT_TZ: %v", err)
	}
	if strings.Contains(err.Error(), invalid) || strings.Contains(err.Error(), "timezone-path-sentinel") {
		t.Errorf("invalid timezone or path leaked into config error: %v", err)
	}
}

func TestLoad_InvalidEmail(t *testing.T) {
	// Boot logging happens before the Redactor exists; Validate must never
	// put the account identity into the error string.
	clearEnv(t)
	bad := "not a valid address at all"
	t.Setenv("ICLOUD_EMAIL", bad)
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: invalid email error")
	}
	if !strings.Contains(err.Error(), "ICLOUD_EMAIL") {
		t.Errorf("expected error mentioning ICLOUD_EMAIL: %v", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("email value leaked into config error: %v", err)
	}
}

func TestLoad_PasswordTooShort(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "short")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: password too short error")
	}
	if strings.Contains(err.Error(), "short") {
		t.Errorf("the error message must never contain the password value: %v", err)
	}
}

func TestLoad_MissingEmail(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: missing email error")
	}
}

func TestLoad_ErrorNeverContainsPassword(t *testing.T) {
	sentinel := "SENTINEL-PW-abc123-XYZ"
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "not-a-valid-email")
	t.Setenv("ICLOUD_PASSWORD", sentinel)

	_, err := Load()
	if err == nil {
		t.Fatal("expected: error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("the sentinel password appears in the error: %v", err)
	}
	if strings.Contains(err.Error(), "not-a-valid-email") {
		t.Fatalf("the invalid email appears in the error: %v", err)
	}
}

func TestLoad_FileCredentialRejectsDotDot(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "file:///tmp/../etc/passwd")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: file:// path with '..' rejected")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("expected error about '..': %v", err)
	}
}

func TestHasDotDotSegment(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/tmp/../etc/passwd", true},
		{"../secret", true},
		{"/run/secrets/app..pwd", false},
		{"/run/secrets/icloud-password", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := hasDotDotSegment(tt.path); got != tt.want {
			t.Errorf("hasDotDotSegment(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestLoad_FileCredentialEmptyPath(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "file://")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: empty file:// path rejected")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-path error: %v", err)
	}
}

func TestLoad_FileCredentialMissingFileOmitsPath(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	missing := "/does/not/exist/app-password-sentinel"
	t.Setenv("ICLOUD_PASSWORD", "file://"+missing)

	_, err := Load()
	if err == nil {
		t.Fatal("expected: file read error")
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("missing file path must not appear in error: %v", err)
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("expected not_found reason code: %v", err)
	}
}

func TestLoad_FileCredentialRejectsOversizedFileWithoutPathLeak(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized-credential-path-sentinel")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxCredentialFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "file://"+path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected oversized credential file error")
	}
	if !strings.Contains(err.Error(), "too_large") {
		t.Fatalf("error = %v, want too_large reason", err)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "oversized-credential-path-sentinel") {
		t.Fatalf("credential path leaked in error: %v", err)
	}
}

func TestReadCredentialFileAcceptsExactLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxCredentialFileBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readCredentialFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != maxCredentialFileBytes {
		t.Fatalf("read %d bytes, want %d", len(data), maxCredentialFileBytes)
	}
}

func TestLoad_FileCredentialRejectsFIFOWithoutBlockingOrPathLeak(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "credential-fifo-path-sentinel")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICLOUD_PASSWORD", "file://"+path)

	done := make(chan error, 1)
	go func() {
		_, err := loadCredential("ICLOUD_PASSWORD")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not_regular") {
			t.Fatalf("error = %v, want not_regular reason", err)
		}
		if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "credential-fifo-path-sentinel") {
			t.Fatalf("credential path leaked in error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO credential read blocked")
	}
}

func TestLoad_FileCredentialRejectsDevice(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_PASSWORD", "file:///dev/null")
	_, err := loadCredential("ICLOUD_PASSWORD")
	if err == nil || !strings.Contains(err.Error(), "not_regular") {
		t.Fatalf("error = %v, want not_regular reason", err)
	}
	if strings.Contains(err.Error(), "/dev/null") {
		t.Fatalf("device path leaked in error: %v", err)
	}
}

func TestLoad_FileCredentials(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()

	emailPath := filepath.Join(dir, "email")
	if err := os.WriteFile(emailPath, []byte("user@example.com\n"), 0o600); err != nil {
		t.Fatalf("writing email file: %v", err)
	}
	pwPath := filepath.Join(dir, "app-password")
	if err := os.WriteFile(pwPath, []byte("  app-specific-password  \n"), 0o600); err != nil {
		t.Fatalf("writing password file: %v", err)
	}

	t.Setenv("ICLOUD_EMAIL", "file://"+emailPath)
	t.Setenv("ICLOUD_PASSWORD", "file://"+pwPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Email != "user@example.com" {
		t.Errorf("Email = %q, want trimmed value from file", cfg.Email)
	}
	if cfg.Password != "app-specific-password" {
		t.Errorf("Password = %q, want trimmed value from file", cfg.Password)
	}
}

func TestLoad_FileCredentialMissingFile(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "file:///does/not/exist/app-password")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: file read error")
	}
	if !strings.Contains(err.Error(), "file://") {
		t.Errorf("expected file:// mention: %v", err)
	}
}

func TestLoad_DefaultLogLevelIsInfo(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want Info (default)", cfg.LogLevel)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		value string
		want  slog.Level
	}{
		{"", slog.LevelInfo},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"garbage", slog.LevelInfo}, // unrecognized -> info
		{"  Debug  ", slog.LevelDebug},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := parseLogLevel(tt.value); got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseBool_Strict(t *testing.T) {
	tests := []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{"1", true, false},
		{"true", true, false},
		{"TRUE", true, false},
		{"True", true, false},
		{"0", false, false},
		{"false", false, false},
		{"FALSE", false, false},
		{"", false, false},
		{"  1  ", true, false},
		{"yes", false, true},
		{"2", false, true},
		{"on", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseBool("ICLOUD_TEST_FLAG", tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseBool() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseBool(%q) = %v, want %v", tt.value, got, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), tt.value) {
				t.Errorf("invalid boolean value leaked into error: %v", err)
			}
		})
	}
}

func TestLoad_AllBooleanVariablesAreStrict(t *testing.T) {
	for _, envVar := range []string{
		"ICLOUD_MCP_READ_ONLY",
		"ICLOUD_MCP_ENABLE_CONTACTS",
		"ICLOUD_MCP_ENABLE_MAIL",
		"ICLOUD_MCP_ENABLE_MAIL_WRITE",
		"ICLOUD_MCP_ENABLE_MAIL_SEND",
	} {
		t.Run(envVar, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ICLOUD_EMAIL", "user@example.com")
			t.Setenv("ICLOUD_PASSWORD", "app-specific-password")
			t.Setenv(envVar, "invalid-boolean-sentinel")

			_, err := Load()
			if err == nil {
				t.Fatal("expected strict boolean error")
			}
			if !strings.Contains(err.Error(), envVar) {
				t.Errorf("error does not identify variable: %v", err)
			}
			if strings.Contains(err.Error(), "invalid-boolean-sentinel") {
				t.Errorf("invalid value leaked into error: %v", err)
			}
		})
	}
}

func TestLoad_DoesNotReadMailFilesWhenMailDisabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "user@example.com")
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")
	t.Setenv("ICLOUD_MAIL_ADDRESS", "file:///missing/mail-address-sentinel")
	t.Setenv("ICLOUD_MAIL_PASSWORD", "file:///missing/mail-password-sentinel")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("disabled Mail files must not be read: %v", err)
	}
	if cfg.MailAddress != "" || cfg.MailPassword != "" {
		t.Errorf("disabled Mail credentials were populated")
	}
}

func TestLoad_MailCredentialsAndPasswordFallback(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "calendar@example.com")
	t.Setenv("ICLOUD_PASSWORD", "calendar-app-password")
	t.Setenv("ICLOUD_MCP_ENABLE_MAIL", "true")
	t.Setenv("ICLOUD_MAIL_ADDRESS", "Mailbox@Example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MailAddress != "mailbox@example.com" {
		t.Errorf("MailAddress = %q, want normalized full address", cfg.MailAddress)
	}
	if cfg.MailPassword != cfg.Password {
		t.Errorf("MailPassword did not fall back to ICLOUD_PASSWORD")
	}

	t.Setenv("ICLOUD_MAIL_PASSWORD", "separate-mail-password")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with dedicated Mail password: %v", err)
	}
	if cfg.MailPassword != "separate-mail-password" {
		t.Errorf("dedicated Mail password was not loaded")
	}
}

func TestLoad_MailCredentialsFromFiles(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	addressPath := filepath.Join(dir, "mail-address")
	passwordPath := filepath.Join(dir, "mail-password")
	if err := os.WriteFile(addressPath, []byte("mailbox@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordPath, []byte("separate-mail-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICLOUD_EMAIL", "calendar@example.com")
	t.Setenv("ICLOUD_PASSWORD", "calendar-app-password")
	t.Setenv("ICLOUD_MCP_ENABLE_MAIL", "1")
	t.Setenv("ICLOUD_MAIL_ADDRESS", "file://"+addressPath)
	t.Setenv("ICLOUD_MAIL_PASSWORD", "file://"+passwordPath)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MailAddress != "mailbox@example.com" || cfg.MailPassword != "separate-mail-password" {
		t.Errorf("Mail file credentials not loaded correctly")
	}
}

func TestConfigValidateRejectsCredentialControlsWithoutLeak(t *testing.T) {
	tests := []struct {
		name     string
		sentinel string
		mutate   func(*Config)
	}{
		{
			name:     "calendar identity CR",
			sentinel: "calendar-identity-sentinel",
			mutate:   func(c *Config) { c.Email = "calendar-identity-sentinel@example.com\r" },
		},
		{
			name:     "calendar password LF",
			sentinel: "calendar-password-sentinel",
			mutate:   func(c *Config) { c.Password = "calendar-password-sentinel\nvalue" },
		},
		{
			name:     "calendar password NUL",
			sentinel: "calendar-nul-sentinel",
			mutate:   func(c *Config) { c.Password = "calendar-nul-sentinel\x00value" },
		},
		{
			name:     "mail identity LF",
			sentinel: "mail-identity-sentinel",
			mutate: func(c *Config) {
				c.EnableMail = true
				c.MailAddress = "mail-identity-sentinel@example.com\n"
				c.MailPassword = "mail-app-password"
			},
		},
		{
			name:     "mail password CR",
			sentinel: "mail-password-sentinel",
			mutate: func(c *Config) {
				c.EnableMail = true
				c.MailAddress = "mailbox@example.com"
				c.MailPassword = "mail-password-sentinel\rvalue"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Email: "calendar@example.com", Password: "calendar-app-password"}
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected credential control validation error")
			}
			if strings.Contains(err.Error(), tt.sentinel) {
				t.Fatalf("credential leaked in validation error: %v", err)
			}
		})
	}
}

func TestConfigValidateRejectsTooShortRedactionIdentities(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "calendar identity",
			cfg:  Config{Email: "a@b", Password: "calendar-app-password"},
		},
		{
			name: "mail identity",
			cfg: Config{
				Email: "calendar@example.com", Password: "calendar-app-password",
				EnableMail: true, MailAddress: "a@b", MailPassword: "mail-app-password",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatal("expected identity below redaction length floor to be rejected")
			}
		})
	}
}

func TestConfigValidatePreservesMailPasswordMinimum(t *testing.T) {
	cfg := Config{
		Email: "calendar@example.com", Password: "calendar-app-password",
		EnableMail: true, MailAddress: "mailbox@example.com", MailPassword: "1234567",
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("error = %v, want existing eight-character minimum", err)
	}
}

func TestConfigValidateRequiresPlainCalendarAddress(t *testing.T) {
	cfg := Config{Email: "Calendar Owner <calendar@example.com>", Password: "calendar-app-password"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("display-name Calendar identity was accepted")
	}
	if strings.Contains(err.Error(), "Calendar Owner") || strings.Contains(err.Error(), "calendar@example.com") {
		t.Fatalf("Calendar identity leaked in validation error: %v", err)
	}
}

func TestLoad_MailDependencies(t *testing.T) {
	tests := []struct {
		name     string
		mail     string
		write    string
		send     string
		allowed  string
		readOnly string
		wantErr  bool
	}{
		{name: "write requires mail", write: "true", wantErr: true},
		{name: "send requires mail", send: "true", allowed: "recipient@example.com", wantErr: true},
		{name: "send requires allowlist", mail: "true", send: "true", wantErr: true},
		{name: "read-only still validates allowlist", mail: "true", send: "true", readOnly: "true", wantErr: true},
		{name: "mail write", mail: "true", write: "true"},
		{name: "mail send", mail: "true", send: "true", allowed: "recipient@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ICLOUD_EMAIL", "calendar@example.com")
			t.Setenv("ICLOUD_PASSWORD", "calendar-app-password")
			t.Setenv("ICLOUD_MAIL_ADDRESS", "mailbox@example.com")
			t.Setenv("ICLOUD_MCP_ENABLE_MAIL", tt.mail)
			t.Setenv("ICLOUD_MCP_ENABLE_MAIL_WRITE", tt.write)
			t.Setenv("ICLOUD_MCP_ENABLE_MAIL_SEND", tt.send)
			t.Setenv("ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS", tt.allowed)
			t.Setenv("ICLOUD_MCP_READ_ONLY", tt.readOnly)

			_, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_GeneratedFeatureGateCombinations(t *testing.T) {
	boolValue := func(value bool) string {
		if value {
			return "true"
		}
		return "false"
	}
	for mask := range 1 << 6 {
		readOnly := mask&(1<<0) != 0
		contacts := mask&(1<<1) != 0
		mailEnabled := mask&(1<<2) != 0
		mailWrite := mask&(1<<3) != 0
		mailSend := mask&(1<<4) != 0
		policyPresent := mask&(1<<5) != 0
		t.Run("combination_"+strconv.Itoa(mask), func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ICLOUD_EMAIL", "calendar@example.com")
			t.Setenv("ICLOUD_PASSWORD", "calendar-app-password")
			t.Setenv("ICLOUD_MAIL_ADDRESS", "mailbox@example.com")
			t.Setenv("ICLOUD_MCP_READ_ONLY", boolValue(readOnly))
			t.Setenv("ICLOUD_MCP_ENABLE_CONTACTS", boolValue(contacts))
			t.Setenv("ICLOUD_MCP_ENABLE_MAIL", boolValue(mailEnabled))
			t.Setenv("ICLOUD_MCP_ENABLE_MAIL_WRITE", boolValue(mailWrite))
			t.Setenv("ICLOUD_MCP_ENABLE_MAIL_SEND", boolValue(mailSend))
			if policyPresent {
				t.Setenv("ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS", "recipient@example.com")
			}

			cfg, err := Load()
			valid := (!mailWrite || mailEnabled) && (!mailSend || mailEnabled && policyPresent)
			if !valid {
				if err == nil {
					t.Fatal("Load() succeeded for an invalid dependency combination")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.ReadOnly != readOnly || cfg.EnableContacts != contacts || cfg.EnableMail != mailEnabled || cfg.EnableMailWrite != mailWrite || cfg.EnableMailSend != mailSend {
				t.Errorf("configured feature flags do not match the environment")
			}
			if cfg.SMTPRecipientPolicy.Configured() != policyPresent {
				t.Errorf("recipient policy configured = %v, want %v", cfg.SMTPRecipientPolicy.Configured(), policyPresent)
			}
			if got, want := cfg.EffectiveContactsWrite(), contacts && !readOnly; got != want {
				t.Errorf("EffectiveContactsWrite() = %v, want %v", got, want)
			}
			if got, want := cfg.EffectiveMailWrite(), mailEnabled && mailWrite && !readOnly; got != want {
				t.Errorf("EffectiveMailWrite() = %v, want %v", got, want)
			}
			if got, want := cfg.EffectiveMailSend(), mailEnabled && mailSend && !readOnly && policyPresent; got != want {
				t.Errorf("EffectiveMailSend() = %v, want %v", got, want)
			}
		})
	}
}

func TestLoad_InvalidMailAddressDoesNotLeak(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "calendar@example.com")
	t.Setenv("ICLOUD_PASSWORD", "calendar-app-password")
	t.Setenv("ICLOUD_MCP_ENABLE_MAIL", "true")
	t.Setenv("ICLOUD_MAIL_ADDRESS", "Mailbox Owner <mailbox@example.com>")

	_, err := Load()
	if err == nil {
		t.Fatal("expected invalid Mail address error")
	}
	if strings.Contains(err.Error(), "Mailbox Owner") || strings.Contains(err.Error(), "mailbox@example.com") {
		t.Fatalf("Mail identity leaked into config error: %v", err)
	}
}

func TestLoad_InvalidRecipientAllowlistDoesNotLeak(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "calendar@example.com")
	t.Setenv("ICLOUD_PASSWORD", "calendar-app-password")
	t.Setenv("ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS", "allowed@example.com,recipient-value-sentinel")

	_, err := Load()
	if err == nil {
		t.Fatal("expected invalid recipient allowlist error")
	}
	for _, value := range []string{"allowed@example.com", "recipient-value-sentinel"} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("recipient allowlist value leaked into config error: %v", err)
		}
	}
}

func TestParseRecipientPolicy(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "one", value: "one@example.com"},
		{name: "many with spaces", value: "One@Example.com, two@example.com"},
		{name: "wildcard", value: " * "},
		{name: "empty", value: "", wantErr: true},
		{name: "empty item", value: "one@example.com,,two@example.com", wantErr: true},
		{name: "trailing empty", value: "one@example.com,", wantErr: true},
		{name: "display name", value: "One <one@example.com>", wantErr: true},
		{name: "partial wildcard", value: "*@example.com", wantErr: true},
		{name: "wildcard mixed", value: "*,one@example.com", wantErr: true},
		{name: "duplicate exact", value: "one@example.com,one@example.com", wantErr: true},
		{name: "duplicate case folded", value: "One@Example.com,one@example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := ParseRecipientPolicy(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRecipientPolicy() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.value != "" && strings.Contains(err.Error(), tt.value) {
				t.Errorf("allowlist value leaked into error: %v", err)
			}
			if err == nil && !policy.Configured() {
				t.Error("valid policy is not configured")
			}
		})
	}
}

func TestRecipientPolicy_ExactCaseFoldedMatching(t *testing.T) {
	policy, err := ParseRecipientPolicy("Allowed@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Allows("allowed@example.com") || !policy.Allows(" ALLOWED@EXAMPLE.COM ") {
		t.Error("exact address should match after surrounding trim and ASCII case folding")
	}
	for _, rejected := range []string{"other@example.com", "allowed@example.com.evil", "Name <allowed@example.com>"} {
		if policy.Allows(rejected) {
			t.Errorf("unexpected match for %q", rejected)
		}
	}

	wildcard, err := ParseRecipientPolicy("*")
	if err != nil {
		t.Fatal(err)
	}
	if !wildcard.AllowAll() || !wildcard.Allows("any@example.com") || wildcard.Allows("not-an-address") {
		t.Error("wildcard policy should allow every valid full address only")
	}
}

func TestConfig_EffectiveCapabilitiesPreserveConfiguredFlags(t *testing.T) {
	policy, err := ParseRecipientPolicy("recipient@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		ReadOnly:              true,
		EnableContacts:        true,
		EnableMail:            true,
		EnableMailWrite:       true,
		EnableMailSend:        true,
		SMTPRecipientPolicy:   policy,
		SMTPAllowedRecipients: policy.Recipients(),
	}
	if cfg.EffectiveContactsWrite() || cfg.EffectiveMailWrite() || cfg.EffectiveMailSend() {
		t.Error("global read-only did not suppress effective writes")
	}
	if !cfg.EnableContacts || !cfg.EnableMailWrite || !cfg.EnableMailSend {
		t.Error("global read-only changed configured flags")
	}

	cfg.ReadOnly = false
	if !cfg.EffectiveContactsWrite() || !cfg.EffectiveMailWrite() || !cfg.EffectiveMailSend() {
		t.Error("configured effective capabilities were not enabled")
	}
}
