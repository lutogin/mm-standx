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
			MaxDailyLossUSD:      500,
			MaxNotionalUSD:       120_000,
			MaxFillsPerHour:      20,
			MarginMode:           trading.Isolated,
			FlattenPolicy:        "immediate_market",
			FlattenDebounce:      150 * time.Millisecond,
			PostFillCooldown:     3 * time.Second,
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
				MinDeadBandBps: 0.8,
				MinBps:         5.5,
				MaxBps:         9.5,
			},
			Volatility: Volatility{
				// Окно в двадцать минут при такте в три секунды даёт до 400
				// замеров; порог в 40 — это две минуты прогрева, 100 — пять
				// минут до зрелой оценки.
				Window:           20 * time.Minute,
				MinSamples:       40,
				FullConfidenceAt: 100,
				// Полураспад в шесть часов переносит вечернюю волатильность
				// через ночь и пятничную — через выходные, оставляя от неё
				// к понедельнику около одной восьмой. Спокойный рынок при этом
				// всё равно позволяет подойти ближе, только не за один час.
				MemoryHalfLife: 6 * time.Hour,
			},
			Program: Program{
				UnitSize:             "0",
				PerSideCap:           "0",
				MaxMakerHoursPerHour: 0,
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
