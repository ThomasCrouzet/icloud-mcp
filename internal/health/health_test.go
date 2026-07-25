package health

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testVersion = "test-1.2.3"

func TestServer_HealthzReturnsJSON(t *testing.T) {
	const addr = "127.0.0.1:18797"
	domains := map[string]DomainStatus{
		"calendar": {Status: "ok"},
		"contacts": {Status: "disabled"},
		"mail":     {Status: "disabled"},
	}
	s, err := Start(addr, testVersion, domains, nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close() }()

	resp := waitFor200(t, "http://"+addr+"/healthz")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if got.Status != "ok" || got.Version != testVersion {
		t.Errorf("status/version = %q/%q", got.Status, got.Version)
	}
	if got.Domains["calendar"].Status != "ok" || got.Domains["contacts"].Status != "disabled" {
		t.Errorf("domains = %+v", got.Domains)
	}
	if got.Timestamp == "" {
		t.Error("timestamp missing")
	}
}

func TestServer_StatusReportsVersionAndRateLimits(t *testing.T) {
	const addr = "127.0.0.1:18799"
	rateStatus := map[string]any{
		"calendar": map[string]any{
			"read":  map[string]any{"tokens": 9.5, "limit": 1.0, "burst": 10},
			"write": map[string]any{"tokens": 2.0, "limit": 0.33, "burst": 3},
		},
	}
	domains := map[string]DomainStatus{"calendar": {Status: "ok"}, "contacts": {Status: "ok"}, "mail": {Status: "disabled"}}
	s, err := Start(addr, testVersion, domains, func() any { return rateStatus })
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
	rl, _ := got["rateLimits"].(map[string]any)
	if rl == nil {
		t.Fatal("rateLimits missing or not an object")
	}
	if cal, _ := rl["calendar"].(map[string]any); cal == nil {
		t.Errorf("rateLimits.calendar missing: %v", rl)
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
	s, err := Start(addr, testVersion, nil, nil)
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
	if got["rateLimits"] != nil {
		t.Errorf("rateLimits = %v, want null when statusFn is nil", got["rateLimits"])
	}
}

func TestServer_Close(t *testing.T) {
	const addr = "127.0.0.1:18798"
	s, err := Start(addr, testVersion, nil, nil)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

func TestStart_InvalidAddrFails(t *testing.T) {
	_, err := Start("not-a-valid-address", testVersion, nil, nil)
	if err == nil {
		t.Fatal("expected: bind error on an invalid address")
	}
}

// TestStart_RejectsNonLoopback enforces the security rule that -health must
// never bind 0.0.0.0 / :: / bare :port (all interfaces).
func TestStart_RejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:18790",
		"[::]:18791",
		":18792",
		"8.8.8.8:18793",
	} {
		_, err := Start(addr, testVersion, nil, nil)
		if err == nil {
			t.Errorf("Start(%q) succeeded, want loopback-only rejection", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("Start(%q) error = %v, want message mentioning loopback", addr, err)
		}
	}
}

// TestStart_AcceptsLoopback: 127.0.0.1 and localhost remain valid.
func TestStart_AcceptsLoopback(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:18810",
		"localhost:18811",
	} {
		s, err := Start(addr, testVersion, nil, nil)
		if err != nil {
			t.Fatalf("Start(%q) error: %v", addr, err)
		}
		_ = s.Close()
	}
}

func TestValidateLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:8797", false},
		{"localhost:8797", false},
		{"[::1]:8797", false},
		{"0.0.0.0:8797", true},
		{":8797", true},
		{"", true},
		{"192.168.1.1:8797", true},
		{"loopback.example:8797", true},
	}
	for _, tt := range tests {
		err := validateLoopbackAddr(tt.addr)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateLoopbackAddr(%q) err=%v, wantErr=%v", tt.addr, err, tt.wantErr)
		}
	}
}

func TestCanonicalLoopbackAddrAvoidsHostnameResolution(t *testing.T) {
	got, err := canonicalLoopbackAddr("localhost:8797")
	if err != nil {
		t.Fatal(err)
	}
	if got != "127.0.0.1:8797" {
		t.Fatalf("canonical address = %q", got)
	}
}

// TestServer_RejectsNonGET: /healthz and /status are read-only probes; a
// non-GET/HEAD method gets 405 with an Allow header, never a side effect.
func TestServer_RejectsNonGET(t *testing.T) {
	const addr = "127.0.0.1:18801"
	s, err := Start(addr, testVersion, nil, nil)
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

func TestSnapshotDefaults(t *testing.T) {
	got := Snapshot("", nil, nil)
	if got.Status != "ok" || got.Version != "dev" {
		t.Fatalf("snapshot = %+v", got)
	}
	if got.Domains["calendar"].Status != "ok" {
		t.Fatalf("calendar domain = %+v", got.Domains)
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

// Silence unused import if io is only needed in older tests.
var _ = io.Discard
