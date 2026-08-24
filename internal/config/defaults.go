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
			Mode:      ModeDryRun,
			LogLevel:  "info",
			LogFormat: "text",
			// The file sink is off by default: a local run has a terminal, and
			// writing to a path nobody asked for is how stray files appear in
			// repositories. Deployment turns it on through config.yaml.
			LogFile:       "",
			LogMaxSizeMB:  50,
			LogMaxFiles:   10,
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
			BandBps: 10,
			// 200, and this is now an upper-bound proof rather than a reading
			// of the campaign page. The page contradicts itself — its formula
			// gives 200, one row of its table implies 120 — and the previous
			// value here followed the table on the strength of a single
			// micro-size reconciliation from 6 August, when the credited
			// numbers were 0.02 units wide and could not separate the two.
			//
			// The 18 August report can. Over hours 09-12 UTC the actor never
			// planned more than 87.402 units a side and never placed a rung
			// nearer than 7.0 bps, so on the 120 curve the weighted side has a
			// hard ceiling of 87.402 x 0.64 = 55.94 whatever the uptime.
			// StandX published P50 = 74.25...78.09 and P70 = 68.80...72.20 for
			// exactly those hours. The 120 curve cannot produce those numbers;
			// the 200 curve caps at 87.402 x 0.88 = 76.91 and brackets them.
			//
			// DO NOT REVERT WITHOUT AN EXCHANGE REPORT THAT BREAKS THIS BOUND.
			// See TestShippedRewardCurveFollowsTheMeasuredWeights and
			// PLAN.md section 12.1.
			MaxWeightPct: 200,
			MinWeightPct: 40,
			UptimeTiers: []UptimeTier{
				{ThresholdPct: 70, Multiplier: 1.0},
				{ThresholdPct: 50, Multiplier: 0.5},
			},
		},
		Risk: Risk{
			MaxDailyLossUSD: 10,
			// Консервативнее боевого значения намеренно, как и остальной блок:
			// умолчания не должны быть агрессивнее того, что явно прописано
			// в файле.
			UtilizationPct:       70,
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
		News: News{
			// On by default, unlike the reference feed: the feed needs a venue
			// chosen per instrument and can be wrong, while a scheduled release
			// is the same fact for every USD-quoted market on the board.
			Enabled:    true,
			Currencies: []string{"USD"},
			Impacts:    []string{"high"},
			// Fifteen minutes is not symmetry for its own sake. Ahead of the
			// print the book thins out and quotes get picked off by anyone
			// repositioning; after it the first minute is the move and the next
			// quarter hour is the argument about it, which is when the spread
			// is widest and the mark is least trustworthy.
			DefensiveBefore: 15 * time.Minute,
			DefensiveAfter:  15 * time.Minute,
			// Measured, not guessed. The CPI print of 12 August moved the mark
			// 48 bps in a single tick, and twelve of that day's fourteen touches
			// of a 7.4 bps quote fell between 12:30:04 and 12:31:57, in two
			// clusters: the print and the reaction a minute behind it. A
			// symmetric minute covers the first and misses the second entirely.
			// Nothing was touched before the release.
			HardBefore: time.Minute,
			HardAfter:  3 * time.Minute,
			// One and a half basis points of room inside the band edge: enough
			// for the three-rung ladder (0.8 bps wide) to still fit, so the
			// defensive posture keeps the presence that pays for the hour
			// instead of collapsing to a single order.
			DefensiveInsetBps: 1.5,
			// Halving is the conventional choice and here it is also the free
			// one — see the field's own note. Anything smaller starts to bite
			// the median sample that carries the 0.5x tier.
			DefensiveSizeMultiple: 0.5,
			Refresh:               2 * time.Hour,
			// Six hours of silence still leaves a calendar worth using — the
			// file covers the week — but past that a postponed release could
			// have gone unnoticed.
			MaxAge:   6 * time.Hour,
			Timeout:  15 * time.Second,
			FailOpen: true,
		},
		Health: Health{
			// Канал price обновляется заметно реже стакана: на живом рынке
			// наблюдалось около 0.3 сообщения в секунду, то есть шаг порядка
			// трёх секунд. Порог в 2 с давал непрерывные ложные срабатывания,
			// поэтому берём запас на три пропущенных обновления.
			// Значение уточняется по длительной записи, см. PLAN.md §12.9.
			MarkStale: 10 * time.Second,
			// Половина ширины полосы награды. Смещение ориентира внутри этого
			// предела не выводит уже стоящую ступень из полосы и не подводит
			// её к касанию — то есть держать ордера ещё честно. Дальше картина
			// перестаёт быть той, по которой они выставлялись.
			MarkStaleHoldBps:  5,
			ReconcileInterval: 5 * time.Second,
			AckTimeout:        2 * time.Second,
			HeartbeatInterval: time.Second,
		},
		Defaults: Symbol{
			Enabled: false,
			Economics: Economics{
				// Ноль означает «ставка пула для этого инструмента ещё
				// не измерена», и как умолчание он верен: новый символ
				// действительно не измерен, а ноль запирает работу в полный
				// тир до первого отчёта биржи. Оставлено намеренно.
				RewardRateLowerBoundUSDPerMakerHour: 0,
				// 22 — МОДЕЛЬНАЯ ОЦЕНКА, ИЗВЕСТНО ЗАВЫШЕННАЯ НА ДВА ПОРЯДКА.
				// Для XAU-USD измерено (PLAN.md §12.4, шесть закрытых дней):
				// награда 0.79…0.86 DUSD за Maker Hour, стоимость 0.21…0.23.
				// Боевой конфиг это переопределяет; здесь число оставлено
				// потому, что оно относится к инструменту, а не к программе,
				// и переносить золотой замер на неизмеренный символ нельзя.
				//
				// Сочетание «ставка задана, порог не задан» отвергает всё
				// подряд — отказ безопасен, но выглядит как поломка.
				// НЕ ВЫВОДИТЬ ЭТО ЧИСЛО ЗАНОВО ИЗ МОДЕЛИ: мерить по
				// mm_realized_pnl_usd, делённому на зачтённые часы.
				BreakEvenUSDPerMakerHour: 22,
			},
			Quote: Quote{
				LadderLayers:    3,
				LadderSpreadBps: 1.0,
				MinRest:         3200 * time.Millisecond, // ≥3с для зачёта Maker Points
				RequoteCooldown: 250 * time.Millisecond,
				// Три секунды: замер 18 августа 2026 показал возврат ступени
				// через 1.95 с после защитного снятия и залив ещё через 3.3 с.
				// Полутора секунд, принятых у сравнимого оператора, там
				// не хватило бы. Стоимость — около 1% времени ступени,
				// арифметика в quotes.Rules.RefillLockout.
				RefillLockout: 3 * time.Second,
				// Two ticks. On gold that is 0.46 bps, small next to the seven
				// the quote normally stands at, so the clamp binds only when the
				// book has run past a mark that has not caught up yet — which is
				// exactly the moment an order placed on the touch gets taken.
				PostOnlyBufferTicks: 2,
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
				// Минута спокойствия возвращает цель к полу. Короче — и обычное
				// дрожание требования начнёт двигать лесенку, ради чего
				// мёртвая зона и заведена; длиннее — и всплеск продолжает
				// стоить веса уже после того, как рынок успокоился.
				TargetRelaxAfter: time.Minute,
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
				// Half the window, the same ratio the one measured instrument
				// settled on (XAU-USD: 4m of an 8m window). A half-life only
				// means anything relative to the window's own timescale, so
				// the ratio is the default, not the number.
				//
				// Cannot be equal to Window: at that point the record releases
				// no faster than the raw window it replaced, which validate.go
				// rejects rather than accepting a no-op. An instrument that
				// shortens Window below this must shorten the half-life with
				// it — the rejection names both values.
				WindowHalfLife: 10 * time.Minute,
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
				Leverage: 4,
				// Измерено на живом счёте пятью пробами при плечах 4, 8 и 20 —
				// все дали одну и ту же долю (test/live/margin_test.go).
				InitialMarginPct:   10.542,
				MaxNetPositionBase: "0",
			},
			Reference: Reference{
				// Off by default: an external symbol that does not track the same
				// underlying is worse than no feed at all, because a wrong signal
				// is still acted upon. It has to be named per instrument and
				// switched on deliberately.
				Enabled:  false,
				Provider: ProviderBinanceFutures,
				Symbol:   "",
				// One mark tick, and for a reason worth stating: silence on a
				// book-ticker stream means the top of book did not change,
				// which is information rather than the absence of it. The
				// threshold is not there to notice a quiet market — it is there
				// to notice a socket that died without saying so, and past one
				// tick the official price supersedes the estimate anyway.
				//
				// Measured at two seconds this flapped once every two minutes
				// on gold, warning about a feed that was working perfectly.
				StaleAfter: 3 * time.Second,
			},
		},
		Symbols: map[string]Symbol{},
	}
}
