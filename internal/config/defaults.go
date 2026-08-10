package config

import (
	"time"

	"github.com/nd/mm-standx/internal/domain/trading"
)

// defaults задаёт безопасную базу: режим dry_run, консервативные дистанции,
// шейпер запросов заметно ниже биржевого лимита.
//
// Любое значение может быть переопределено файлом или окружением, но пропуск
// параметра никогда не приводит к более агрессивному поведению, чем здесь.
func defaults() *Config {
	return &Config{
		Runtime: Runtime{
			Mode:          ModeDryRun,
			LogLevel:      "info",
			LogFormat:     "text",
			MetricsAddr:   ":9090",
			HeartbeatPath: "data/mmbot.hb",
			RecordingDir:  "recordings",
			StatsInterval: 30 * time.Second,
			SessionPath:   "data/session.json",
			StateDir:      "data/state",
			StatsDir:      "data/stats",
			StatsSample:   15 * time.Second,
		},
		Exchange: Exchange{
			AuthBaseURL:      "https://api.standx.com",
			RESTBaseURL:      "https://perps.standx.com",
			WSStreamURL:      "wss://perps.standx.com/ws-stream/v1",
			WSAPIURL:         "wss://perps.standx.com/ws-api/v1",
			Chain:            "bsc",
			TokenTTL:         24 * time.Hour,
			TokenRefreshLead: 2 * time.Hour,
			WSRotateAfter:    20 * time.Hour, // биржа рвёт соединение на 24-м часе
			WSReconnectMin:   250 * time.Millisecond,
			WSReconnectMax:   30 * time.Second,
			// На рыночном потоке данные идут несколько раз в секунду, поэтому
			// полминуты тишины однозначно означают разрыв.
			WSIdleTimeout:       30 * time.Second,
			WSHeartbeatInterval: 10 * time.Second,
			RequestTimeout:      5 * time.Second,
		},
		RateLimit: RateLimit{
			SustainedRPS:     15, // биржевой предел ~22, оставляем запас
			Burst:            12,
			SafetyReservePct: 30,
		},
		Reward: Reward{
			BandBps:      10,
			MaxWeightPct: 200,
			MinWeightPct: 40,
			UptimeTiers: []UptimeTier{
				{ThresholdPct: 70, Multiplier: 1.0},
				{ThresholdPct: 50, Multiplier: 0.5},
			},
		},
		Risk: Risk{
			MaxDailyLossUSD:      10,
			MaxNotionalUSD:       120_000,
			MaxFillsPerHour:      6,
			MarginMode:           trading.Isolated,
			FlattenPolicy:        "immediate_market",
			FlattenDebounce:      0,
			PostFillCooldown:     3 * time.Second,
			FillBurstLimit:       2,
			FillBurstWindow:      10 * time.Minute,
			FillBurstCooldown:    20 * time.Minute,
			KillSwitchOnDrawdown: true,
			// Ставки одинаковы для всех рынков StandX, см. PLAN.md §1.3.
			Fees: Fees{MakerBps: 1, TakerBps: 4},
		},
		Health: Health{
			// Канал price обновляется заметно реже стакана: на живом рынке
			// наблюдалось около 0.3 сообщения в секунду, то есть шаг порядка
			// трёх секунд. Порог в 2 с давал непрерывные ложные срабатывания,
			// поэтому берём запас на три пропущенных обновления.
			// Значение уточняется по длительной записи, см. PLAN.md §12.9.
			MarkStale:         10 * time.Second,
			ReconcileInterval: 5 * time.Second,
			AckTimeout:        2 * time.Second,
			HeartbeatInterval: time.Second,
		},
		Defaults: Symbol{
			Enabled:       false,
			AllocationUSD: 0,
			Economics: Economics{
				RewardRateLowerBoundUSDPerMakerHour: 0,
				BreakEvenUSDPerMakerHour:            22,
				CalibrationMaxAllocationUSD:         60,
			},
			Quote: Quote{
				LadderLayers:    3,
				LadderSpreadBps: 1.0,
				MinRest:         3200 * time.Millisecond, // ≥3с для зачёта Maker Points
				RequoteCooldown: 250 * time.Millisecond,
			},
			Appetite: Appetite{
				// Мера риска — максимум скользящего окна, то есть уже оценка
				// хвоста, а не типичного движения. Множитель поэтому покрывает
				// только смену режима, а не толщину хвоста: удвоение считало бы
				// запас дважды и запрещало котировать в обычный будний день.
				JumpMultiple:   1.25,
				FloorMarginBps: 1.5,
				DeadBandJumps:  2.0,
				// Не меньше половины ширины лесенки: крайние ступени обязаны
				// помещаться в мёртвую зону, иначе они просят перестановки сразу
				// после размещения. Согласованность проверяется при запуске.
				MinDeadBandBps: 1.2,
				// Сторона снимается целиком, когда рынок съел половину пути
				// до котировки. Физический порог остаётся нижней границей.
				PanicFraction: 0.5,
				// Измерено 6-7 августа: половина спреда ходит от 0.12 до 3.5 bps
				// внутри часа, медиана около 1. Одного базисного пункта хватает,
				// чтобы обычный размах перестал перебрасывать порог, и мало,
				// чтобы заметно сократить время в рынке.
				ReentryDeadBandBps: 1.0,
				MinBps:             5.5,
				MaxBps:             9.5,
			},
			Volatility: Volatility{
				// Окно в двадцать минут при такте в три секунды даёт до 400
				// замеров; порог в 40 — это две минуты прогрева, 100 — пять
				// минут до зрелой оценки.
				Window:           20 * time.Minute,
				MinSamples:       40,
				FullConfidenceAt: 100,
				// Six hours was never derived. It was chosen to carry Friday's
				// volatility across the weekend and does not do that either:
				// 51 hours of decay leave 0.3% of the record. What it actually
				// does is carry an evening through the night — 10 hours leave
				// about a third.
				//
				// The number stays because the bound below, not the half-life,
				// is what keeps a single outlier from governing the estimate.
				// Revisit it once the price of a Maker Hour and the fill rate
				// are measured; until then it is a guess either way.
				MemoryHalfLife: 6 * time.Hour,
				// The record may claim at most three times what the live window
				// shows. Without the bound one outlier governs the estimate for a
				// fixed stretch of wall time regardless of what follows it.
				MemoryWindowMultiple: 3,
			},
			Program: Program{
				UnitSize:             "0",
				PerSideCap:           "0",
				MaxMakerHoursPerHour: 0,
				// One describes a 24/7 market. Instruments whose reference
				// market closes override this together with the window.
				OffHoursMultiplier: 1,
			},
			Uptime: Uptime{
				TargetPct:    80,
				HardFloorPct: 72,
				ReservePct:   8,
			},
			SymbolRisk: SymbolRisk{
				Leverage:           4,
				MaxNetPositionBase: "0",
			},
		},
		Symbols: map[string]Symbol{},
	}
}
