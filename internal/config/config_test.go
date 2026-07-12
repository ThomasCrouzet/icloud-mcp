package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ICLOUD_EMAIL", "ICLOUD_PASSWORD", "ICLOUD_MCP_READ_ONLY"} {
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
}

func TestLoad_InvalidEmail(t *testing.T) {
	clearEnv(t)
	t.Setenv("ICLOUD_EMAIL", "not-an-email")
	t.Setenv("ICLOUD_PASSWORD", "app-specific-password")

	_, err := Load()
	if err == nil {
		t.Fatal("expected: invalid email error")
	}
	if !strings.Contains(err.Error(), "ICLOUD_EMAIL") {
		t.Errorf("expected error mentioning ICLOUD_EMAIL: %v", err)
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
}

func TestParseBool_ReadOnly(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"0", false},
		{"false", false},
		{"", false},
		{"yes", false},
		{"  1  ", true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ICLOUD_EMAIL", "user@example.com")
			t.Setenv("ICLOUD_PASSWORD", "app-specific-password")
			t.Setenv("ICLOUD_MCP_READ_ONLY", tt.value)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ReadOnly != tt.want {
				t.Errorf("ReadOnly for %q = %v, want %v", tt.value, cfg.ReadOnly, tt.want)
			}
		})
	}
}
