package security

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsICloudHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{"host principal", "caldav.icloud.com", true},
		{"shard 1 chiffre", "p1-caldav.icloud.com", true},
		{"shard 2 chiffres", "p46-caldav.icloud.com", true},
		{"shard 3 chiffres", "p123-caldav.icloud.com", true},
		{"host étranger", "evil.com", false},
		{"host principal + suffixe malveillant", "caldav.icloud.com.evil.io", false},
		{"préfixe non conforme", "xcaldav.icloud.com", false},
		{"shard + suffixe malveillant", "p46-caldav.icloud.com.evil.io", false},
		{"majuscules non normalisées", "P46-CALDAV.ICLOUD.COM", false},
		{"vide", "", false},
		{"shard 4 chiffres refusé", "p1234-caldav.icloud.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsICloudHost(tt.host); got != tt.want {
				t.Errorf("IsICloudHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestAllowlistTransport_RejectsDisallowedHost(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Allowlist de production : seul caldav.icloud.com est autorisé, le
	// serveur de test (127.0.0.1:xxxxx) ne l'est pas.
	transport := NewAllowlistTransport(http.DefaultTransport, IsICloudHost)
	client := &http.Client{Transport: transport}

	resp, err := client.Get(srv.URL) //nolint:noctx
	if err == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("attendu : erreur allowlist, obtenu : succès")
	}
	if !strings.Contains(err.Error(), "refusé") {
		t.Errorf("erreur attendue contenant 'refusé', obtenu : %v", err)
	}
	if hits != 0 {
		t.Errorf("le serveur mock n'aurait jamais dû être contacté, hits=%d", hits)
	}
}

// stubRoundTripper est un http.RoundTripper minimal qui ne fait AUCUN accès
// réseau, utile pour tester la logique de AllowlistTransport.RoundTrip
// isolément, y compris avec une URL sans port explicite (un vrai
// httptest.Server expose toujours un port, ce qui empêche de driver un appel
// réseau réel vers une URL "https://host/" sans port, cf. FIX-1).
type stubRoundTripper struct {
	called bool
	resp   *http.Response
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.called = true
	return s.resp, nil
}

func newStubResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
}

func TestAllowlistTransport_AllowsWhitelistedHost(t *testing.T) {
	stub := &stubRoundTripper{resp: newStubResponse()}
	// Allowlist permissive pour le test : tout est autorisé. Le RoundTripper
	// interne est un stub (pas de vrai accès réseau), nécessaire depuis
	// FIX-1, qui rejette tout port explicite dans l'URL, incompatible avec
	// un httptest.Server réel (toujours sur un port éphémère).
	transport := NewAllowlistTransport(stub, func(string) bool { return true })

	req, err := http.NewRequest(http.MethodGet, "https://caldav.icloud.com/", nil)
	if err != nil {
		t.Fatalf("construction requête : %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	_ = resp.Body.Close()
	if !stub.called {
		t.Error("le RoundTripper interne aurait dû être appelé (host+scheme+port autorisés)")
	}
}

// TestAllowlistTransport_RejectsExplicitPort, FIX-1. Un port explicite NON
// standard (≠ 443) doit être refusé, même sur un host par ailleurs autorisé.
func TestAllowlistTransport_RejectsExplicitPort(t *testing.T) {
	stub := &stubRoundTripper{resp: newStubResponse()}
	transport := NewAllowlistTransport(stub, IsICloudHost)

	req, err := http.NewRequest(http.MethodGet, "https://caldav.icloud.com:1234/", nil)
	if err != nil {
		t.Fatalf("construction requête : %v", err)
	}
	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("attendu : erreur port explicite refusé")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("erreur attendue mentionnant 'port', obtenu : %v", err)
	}
	if stub.called {
		t.Error("le RoundTripper interne n'aurait jamais dû être appelé (port refusé avant dispatch)")
	}
}

// TestAllowlistTransport_AllowsExplicit443, régression : iCloud renvoie l'URL
// de shard avec un port 443 EXPLICITE (ex. p120-caldav.icloud.com:443). Ce port
// doit être accepté, sinon tout appel de tool échoue après la découverte.
func TestAllowlistTransport_AllowsExplicit443(t *testing.T) {
	stub := &stubRoundTripper{resp: newStubResponse()}
	transport := NewAllowlistTransport(stub, IsICloudHost)

	req, err := http.NewRequest(http.MethodGet, "https://p120-caldav.icloud.com:443/11901403220/calendars/", nil)
	if err != nil {
		t.Fatalf("construction requête : %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("port 443 explicite devrait être autorisé, erreur : %v", err)
	}
	_ = resp.Body.Close()
	if !stub.called {
		t.Error("le RoundTripper interne aurait dû être appelé (host + 443 autorisés)")
	}
}

func TestAllowlistTransport_RejectsNonHTTPS(t *testing.T) {
	transport := NewAllowlistTransport(http.DefaultTransport, func(string) bool { return true })
	req, err := http.NewRequest(http.MethodGet, "http://caldav.icloud.com/", nil)
	if err != nil {
		t.Fatalf("construction requête : %v", err)
	}
	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("attendu : erreur scheme http refusé")
	}
	if !strings.Contains(err.Error(), "https uniquement") {
		t.Errorf("erreur attendue mentionnant 'https uniquement', obtenu : %v", err)
	}
}

func TestNewICloudHTTPClient_RejectsNonICloudHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewICloudHTTPClient(2 * 1000 * 1000 * 1000) // 2s
	// srv.URL est en http://127.0.0.1:port, refusé sur le scheme ET le host.
	resp, err := client.Get(srv.URL) //nolint:noctx
	if err == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("attendu : erreur allowlist, obtenu : succès")
	}
}
