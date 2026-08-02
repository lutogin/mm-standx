package di

import (
	"context"
	"log/slog"
	"sort"

	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/adapters/standx/auth"
	"github.com/nd/mm-standx/internal/adapters/standx/wsapi"
	"github.com/nd/mm-standx/internal/adapters/standx/wsstream"
	"github.com/nd/mm-standx/internal/app/observer"
	"github.com/nd/mm-standx/internal/app/watchdog"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/events"
	"github.com/nd/mm-standx/internal/ports"
)

// Runtime — компоненты с жизненным циклом.
//
// Все они складываются в группу runnables, поэтому добавление нового компонента
// не требует правки функции запуска: достаточно объявить провайдер с тем же тегом.
var Runtime = fx.Module("runtime",
	fx.Provide(
		fx.Annotate(provideStreams, fx.ResultTags(`group:"runnables,flatten"`)),
		fx.Annotate(provideWatchdog, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideObserver, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
	),
	fx.Invoke(fx.Annotate(registerRunnables, fx.ParamTags(``, ``, `group:"runnables"`))),
)

// provideStreams собирает потоки данных.
//
// Рыночные и пользовательские каналы разнесены по разным соединениям: отказ
// авторизации не должен лишать нас справочной цены, по которой принимаются
// решения о съёме котировок.
func provideStreams(
	cfg *config.Config,
	wallet *auth.Wallet,
	tokens ports.TokenProvider,
	orderAPI *wsapi.Client,
	bus *events.Bus,
	clk ports.Clock,
	rec ports.Recorder,
	log *slog.Logger,
) []ports.Runnable {
	symbols := sortedSymbols(cfg)

	public := make([]wsstream.Subscription, 0, len(symbols)*2)
	for _, s := range symbols {
		public = append(public,
			wsstream.Subscription{Channel: wsstream.ChannelPrice, Symbol: s},
			wsstream.Subscription{Channel: wsstream.ChannelDepthBook, Symbol: s},
		)
	}

	streams := []ports.Runnable{
		wsstream.New("market", cfg.Exchange.WSStreamURL, cfg.Exchange, public, nil, bus, clk, rec, log),
	}

	// Пользовательские каналы требуют учётных данных. Без кошелька процесс
	// работает в режиме наблюдения, а не падает: это штатный сценарий до
	// открытия счёта.
	if wallet == nil {
		log.Info("пользовательские каналы отключены: кошелёк не задан")
		return streams
	}

	userChannels := []wsstream.Subscription{
		{Channel: wsstream.ChannelOrder},
		{Channel: wsstream.ChannelPosition},
		{Channel: wsstream.ChannelBalance},
		{Channel: wsstream.ChannelTrade},
	}
	// Пользовательские каналы молчат, пока ничего не происходит, а живость
	// соединения определяется по потоку сообщений: пинговать нельзя, сервер
	// на клиентские пинги не отвечает. Подписка на цену одного инструмента
	// превращает молчащее соединение в самодиагностируемое ценой нескольких
	// сообщений в секунду.
	if len(symbols) > 0 {
		userChannels = append(userChannels,
			wsstream.Subscription{Channel: wsstream.ChannelPrice, Symbol: symbols[0]})
	}
	streams = append(streams,
		wsstream.New("user", cfg.Exchange.WSStreamURL, cfg.Exchange, userChannels, tokens, bus, clk, rec, log))

	// Торговый канал поднимается только в режиме live: держать авторизованное
	// соединение, через которое нечего отправлять, незачем.
	if orderAPI != nil {
		streams = append(streams, orderAPI.Runnable())
	}
	return streams
}

func provideWatchdog(cfg *config.Config, bus *events.Bus, clk ports.Clock, log *slog.Logger) *watchdog.Watchdog {
	return watchdog.New(cfg.Health.MarkStale, bus, clk, log)
}

func provideObserver(cfg *config.Config, bus *events.Bus, clk ports.Clock, log *slog.Logger) *observer.Observer {
	return observer.New(cfg.Runtime.StatsInterval, bus, clk, log)
}

// registerRunnables связывает компоненты с жизненным циклом приложения.
//
// fx останавливает хуки в порядке, обратном регистрации, поэтому потоки данных
// закрываются последними — прикладные компоненты успевают доработать на живых
// данных до разрыва соединений.
func registerRunnables(lc fx.Lifecycle, log *slog.Logger, runnables []ports.Runnable) {
	for _, r := range runnables {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				log.Debug("запуск компонента", "name", r.Name())
				return r.Start(ctx)
			},
			OnStop: func(ctx context.Context) error {
				log.Debug("остановка компонента", "name", r.Name())
				return r.Stop(ctx)
			},
		})
	}
}

// sortedSymbols возвращает включённые инструменты в устойчивом порядке:
// случайный обход карты сделал бы логи запуска невоспроизводимыми.
func sortedSymbols(cfg *config.Config) []market.Symbol {
	enabled := cfg.EnabledSymbols()
	out := make([]market.Symbol, 0, len(enabled))
	for s := range enabled {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
