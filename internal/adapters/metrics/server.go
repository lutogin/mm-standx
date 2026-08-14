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
	// actors отвечает, тикают ли акторы. Пусто — проба говорит только
	// о способности процесса принимать соединения.
	actors ports.LivenessSource
	clock  ports.Clock
	log    *slog.Logger

	srv      *http.Server
	listener net.Listener
}

// NewServer создаёт сервер метрик. Пустой адрес отключает его.
func NewServer(addr string, p *Prometheus, actors ports.LivenessSource, clk ports.Clock, log *slog.Logger) *Server {
	return &Server{
		addr:    addr,
		handler: promhttp.HandlerFor(p.Registry(), promhttp.HandlerOpts{}),
		actors:  actors,
		clock:   clk,
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
	// Проба живости для супервизора.
	//
	// Прежде она отвечала только за способность процесса принимать соединения
	// и о торговле не говорила ничего — намеренно, потому что для торговли есть
	// сторож. Но сторожа нет в Docker (AGENTS.md: его заменяют restart плюс
	// сверка при старте), а значит в боевой поставке признак жизни торговли
	// не читал никто. 13 августа очередь актора переполнилась, торговля встала,
	// и контейнер отвечал healthy тридцать пять минут подряд.
	//
	// Теперь проба говорит о том, ради чего процесс запущен. Замерший актор
	// делает контейнер unhealthy, а перезапуск проходит штатным путём: съём
	// котировок и закрытие позиции укладываются в stop_grace_period.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if s.actors != nil {
			if symbol, silent, stale := s.actors.Stalest(s.clock.Now()); stale {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "actor %s has not ticked for %s\n",
					symbol, silent.Truncate(time.Millisecond))
				return
			}
		}
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
