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
		{"primary host", "caldav.icloud.com", true},
		{"shard 1 digit", "p1-caldav.icloud.com", true},
		{"shard 2 digits", "p46-caldav.icloud.com", true},
		{"shard 3 digits", "p123-caldav.icloud.com", true},
		{"foreign host", "evil.com", false},
		{"primary host + malicious suffix", "caldav.icloud.com.evil.io", false},
		{"non-conforming prefix", "xcaldav.icloud.com", false},
		{"shard + malicious suffix", "p46-caldav.icloud.com.evil.io", false},
		{"uppercase not normalized", "P46-CALDAV.ICLOUD.COM", false},
		{"empty", "", false},
		{"shard 4 digits rejected", "p1234-caldav.icloud.com", false},
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

	// Production allowlist: only caldav.icloud.com is authorized; the test
	// server (127.0.0.1:xxxxx) is not.
	transport := NewAllowlistTransport(http.DefaultTransport, IsICloudHost)
	client := &http.Client{Transport: transport}

	resp, err := client.Get(srv.URL) //nolint:noctx
	if err == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected: allowlist error, got: success")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("expected error containing 'rejected', got: %v", err)
	}
	if hits != 0 {
		t.Errorf("the mock server should never have been contacted, hits=%d", hits)
	}
}

// stubRoundTripper is a minimal http.RoundTripper performing NO network
// access, useful to test the AllowlistTransport.RoundTrip logic in
// isolation, including with a URL without an explicit port (a real
// httptest.Server always exposes a port, which prevents driving a real
// network call to an "https://host/" URL without a port).
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
	// Permissive allowlist for the test: everything is authorized. The inner
	// RoundTripper is a stub (no real network access), which became necessary
	// once the transport started rejecting any non-standard explicit port in
	// the URL, incompatible with a real httptest.Server (always bound to an
	// ephemeral port).
	transport := NewAllowlistTransport(stub, func(string) bool { return true })

	req, err := http.NewRequest(http.MethodGet, "https://caldav.icloud.com/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if !stub.called {
		t.Error("the inner RoundTripper should have been called (host+scheme+port allowed)")
	}
}

// TestAllowlistTransport_RejectsExplicitPort: a NON-standard explicit port
// (anything other than 443) must be refused, even on an otherwise
// authorized host.
func TestAllowlistTransport_RejectsExplicitPort(t *testing.T) {
	stub := &stubRoundTripper{resp: newStubResponse()}
	transport := NewAllowlistTransport(stub, IsICloudHost)

	req, err := http.NewRequest(http.MethodGet, "https://caldav.icloud.com:1234/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected: explicit port rejected error")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("expected error mentioning 'port', got: %v", err)
	}
	if stub.called {
		t.Error("the inner RoundTripper should never have been called (port rejected before dispatch)")
	}
}

// TestAllowlistTransport_AllowsExplicit443, regression: iCloud returns the
// shard URL with an EXPLICIT 443 port (e.g. p120-caldav.icloud.com:443).
// This port must be accepted, otherwise every tool call fails after
// discovery.
func TestAllowlistTransport_AllowsExplicit443(t *testing.T) {
	stub := &stubRoundTripper{resp: newStubResponse()}
	transport := NewAllowlistTransport(stub, IsICloudHost)

	req, err := http.NewRequest(http.MethodGet, "https://p120-caldav.icloud.com:443/11901403220/calendars/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("explicit port 443 should be allowed, error: %v", err)
	}
	_ = resp.Body.Close()
	if !stub.called {
		t.Error("the inner RoundTripper should have been called (host + 443 allowed)")
	}
}

func TestAllowlistTransport_RejectsNonHTTPS(t *testing.T) {
	transport := NewAllowlistTransport(http.DefaultTransport, func(string) bool { return true })
	req, err := http.NewRequest(http.MethodGet, "http://caldav.icloud.com/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected: http scheme rejected error")
	}
	if !strings.Contains(err.Error(), "https only") {
		t.Errorf("expected error mentioning 'https only', got: %v", err)
	}
}

func TestNewICloudHTTPClient_RejectsNonICloudHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewICloudHTTPClient(2 * 1000 * 1000 * 1000) // 2s
	// srv.URL is http://127.0.0.1:port, rejected on the scheme AND the host.
	resp, err := client.Get(srv.URL) //nolint:noctx
	if err == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected: allowlist error, got: success")
	}
}
