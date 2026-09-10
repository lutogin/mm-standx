package di

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"go.uber.org/fx"

	"github.com/nd/mm-standx/internal/adapters/metrics"
	"github.com/nd/mm-standx/internal/adapters/reference"
	"github.com/nd/mm-standx/internal/adapters/standx/auth"
	"github.com/nd/mm-standx/internal/adapters/standx/wsapi"
	"github.com/nd/mm-standx/internal/adapters/standx/wsstream"
	"github.com/nd/mm-standx/internal/app/account"
	"github.com/nd/mm-standx/internal/app/engine"
	"github.com/nd/mm-standx/internal/app/heartbeat"
	"github.com/nd/mm-standx/internal/app/liveness"
	"github.com/nd/mm-standx/internal/app/news"
	"github.com/nd/mm-standx/internal/app/observer"
	"github.com/nd/mm-standx/internal/app/risk"
	"github.com/nd/mm-standx/internal/app/stats"
	"github.com/nd/mm-standx/internal/app/telemetry"
	"github.com/nd/mm-standx/internal/app/watchdog"
	"github.com/nd/mm-standx/internal/config"
	"github.com/nd/mm-standx/internal/domain/economics"
	"github.com/nd/mm-standx/internal/domain/liquidity"
	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/reward"
	"github.com/nd/mm-standx/internal/domain/volatility"
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
		fx.Annotate(provideReferenceFeeds, fx.ResultTags(`group:"runnables,flatten"`)),
		fx.Annotate(provideNewsPoller, fx.ResultTags(`group:"runnables,flatten"`)),
		fx.Annotate(provideMetricsServer, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideRiskGuard, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideWatchdog, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideObserver, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideTelemetry, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideStatsCollector, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideAccountPoller, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		provideBeacon,
		fx.Annotate(provideHeartbeat, fx.As(new(ports.Runnable)), fx.ResultTags(`group:"runnables"`)),
		fx.Annotate(provideEngines, fx.ResultTags(`group:"runnables,flatten"`)),
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
		log.Info("user channels disabled: no wallet configured")
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

// provideReferenceFeeds поднимает по одному соединению с внешней площадкой
// на каждый инструмент, для которого она названа.
//
// Соединение на инструмент, а не одно общее: площадки различаются, символы
// тоже, и разрыв ориентира по одному инструменту не должен трогать другой.
// Инструментов у нас единицы, поэтому экономить сокеты незачем.
func provideReferenceFeeds(
	cfg *config.Config,
	bus *events.Bus,
	clk ports.Clock,
	rec ports.Recorder,
	log *slog.Logger,
) ([]ports.Runnable, error) {
	var feeds []ports.Runnable
	for _, symbol := range sortedSymbols(cfg) {
		ref := cfg.Symbols[symbol.String()].Reference
		if !ref.Enabled {
			continue
		}
		feed, err := reference.New(symbol, ref, cfg.Exchange, bus, clk, rec, log)
		if err != nil {
			return nil, fmt.Errorf("reference feed for %s: %w", symbol, err)
		}
		log.Info("external reference feed enabled",
			"symbol", symbol, "provider", ref.Provider, "venue_symbol", ref.Symbol)
		feeds = append(feeds, feed)
	}
	return feeds, nil
}

// provideNewsPoller поднимает чтение календаря экономических публикаций.
//
// Один поллер на процесс, а не на инструмент: расписание общее, а публикация
// CPI — один и тот же факт для каждого долларового рынка на доске.
//
// Возвращается срез, чтобы выключенный календарь просто не давал компонента:
// пустой поллер, исправно опрашивающий никого, выглядел бы в логе работающим.
func provideNewsPoller(
	cfg *config.Config,
	source ports.NewsSource,
	bus *events.Bus,
	clk ports.Clock,
	log *slog.Logger,
) []ports.Runnable {
	if !cfg.News.Enabled {
		log.Info("news blackout disabled: scheduled releases will not stop quoting")
		return nil
	}
	return []ports.Runnable{news.New(news.Config{
		Refresh: cfg.News.Refresh,
		MaxAge:  cfg.News.MaxAge,
		Filter:  cfg.News.Filter(),
		Policy:  cfg.News.Policy(),
		Timeout: cfg.News.Timeout,
	}, source, bus, clk, log)}
}

func provideWatchdog(cfg *config.Config, bus *events.Bus, clk ports.Clock, log *slog.Logger) *watchdog.Watchdog {
	return watchdog.New(sortedSymbols(cfg), cfg.Health.MarkStale, bus, clk, log)
}

func provideMetricsServer(
	cfg *config.Config,
	p *metrics.Prometheus,
	beacon *liveness.Beacon,
	clk ports.Clock,
	log *slog.Logger,
) *metrics.Server {
	return metrics.NewServer(cfg.Runtime.MetricsAddr, p, beacon, clk, log)
}

// provideRiskGuard собирает защитный контур.
//
// Опрос идёт с шагом сверки: вердикт зависит от времени (скользящий час,
// граница суток), поэтому проверять его только по событиям недостаточно —
// час может истечь и без единого исполнения.
func provideRiskGuard(
	cfg *config.Config,
	store ports.StateStore,
	bus *events.Bus,
	clk ports.Clock,
	m ports.Metrics,
	log *slog.Logger,
) *risk.Guard {
	return risk.New(cfg.Risk, cfg.Health.ReconcileInterval, store, bus, clk, m, log)
}

func provideObserver(cfg *config.Config, bus *events.Bus, clk ports.Clock, log *slog.Logger) *observer.Observer {
	return observer.New(cfg.Runtime.StatsInterval, bus, clk, log)
}

// provideStatsCollector собирает выгрузку истории показателей на диск.
func provideStatsCollector(
	cfg *config.Config,
	p *metrics.Prometheus,
	clk ports.Clock,
	log *slog.Logger,
) *stats.Collector {
	return stats.New(cfg.Runtime.StatsDir, cfg.Runtime.StatsSample, p, clk, log)
}

func provideTelemetry(
	cfg *config.Config,
	limiter ports.RateLimiter,
	bus *events.Bus,
	clk ports.Clock,
	m ports.Metrics,
	log *slog.Logger,
) *telemetry.Collector {
	return telemetry.New(cfg.Health.HeartbeatInterval, limiter, bus, clk, m, log)
}

// registerRunnables связывает компоненты с жизненным циклом приложения.
//
// Порядок здесь не косметика. fx останавливает хуки в порядке, обратном
// регистрации, а акторы обязаны успеть снять котировки, пока соединения ещё
// живы: у StandX нет отмены ордеров по разрыву, и оставленная котировка
// продолжит исполняться после выхода процесса. Поэтому акторы регистрируются
// последними и останавливаются первыми.
func registerRunnables(lc fx.Lifecycle, log *slog.Logger, runnables []ports.Runnable) {
	sort.SliceStable(runnables, func(i, j int) bool {
		return stopRank(runnables[i]) < stopRank(runnables[j])
	})

	for _, r := range runnables {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				log.Debug("starting component", "name", r.Name())
				return r.Start(ctx)
			},
			OnStop: func(ctx context.Context) error {
				log.Debug("stopping component", "name", r.Name())
				return r.Stop(ctx)
			},
		})
	}
}

func provideAccountPoller(
	cfg *config.Config,
	reader ports.ExchangeReader,
	bus *events.Bus,
	clk ports.Clock,
	log *slog.Logger,
) *account.Poller {
	return account.New(sortedSymbols(cfg), cfg.Health.ReconcileInterval, reader, bus, clk, log)
}

// provideBeacon заводит маяк живости акторов.
func provideBeacon(cfg *config.Config, clk ports.Clock) *liveness.Beacon {
	return liveness.New(sortedSymbols(cfg), actorStale(cfg.Exchange), clk.Now())
}

// actorStale — сколько молчания актора означает, что торговли больше нет.
//
// Соблазнительно взять величину порядка такта актора: тикает он раз в 250 мс,
// и пять секунд выглядят как двадцать пропущенных тактов подряд, то есть как
// очевидная смерть. Это неверно, и ошибка дорогая.
//
// Актор ходит в сеть синхронно, своей же горутиной (PLAN.md §0, дефект 5).
// Один вызов шлюза по худшему пути стоит четырёх попыток по requestTimeout
// каждая, плюс отступы 200/400/800 мс с джиттером, плюс ожидание кредита
// у шейпера: при нынешних пяти секундах таймаута это **больше двадцати секунд
// молчания у совершенно здорового актора**. Порог ниже объявил бы его мёртвым,
// `/healthz` вернул бы 503, и Docker перезапустил бы работающего бота — а
// перезапуск в середине часа стоит всего накопленного аптайма (domain/uptime:
// дотиковое состояние не переживает старт намеренно).
//
// Отсюда шесть таймаутов запроса: покрывает худший путь с запасом и остаётся
// в двух порядках от тридцати пяти минут, которые этот механизм и должен был
// заметить 13 августа. Обнаружить за полминуты вместо мгновенного — приемлемо;
// убить исправного — нет.
func actorStale(e config.Exchange) time.Duration {
	const cover = 6
	if e.RequestTimeout <= 0 {
		return 30 * time.Second
	}
	return max(cover*e.RequestTimeout, 30*time.Second)
}

func provideHeartbeat(
	cfg *config.Config,
	beacon *liveness.Beacon,
	clk ports.Clock,
	log *slog.Logger,
) *heartbeat.Writer {
	return heartbeat.New(
		cfg.Runtime.HeartbeatPath,
		cfg.Health.HeartbeatInterval,
		sortedSymbols(cfg),
		cfg.IsLive(),
		beacon,
		clk,
		log,
	)
}

// stopRank задаёт очерёдность запуска: чем меньше, тем раньше стартует
// и тем позже останавливается.
func stopRank(r ports.Runnable) int {
	if strings.HasPrefix(r.Name(), "engine[") {
		return 1
	}
	return 0
}

// provideEngines создаёт актора на каждый включённый инструмент.
//
// Спецификации инструментов читаются с биржи на старте: тики и минимальные
// размеры нельзя держать в конфигурации, потому что биржа их меняет,
// а расхождение приводит к отклонённым ордерам.
func provideEngines(
	cfg *config.Config,
	reader ports.ExchangeReader,
	gateway ports.OrderGateway,
	bus *events.Bus,
	clk ports.Clock,
	m ports.Metrics,
	beacon *liveness.Beacon,
	store ports.StateStore,
	log *slog.Logger,
) ([]ports.Runnable, error) {
	symbols := sortedSymbols(cfg)
	if len(symbols) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Плечо счёта приводится к настроенному до сборки акторов: расчёт размера
	// исходит из него, а действует оно только будучи выставленным на счёте —
	// поле leverage в ордере биржа игнорирует (PLAN.md §12.5).
	if configurer, ok := reader.(ports.PositionConfigurer); ok && cfg.IsLive() {
		wanted := make(map[market.Symbol]int, len(symbols))
		for _, symbol := range symbols {
			wanted[symbol] = cfg.Symbols[symbol.String()].SymbolRisk.Leverage
		}
		account.ApplyLeverage(ctx, configurer, wanted, log)
	}

	out := make([]ports.Runnable, 0, len(symbols))
	for _, symbol := range symbols {
		symCfg := cfg.Symbols[symbol.String()]

		spec, err := reader.SymbolSpec(ctx, symbol)
		if err != nil {
			return nil, fmt.Errorf("load specification for %s: %w", symbol, err)
		}
		program, err := buildProgram(cfg.Reward, symCfg.Program)
		if err != nil {
			return nil, fmt.Errorf("reward programme for %s: %w", symbol, err)
		}
		maxPosition, err := money.ParseQty(symCfg.SymbolRisk.MaxNetPositionBase)
		if err != nil {
			return nil, fmt.Errorf("max net position for %s: %w", symbol, err)
		}

		out = append(out, engine.New(
			engine.Params{
				Symbol:  symbol,
				Spec:    spec,
				Config:  symCfg,
				Health:  cfg.Health,
				Risk:    cfg.Risk,
				Live:    cfg.IsLive(),
				Program: program,
				Appetite: policy.Appetite{
					JumpMultiple:       symCfg.Appetite.JumpMultiple,
					FloorMarginBps:     symCfg.Appetite.FloorMarginBps,
					DeadBandJumps:      symCfg.Appetite.DeadBandJumps,
					MinDeadBandBps:     symCfg.Appetite.MinDeadBandBps,
					PanicFraction:      symCfg.Appetite.PanicFraction,
					ReentryDeadBandBps: symCfg.Appetite.ReentryDeadBandBps,
				},
				Bounds: policy.Bounds{MinBps: symCfg.Appetite.MinBps, MaxBps: symCfg.Appetite.MaxBps},
				Liquidity: liquidity.Config{
					Window:          symCfg.Liquidity.Window,
					SampleEvery:     symCfg.Liquidity.SampleEvery,
					MinSamples:      symCfg.Liquidity.MinSamples,
					ThinFraction:    symCfg.Liquidity.ThinFraction,
					SevereFraction:  symCfg.Liquidity.SevereFraction,
					ThinAfter:       symCfg.Liquidity.ThinAfter,
					ShortWindow:     symCfg.Liquidity.ShortWindow,
					RecoverFraction: symCfg.Liquidity.RecoverFraction,
					RecoverAfter:    symCfg.Liquidity.RecoverAfter,
				},
				Volatility: volatility.Config{
					Window:               symCfg.Volatility.Window,
					MinSamples:           symCfg.Volatility.MinSamples,
					FullConfidenceAt:     symCfg.Volatility.FullConfidenceAt,
					WindowHalfLife:       symCfg.Volatility.WindowHalfLife,
					MemoryHalfLife:       symCfg.Volatility.MemoryHalfLife,
					MemoryWindowMultiple: symCfg.Volatility.MemoryWindowMultiple,
				},
				Economics: economics.Gate{
					RewardRateLowerBoundUSDPerMakerHour: symCfg.Economics.RewardRateLowerBoundUSDPerMakerHour,
					BreakEvenUSDPerMakerHour:            symCfg.Economics.BreakEvenUSDPerMakerHour,
				},
				// Ноль, когда ориентир для инструмента не поднят: путь быстрого
				// сигнала тогда не задействуется вовсе, а это отличается
				// от «настроен и молчит».
				ReferenceStaleAfter: referenceStaleAfter(symCfg.Reference),
				News: engine.NewsGuard{
					Enabled:               cfg.News.Enabled,
					MaxAge:                cfg.News.MaxAge,
					FailOpen:              cfg.News.FailOpen,
					DefensiveInsetBps:     cfg.News.DefensiveInsetBps,
					DefensiveSizeMultiple: cfg.News.DefensiveSizeMultiple,
				},
				MaxNetPosition: maxPosition,
			},
			engine.Deps{
				Gateway:  gateway,
				Reader:   reader,
				Bus:      bus,
				Clock:    clk,
				Metrics:  m,
				Liveness: beacon,
				Store:    store,
				Log:      log,
			},
		))
	}
	return out, nil
}

// referenceStaleAfter отдаёт порог молчания только для поднятого ориентира.
//
// Выключенный фид обязан выглядеть в акторе как отсутствующий, а не как
// молчащий: иначе выключение оставляло бы в сводке и метриках вечно устаревший
// ориентир, а тревога о нём приходила бы каждый раз, когда его никто не ждёт.
func referenceStaleAfter(ref config.Reference) time.Duration {
	if !ref.Enabled {
		return 0
	}
	return ref.StaleAfter
}

func buildProgram(rewardCfg config.Reward, p config.Program) (reward.Program, error) {
	unit, err := decimal.NewFromString(p.UnitSize)
	if err != nil {
		return reward.Program{}, fmt.Errorf("unit size %q: %w", p.UnitSize, err)
	}
	cap, err := decimal.NewFromString(p.PerSideCap)
	if err != nil {
		return reward.Program{}, fmt.Errorf("per-side cap %q: %w", p.PerSideCap, err)
	}

	schedule, err := p.Schedule()
	if err != nil {
		return reward.Program{}, fmt.Errorf("off-hours schedule: %w", err)
	}

	tiers := make([]reward.Tier, 0, len(rewardCfg.UptimeTiers))
	for _, t := range rewardCfg.UptimeTiers {
		tiers = append(tiers, reward.Tier{ThresholdPct: t.ThresholdPct, Multiplier: t.Multiplier})
	}

	return reward.Program{
		Curve: reward.Curve{
			BandBps:      rewardCfg.BandBps,
			MaxWeightPct: rewardCfg.MaxWeightPct,
			MinWeightPct: rewardCfg.MinWeightPct,
		},
		Tiers:                tiers,
		UnitSize:             unit,
		PerSideCap:           cap,
		MaxMakerHoursPerHour: p.MaxMakerHoursPerHour,
		OffHours:             schedule,
		OffHoursMultiplier:   p.OffHoursMultiplier,
	}, nil
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
