// Command guardian — сторожевой процесс, снимающий котировки, если бот умер.
//
// StandX не отменяет ордера при разрыве соединения. Значит любой способ, каким
// основной процесс может исчезнуть — паника, OOM, kill -9, отключение питания
// машины, — оставляет котировки в стакане, где они продолжат исполняться,
// а следить за ними будет некому.
//
// Сторож умышленно устроен предельно просто: он читает файл биения и, если тот
// перестал обновляться, снимает все ордера по перечисленным в нём инструментам.
// Никакой торговой логики здесь нет и быть не должно — компонент последней
// надежды обязан быть тем, что ломается в последнюю очередь.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/adapters/standx/rest"
	"github.com/nd/mm-standx/internal/app/heartbeat"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/di"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/trading"
	"github.com/nd/mm-standx/internal/ports"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	timeout := flag.Duration("timeout", 15*time.Second,
		"how long the heartbeat may be silent before quotes are withdrawn")
	interval := flag.Duration("interval", 2*time.Second, "how often the heartbeat is checked")
	flag.Parse()

	if err := run(*configPath, *timeout, *interval); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, timeout, interval time.Duration) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration:\n%w", err)
	}
	cfg.Runtime.RecordingDir = ""

	if cfg.Runtime.HeartbeatPath == "" {
		return errors.New("runtime.heartbeatPath is not configured, nothing to watch")
	}

	var (
		client *rest.Client
		log    *slog.Logger
	)
	app := fx.New(
		fx.Supply(cfg),
		fx.NopLogger,
		di.Core,
		di.StandX,
		fx.Populate(&client, &log),
	)
	if err := app.Err(); err != nil {
		return fmt.Errorf("failed to build dependencies: %w", err)
	}
	log = log.With("component", "guardian")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("watching heartbeat",
		"path", cfg.Runtime.HeartbeatPath, "timeout", timeout, "interval", interval)

	watch(ctx, cfg, client, log, timeout, interval)
	return nil
}

func watch(
	ctx context.Context,
	cfg *config.Config,
	client *rest.Client,
	log *slog.Logger,
	timeout, interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// tripped не даёт снимать котировки на каждом тике после срабатывания:
	// один раз сняли — дальше ждём возвращения бота.
	tripped := false

	for {
		select {
		case <-ctx.Done():
			log.Info("guardian stopped")
			return
		case <-ticker.C:
		}

		state, err := heartbeat.Read(cfg.Runtime.HeartbeatPath)
		if err != nil {
			// Отсутствие файла означает штатное завершение: бот удаляет его,
			// сняв котировки самостоятельно.
			if os.IsNotExist(err) {
				tripped = false
				continue
			}
			log.Warn("failed to read heartbeat", "error", err)
			continue
		}

		age := time.Since(state.At)
		if age <= timeout {
			if tripped {
				log.Info("heartbeat resumed", "pid", state.PID)
			}
			tripped = false
			continue
		}
		if tripped {
			continue
		}

		log.Error("heartbeat is stale, withdrawing quotes",
			"age", age.Round(time.Second), "pid", state.PID, "symbols", state.Symbols)
		withdraw(ctx, client, log, state.Symbols)
		tripped = true
	}
}

// withdraw снимает все висящие ордера по перечисленным инструментам.
func withdraw(ctx context.Context, client *rest.Client, log *slog.Logger, symbols []string) {
	for _, name := range symbols {
		symbol := market.Symbol(name)

		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		orders, err := client.OpenOrders(reqCtx, symbol)
		if err != nil {
			cancel()
			log.Error("failed to read open orders", "symbol", symbol, "error", err)
			continue
		}
		if len(orders) == 0 {
			cancel()
			continue
		}

		ids := make([]trading.ClOrdID, 0, len(orders))
		for _, o := range orders {
			ids = append(ids, o.ClOrdID)
		}
		err = client.CancelBatch(reqCtx, ids, ports.PrioritySafety)
		cancel()

		if err != nil {
			log.Error("failed to withdraw quotes", "symbol", symbol, "count", len(ids), "error", err)
			continue
		}
		log.Warn("quotes withdrawn by guardian", "symbol", symbol, "count", len(ids))
	}
}
