package health

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

const testVersion = "test-1.2.3"

func TestServer_HealthzReturnsOK(t *testing.T) {
	const addr = "127.0.0.1:18797"
	s, err := Start(addr, testVersion, nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close() }()

	resp := waitFor200(t, "http://"+addr+"/healthz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

func TestServer_StatusReportsVersionAndRateLimits(t *testing.T) {
	const addr = "127.0.0.1:18799"
	rateStatus := map[string]any{
		"read":  map[string]any{"tokens": 9.5, "limit": 1.0, "burst": 10},
		"write": map[string]any{"tokens": 2.0, "limit": 0.33, "burst": 3},
	}
	s, err := Start(addr, testVersion, func() any { return rateStatus })
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close() }()

	resp := waitFor200(t, "http://"+addr+"/status")
	defer func() { _ = resp.Body.Close() }()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding /status JSON: %v", err)
	}
	if got["version"] != testVersion {
		t.Errorf("version = %v, want %q", got["version"], testVersion)
	}
	rl, _ := got["rate_limits"].(map[string]any)
	if rl == nil {
		t.Fatal("rate_limits missing or not an object")
	}
	if read, _ := rl["read"].(map[string]any); read == nil || read["burst"] == nil {
		t.Errorf("rate_limits.read missing: %v", rl)
	}
	// No secrets: ensure no credential-looking keys are present.
	for k := range got {
		if k == "password" || k == "email" || k == "secret" {
			t.Errorf("unexpected secret-bearing key in /status: %q", k)
		}
	}
}

func TestServer_StatusNullRateLimitsWhenStatusFnNil(t *testing.T) {
	const addr = "127.0.0.1:18800"
	s, err := Start(addr, testVersion, nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close() }()

	resp := waitFor200(t, "http://"+addr+"/status")
	defer func() { _ = resp.Body.Close() }()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding /status JSON: %v", err)
	}
	if got["version"] != testVersion {
		t.Errorf("version = %v, want %q", got["version"], testVersion)
	}
	if got["rate_limits"] != nil {
		t.Errorf("rate_limits = %v, want null when statusFn is nil", got["rate_limits"])
	}
}

func TestServer_Close(t *testing.T) {
	const addr = "127.0.0.1:18798"
	s, err := Start(addr, testVersion, nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

func TestStart_InvalidAddrFails(t *testing.T) {
	_, err := Start("not-a-valid-address", testVersion, nil)
	if err == nil {
		t.Fatal("expected: bind error on an invalid address")
	}
}

// TestServer_RejectsNonGET: /healthz and /status are read-only probes; a
// non-GET/HEAD method gets 405 with an Allow header, never a side effect.
func TestServer_RejectsNonGET(t *testing.T) {
	const addr = "127.0.0.1:18801"
	s, err := Start(addr, testVersion, nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Wait for the listener to be up before probing with POST.
	ready := waitFor200(t, "http://"+addr+"/healthz")
	_ = ready.Body.Close()

	for _, path := range []string{"/healthz", "/status"} {
		resp, perr := http.Post("http://"+addr+path, "text/plain", nil) //nolint:noctx
		if perr != nil {
			t.Fatalf("POST %s: %v", path, perr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want 405", path, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow == "" {
			t.Errorf("POST %s: missing Allow header", path)
		}
	}
}

// waitFor200 polls the URL until it responds, up to ~500ms, so the Serve
// goroutine has time to start.
func waitFor200(t *testing.T, url string) *http.Response {
	t.Helper()
	var resp *http.Response
	var getErr error
	for i := 0; i < 50; i++ {
		resp, getErr = http.Get(url) //nolint:noctx
		if getErr == nil {
			return resp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s: %v", url, getErr)
	return nil
}
