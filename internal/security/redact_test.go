package security

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestRedactor_Redact(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		input   string
		want    string
	}{
		{
			name:    "secret simple présent",
			secrets: []string{"SENTINEL-PW-abc123"},
			input:   "mot de passe : SENTINEL-PW-abc123 refusé",
			want:    "mot de passe : [REDACTED] refusé",
		},
		{
			name:    "secret absent",
			secrets: []string{"SENTINEL-PW-abc123"},
			input:   "rien à voir ici",
			want:    "rien à voir ici",
		},
		{
			name:    "plusieurs secrets",
			secrets: []string{"pass1234", "user@example.com-secret"},
			input:   "pass1234 et user@example.com-secret dans le même message",
			want:    "[REDACTED] et [REDACTED] dans le même message",
		},
		{
			name:    "secret répété",
			secrets: []string{"pass1234"},
			input:   "pass1234 pass1234 pass1234",
			want:    "[REDACTED] [REDACTED] [REDACTED]",
		},
		{
			name:    "secret trop court ignoré",
			secrets: []string{"ab"},
			input:   "ab ab ab",
			want:    "ab ab ab",
		},
		{
			name:    "secret vide ignoré",
			secrets: []string{""},
			input:   "texte normal",
			want:    "texte normal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRedactor(tt.secrets...)
			if got := r.Redact(tt.input); got != tt.want {
				t.Errorf("Redact() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedactingWriter_ThroughSlog(t *testing.T) {
	password := "SENTINEL-PW-abc123" // gitleaks:allow, sentinelle de test, pas un vrai secret
	var buf bytes.Buffer
	r := NewRedactor(password)
	rw := NewRedactingWriter(&buf, r)

	logger := slog.New(slog.NewTextHandler(rw, nil))
	logger.Info("échec authentification", "err", "mot de passe invalide: "+password)

	out := buf.String()
	if strings.Contains(out, password) {
		t.Fatalf("le password n'a pas été rédigé dans la sortie slog : %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("sortie attendue contenant [REDACTED], obtenu : %q", out)
	}
}

func TestRedactingWriter_Base64AndURLEncodedForms(t *testing.T) {
	email := "user@example.com"
	password := "SENTINEL-PW-abc123" // gitleaks:allow, sentinelle de test, pas un vrai secret
	basicAuth := base64.StdEncoding.EncodeToString([]byte(email + ":" + password))
	urlEncoded := url.QueryEscape(password)

	r := NewRedactor(password, basicAuth, urlEncoded)
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, r)

	msg := "Authorization: Basic " + basicAuth + " ; query redirigée avec pw=" + urlEncoded
	if _, err := rw.Write([]byte(msg)); err != nil {
		t.Fatalf("Write : %v", err)
	}

	out := buf.String()
	if strings.Contains(out, password) {
		t.Errorf("password brut trouvé dans la sortie : %q", out)
	}
	if strings.Contains(out, basicAuth) {
		t.Errorf("forme base64 trouvée dans la sortie : %q", out)
	}
	if strings.Contains(out, urlEncoded) && urlEncoded != password {
		t.Errorf("forme url-encoded trouvée dans la sortie : %q", out)
	}
}

// TestRedactor_RedactsEmail, FIX-12 (optionnel). main.go enregistre
// désormais cfg.Email au même titre que cfg.Password dans le Redactor
// runtime (« le password OU l'email » selon le cahier des charges). Le
// mécanisme générique multi-secrets de Redactor le supportait déjà (cf.
// "plusieurs secrets" ci-dessus), ce test isole spécifiquement le cas
// email pour documenter l'intention de FIX-12.
func TestRedactor_RedactsEmail(t *testing.T) {
	email := "user@example.com"
	r := NewRedactor(email)
	out := r.Redact("authentification refusée pour " + email)
	if strings.Contains(out, email) {
		t.Errorf("email non rédigé dans la sortie : %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("sortie attendue contenant [REDACTED], obtenu : %q", out)
	}
}

func TestRedactingWriter_ReturnsOriginalLength(t *testing.T) {
	r := NewRedactor("secretvalue")
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf, r)

	p := []byte("contient secretvalue ici")
	n, err := rw.Write(p)
	if err != nil {
		t.Fatalf("Write : %v", err)
	}
	if n != len(p) {
		t.Errorf("Write() n = %d, want %d (len original)", n, len(p))
	}
}
