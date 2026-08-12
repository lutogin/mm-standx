// Command mmbot — маркет-мейкинг бот для программы StandX Community Maker Yield.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/di"
)

// shutdownTimeout — сколько ждём корректного завершения.
//
// Запас нужен на снятие котировок: оставить ордера в стакане при выходе нельзя,
// у StandX нет отмены по разрыву соединения.
const shutdownTimeout = 20 * time.Second

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to the configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load configuration:\n%v\n", err)
		os.Exit(1)
	}

	announce(cfg)

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// announce печатает, что именно сейчас будет запущено, до всякой инициализации.
//
// Единственная защита от запуска не того, что ожидалось, — увидеть это раньше,
// чем уйдёт первый ордер. Технической защиты здесь быть не может: режим берётся
// из файла, а любой второй признак пришлось бы держать в том же файле и он
// залипал бы после первого боевого запуска.
//
// Поэтому сводка идёт в stderr отдельным блоком, а не строкой лога: она обязана
// быть видна и когда логи в JSON, и когда их перехватывает docker, и когда
// человек запускает бот вручную и смотрит в терминал.
func announce(cfg *config.Config) {
	symbols := make([]string, 0, len(cfg.Symbols))
	total := 0.0
	for name, sym := range cfg.Symbols {
		if sym.Enabled {
			symbols = append(symbols, fmt.Sprintf("%s (%.0f USD × %d)",
				name, sym.AllocationUSD, sym.SymbolRisk.Leverage))
			total += sym.AllocationUSD * float64(sym.SymbolRisk.Leverage)
		}
	}
	sort.Strings(symbols)

	banner := "DRY RUN — no orders reach the exchange"
	if cfg.IsLive() {
		banner = "LIVE — real orders, real money"
	}

	fmt.Fprintf(os.Stderr, `
┌─────────────────────────────────────────────────────────────
│ %s
│ symbols:      %s
│ notional:     up to %.0f USD
│ risk limits:  halt at %.0f USD daily loss, %d fills per hour
└─────────────────────────────────────────────────────────────

`, banner, strings.Join(symbols, ", "), total,
		cfg.Risk.MaxDailyLossUSD, cfg.Risk.MaxFillsPerHour)
}

func run(cfg *config.Config) error {
	app := fx.New(
		fx.Supply(cfg),
		di.Core,
		di.StandX,
		di.Runtime,
		fx.StopTimeout(shutdownTimeout),
	)
	if err := app.Err(); err != nil {
		return fmt.Errorf("failed to build dependencies: %w", err)
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), time.Minute)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		return fmt.Errorf("failed to start: %w", err)
	}

	// SIGHUP обрабатывается наравне с остальными намеренно. По умолчанию он
	// убивает процесс без всякой уборки, а приходит он в самом обыденном
	// случае — когда закрывают терминал, из которого бот запущен. Оставленные
	// при этом котировки продолжат исполняться: отмены по разрыву у StandX нет.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-stop

	stopCtx, cancelStop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelStop()

	fmt.Fprintf(os.Stderr, "received %s, shutting down\n", sig)
	if err := app.Stop(stopCtx); err != nil {
		return fmt.Errorf("failed to stop cleanly: %w", err)
	}
	return nil
}
