// Command mmbot — маркет-мейкинг бот для программы StandX Community Maker Yield.
//
// На текущей фазе процесс подключается к потокам данных, ведёт сводную статистику
// и пишет рыночные данные на диск. Торговый контур подключается следующей фазой,
// см. PLAN.md §11.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/di"
)

// shutdownTimeout — сколько ждём корректного завершения.
//
// Запас нужен на снятие котировок: оставить ордера в стакане при выходе нельзя,
// у StandX нет отмены по разрыву соединения.
const shutdownTimeout = 15 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "путь к файлу конфигурации")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "конфигурация не загружена:\n%v\n", err)
		os.Exit(1)
	}

	fx.New(
		fx.Supply(cfg),
		di.Core,
		di.StandX,
		di.Runtime,
		fx.StopTimeout(shutdownTimeout),
	).Run()
}
