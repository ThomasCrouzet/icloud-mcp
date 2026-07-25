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
	listenAddr, err := canonicalLoopbackAddr(addr)
	if err != nil {
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
