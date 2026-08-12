// Command recorder собирает рыночные данные StandX в файлы для последующего
// воспроизведения.
//
// Записи нужны интеграционным тестам: fake-биржа проигрывает настоящую траекторию
// mark price, поэтому измеренные на стенде аптайм и частота заливов отражают
// реальный рынок, а не выдуманный. Данные накапливаются только во времени,
// поэтому запись стоит запускать как можно раньше.
//
// Учётные данные не используются: пишутся только публичные каналы.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/di"
	"github.com/nd/mm-standx/internal/secret"
)

const shutdownTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to the configuration file")
	outputDir := flag.String("out", "", "output directory for recordings (defaults to runtime.recordingDir)")
	duration := flag.Duration("duration", 0, "recording duration; 0 means run until terminated")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load configuration:\n%v\n", err)
		os.Exit(1)
	}

	if *outputDir != "" {
		cfg.Runtime.RecordingDir = *outputDir
	}
	if cfg.Runtime.RecordingDir == "" {
		fmt.Fprintln(os.Stderr, "no output directory: pass -out or set runtime.recordingDir")
		os.Exit(1)
	}

	// Режим и учётные данные принудительно сбрасываются: рекордер обязан быть
	// неспособен отправить ордер, даже если конфиг говорит обратное.
	cfg.Runtime.Mode = config.ModeDryRun
	cfg.Exchange.WalletPrivateKey = secret.New("")

	if len(cfg.EnabledSymbols()) == 0 {
		fmt.Fprintln(os.Stderr, "no symbol is enabled in the configuration, nothing to record")
		os.Exit(1)
	}

	opts := []fx.Option{
		fx.Supply(cfg),
		di.Core,
		di.StandX,
		di.Runtime,
		fx.StopTimeout(shutdownTimeout),
	}
	// Запись на фиксированный срок удобна для сбора по расписанию: процесс
	// сам завершается, корректно закрыв файл, и не требует внешнего таймера.
	if *duration > 0 {
		opts = append(opts, fx.Invoke(stopAfter(*duration)))
	}

	fx.New(opts...).Run()
}

// stopAfter регистрирует отложенное завершение приложения.
func stopAfter(d time.Duration) func(fx.Lifecycle, fx.Shutdowner) {
	return func(lc fx.Lifecycle, shutdowner fx.Shutdowner) {
		var timer *time.Timer
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error {
				timer = time.AfterFunc(d, func() { _ = shutdowner.Shutdown() })
				return nil
			},
			OnStop: func(context.Context) error {
				if timer != nil {
					timer.Stop()
				}
				return nil
			},
		})
	}
}
