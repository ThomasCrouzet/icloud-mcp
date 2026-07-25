// Package health provides a minimal HTTP healthcheck, optional and OFF by
// default. This is not an application-level network service: the MCP server
// speaks stdio; this endpoint only lets an external supervisor (e.g. a
// Docker healthcheck) probe that the process is alive and report its version,
// enabled domains, and current rate-limiter state.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DomainStatus describes one domain for the rich health JSON.
type DomainStatus struct {
	Status string `json:"status"` // ok | disabled
}

// Status is the unified JSON body for /healthz and /status.
type Status struct {
	Status     string                  `json:"status"`
	Timestamp  string                  `json:"timestamp"`
	Version    string                  `json:"version"`
	Domains    map[string]DomainStatus `json:"domains"`
	RateLimits any                     `json:"rateLimits"`
}

// Snapshot builds the live Status value. rateLimits may be nil.
func Snapshot(version string, domains map[string]DomainStatus, rateLimits any) Status {
	if version == "" {
		version = "dev"
	}
	if domains == nil {
		domains = map[string]DomainStatus{
			"calendar": {Status: "ok"},
			"contacts": {Status: "disabled"},
			"mail":     {Status: "disabled"},
		}
	}
	return Status{
		Status:     "ok",
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Version:    version,
		Domains:    domains,
		RateLimits: rateLimits,
	}
}

// Server exposes /healthz and /status (identical rich JSON) on the provided
// address. Both remain GET/HEAD only.
type Server struct {
	srv *http.Server
}

// Start launches the HTTP healthcheck in the background on addr (e.g.
// "127.0.0.1:8797"). Non-loopback binds (0.0.0.0, ::, LAN addresses) are
// rejected before Listen. If the bind fails, the error is returned by this
// call; errors occurring afterwards (ListenAndServe) are silent from the
// caller's perspective (the MCP server must not die because of a healthcheck).
//
// version is the binary version (main.version, overridden at build time).
// domains reports enablement only (no network probe). statusFn, if non-nil,
// returns the current multi-domain rate-limiter state.
func Start(addr, version string, domains map[string]DomainStatus, statusFn func() any) (*Server, error) {
	listenAddr, err := canonicalLoopbackAddr(addr)
	if err != nil {
		return nil, err
	}

	writeStatus := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var rate any
		if statusFn != nil {
			rate = statusFn()
		}
		body, _ := json.Marshal(Snapshot(version, domains, rate))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", writeStatus)
	mux.HandleFunc("/status", writeStatus)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}

	go func() {
		_ = srv.Serve(ln)
	}()

	return &Server{srv: srv}, nil
}

// Close stops the healthcheck server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// validateLoopbackAddr rejects addresses that would listen on non-loopback
// interfaces (0.0.0.0, ::, bare ":port", LAN IPs). Only 127.0.0.1, ::1, and
// localhost are accepted. This enforces the documented threat-model rule:
// never expose /healthz or /status on all interfaces.
func validateLoopbackAddr(addr string) error {
	_, err := canonicalLoopbackAddr(addr)
	return err
}

func canonicalLoopbackAddr(addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("health address cannot be empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("health address is invalid: %w", err)
	}

	switch host {
	case "localhost":
		return net.JoinHostPort("127.0.0.1", port), nil
	case "127.0.0.1", "::1":
		return net.JoinHostPort(host, port), nil
	default:
		return "", fmt.Errorf("health address %q rejected: must bind to loopback only (use 127.0.0.1 or ::1)", addr)
	}
}
