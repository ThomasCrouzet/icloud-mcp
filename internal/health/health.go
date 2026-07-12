// Package health provides a minimal HTTP healthcheck, optional and OFF by
// default. This is not an application-level network service: the MCP server
// speaks stdio; this endpoint only lets an external supervisor (e.g. a
// Docker healthcheck) probe that the process is alive.
package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Server exposes /healthz on the provided address.
type Server struct {
	srv *http.Server
}

// Start launches the HTTP healthcheck in the background on addr (e.g.
// "127.0.0.1:8797", never 0.0.0.0) and returns immediately. If the bind
// fails, the error is returned by this call; errors occurring afterwards
// (ListenAndServe) are silent from the caller's perspective (the MCP server
// must not die because of a healthcheck).
func Start(addr string) (*Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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
