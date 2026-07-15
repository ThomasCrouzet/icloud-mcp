// Package health provides a minimal HTTP healthcheck, optional and OFF by
// default. This is not an application-level network service: the MCP server
// speaks stdio; this endpoint only lets an external supervisor (e.g. a
// Docker healthcheck) probe that the process is alive and report its version
// and current rate-limiter state.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Server exposes /healthz (liveness) and /status (version + rate limits) on
// the provided address.
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
// statusFn, if non-nil, returns the current rate-limiter state for /status;
// pass nil when there is no guarded service to report on.
func Start(addr, version string, statusFn func() any) (*Server, error) {
	if err := validateLoopbackAddr(addr); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		// GET/HEAD only: keep the endpoint strict, no side effects.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var rate any
		if statusFn != nil {
			rate = statusFn()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// A nil rate is serialized as a JSON null, not an error.
		body, _ := json.Marshal(map[string]any{
			"version":     version,
			"rate_limits": rate,
		})
		_, _ = w.Write(body)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
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
	if addr == "" {
		return fmt.Errorf("health address cannot be empty")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Malformed addresses that are not parseable as host:port are left to
		// net.Listen so Start keeps failing with a bind error (preserves the
		// historical TestStart_InvalidAddrFails behavior).
		return nil
	}

	// Empty host means "all interfaces" (e.g. ":8797"). Always reject.
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "::", "[::]":
		return fmt.Errorf("health address %q rejected: must bind to loopback only (use 127.0.0.1 or ::1)", addr)
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return nil
	}

	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// Hostname other than localhost: resolve and require every answer to
		// be loopback. Fail closed if resolution fails.
		ips, rerr := net.LookupIP(host)
		if rerr != nil || len(ips) == 0 {
			return fmt.Errorf("health address %q rejected: cannot resolve host as loopback: %v", addr, rerr)
		}
		for _, resolved := range ips {
			if !resolved.IsLoopback() {
				return fmt.Errorf("health address %q rejected: must bind to loopback only (use 127.0.0.1 or ::1)", addr)
			}
		}
		return nil
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("health address %q rejected: must bind to loopback only (use 127.0.0.1 or ::1)", addr)
	}
	return nil
}
