package health

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestServer_HealthzReturnsOK(t *testing.T) {
	const addr = "127.0.0.1:18797"
	s, err := Start(addr)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Give the Serve goroutine time to start.
	var resp *http.Response
	var getErr error
	for i := 0; i < 50; i++ {
		resp, getErr = http.Get("http://" + addr + "/healthz") //nolint:noctx
		if getErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if getErr != nil {
		t.Fatalf("GET /healthz: %v", getErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

func TestServer_Close(t *testing.T) {
	const addr = "127.0.0.1:18798"
	s, err := Start(addr)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

func TestStart_InvalidAddrFails(t *testing.T) {
	_, err := Start("not-a-valid-address")
	if err == nil {
		t.Fatal("expected: bind error on an invalid address")
	}
}
