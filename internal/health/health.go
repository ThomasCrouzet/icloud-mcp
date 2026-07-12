// Package health fournit un healthcheck HTTP minimal, optionnel et OFF par
// défaut. Ce n'est pas un service réseau applicatif : le serveur MCP parle
// stdio, ce endpoint sert uniquement à un superviseur externe (ex. Docker
// healthcheck) qui voudrait sonder que le process est vivant.
package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Server expose /healthz sur l'adresse fournie.
type Server struct {
	srv *http.Server
}

// Start démarre le healthcheck HTTP en arrière-plan sur addr (ex.
// "127.0.0.1:8797", jamais 0.0.0.0) et retourne immédiatement. En cas
// d'échec de bind, l'erreur est retournée par cet appel ; les erreurs
// survenant après (ListenAndServe) sont silencieuses côté appelant (le
// serveur MCP ne doit pas mourir pour un healthcheck).
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

// Close arrête le serveur de healthcheck.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
