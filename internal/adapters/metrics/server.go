package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nd/mm-standx/internal/ports"
)

// Server отдаёт показатели по HTTP.
//
// Слушающий сокет открывается синхронно в Start: занятый порт означает, что
// рядом уже работает другой экземпляр бота, и узнать об этом нужно до того,
// как второй экземпляр начнёт выставлять ордера тем же ключом.
type Server struct {
	addr    string
	handler http.Handler
	log     *slog.Logger

	srv      *http.Server
	listener net.Listener
}

// NewServer создаёт сервер метрик. Пустой адрес отключает его.
func NewServer(addr string, p *Prometheus, log *slog.Logger) *Server {
	return &Server{
		addr:    addr,
		handler: promhttp.HandlerFor(p.Registry(), promhttp.HandlerOpts{}),
		log:     log.With("component", "metrics"),
	}
}

func (s *Server) Name() string { return "metrics" }

func (s *Server) Start(context.Context) error {
	if s.addr == "" {
		s.log.Info("metrics are disabled: no address configured")
		return nil
	}

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("metrics: listen on %s: %w", s.addr, err)
	}
	s.listener = listener

	mux := http.NewServeMux()
	mux.Handle("/metrics", s.handler)
	// Проба живости для супервизора: отвечает, пока процесс способен принимать
	// соединения. О состоянии торговли она не говорит ничего — это работа
	// метрик и сторожа, а не HTTP-обработчика.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := s.srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("metrics server stopped", "error", err)
		}
	}()

	s.log.Info("metrics are available", "addr", listener.Addr().String(), "path", "/metrics")
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

var _ ports.Runnable = (*Server)(nil)
