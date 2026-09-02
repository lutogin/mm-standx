package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/calendar"
	"github.com/nd/mm-standx/internal/domain/money"
	"github.com/nd/mm-standx/internal/domain/policy"
	"github.com/nd/mm-standx/internal/domain/trading"
)

// Validate проверяет конфигурацию целиком и возвращает все найденные проблемы сразу.
//
// Собираем полный список, а не падаем на первой ошибке: правка конфига по одному
// сообщению за запуск — плохой способ провести вечер.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.Runtime.validate()...)
	errs = append(errs, c.Exchange.validate(c.IsLive())...)
	errs = append(errs, c.RateLimit.validate()...)
	errs = append(errs, c.Reward.validate()...)
	errs = append(errs, c.Risk.validate()...)
	errs = append(errs, c.Health.validate(c.Reward.BandBps)...)
	errs = append(errs, c.News.validate()...)

	enabled := 0
	for name, sym := range c.Symbols {
		if sym.Enabled {
			enabled++
		}
		errs = append(errs, sym.validate(name, c.Reward.BandBps, c.Reward.UptimeTiers, c.IsLive())...)
	}
	if enabled == 0 && c.IsLive() {
		errs = append(errs, errors.New("runtime.mode=live but no symbol is enabled"))
	}

	if c.IsLive() && c.Runtime.StateDir == "" {
		errs = append(errs, errors.New(
			"runtime.stateDir is required in live mode: without it the daily loss counter "+
				"resets on every restart and the kill switch protects nothing"))
	}

	return errors.Join(errs...)
}

func (r Runtime) validate() []error {
	var errs []error
	// Режим приходит из MM_MODE, поэтому и жалоба называет переменную: искать
	// опечатку в конфигурации, которой там нет, — потерянные минуты на старте.
	if r.Mode != ModeDryRun && r.Mode != ModeLive {
		errs = append(errs, fmt.Errorf("%s: want %q or %q, got %q", modeEnv, ModeDryRun, ModeLive, r.Mode))
	}
	switch strings.ToLower(r.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("runtime.logLevel: unsupported value %q", r.LogLevel))
	}
	if r.LogFormat != "json" && r.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("runtime.logFormat: want json or text, got %q", r.LogFormat))
	}
	// The bounds are only checked once a path exists: an unbounded log on a
	// trading box fills the disk and kills the process that is holding orders
	// in the market, so "write to a file" and "cap the file" are one decision.
	if r.LogFile != "" {
		if r.LogMaxSizeMB <= 0 {
			errs = append(errs, fmt.Errorf(
				"runtime.logMaxSizeMb (%d) must be positive when runtime.logFile is set",
				r.LogMaxSizeMB))
		}
		if r.LogMaxFiles < 0 {
			errs = append(errs, fmt.Errorf(
				"runtime.logMaxFiles (%d) must not be negative", r.LogMaxFiles))
		}
	}
	if r.StatsInterval <= 0 {
		errs = append(errs, errors.New("runtime.statsInterval must be positive"))
	}
	// Свёртка часовая, поэтому шаг съёма обязан укладываться в час
	// достаточное число раз, чтобы квантили имели смысл.
	if r.StatsDir != "" && (r.StatsSample <= 0 || r.StatsSample > 5*time.Minute) {
		errs = append(errs, fmt.Errorf(
			"runtime.statsSample (%s) must be within (0, 5m] for hourly quantiles to mean anything",
			r.StatsSample))
	}
	return errs
}

func (e Exchange) validate(live bool) []error {
	var errs []error

	for name, raw := range map[string]string{
		"exchange.authBaseUrl": e.AuthBaseURL,
		"exchange.restBaseUrl": e.RESTBaseURL,
		"exchange.wsStreamUrl": e.WSStreamURL,
		"exchange.wsApiUrl":    e.WSAPIURL,
	} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: malformed URL %q", name, raw))
			continue
		}
		switch u.Scheme {
		case "http", "https", "ws", "wss":
		default:
			errs = append(errs, fmt.Errorf("%s: unsupported scheme %q", name, u.Scheme))
		}
	}

	if e.Chain != "bsc" && e.Chain != "solana" {
		errs = append(errs, fmt.Errorf("exchange.chain: want bsc or solana, got %q", e.Chain))
	}
	if live && e.WalletPrivateKey.IsEmpty() {
		errs = append(errs, errors.New("environment variable STANDX_WALLET_PK is required in live mode"))
	}
	if e.TokenRefreshLead >= e.TokenTTL {
		errs = append(errs, fmt.Errorf("exchange.tokenRefreshLead (%s) must be less than tokenTtl (%s)", e.TokenRefreshLead, e.TokenTTL))
	}
	// Биржа принудительно рвёт соединение на 24-м часе — ротируемся заведомо раньше.
	if e.WSRotateAfter <= 0 || e.WSRotateAfter >= 24*time.Hour {
		errs = append(errs, fmt.Errorf("exchange.wsRotateAfter (%s) must be within (0, 24h)", e.WSRotateAfter))
	}
	if e.WSReconnectMin <= 0 || e.WSReconnectMin > e.WSReconnectMax {
		errs = append(errs, errors.New("exchange.wsReconnectMin must be positive and not greater than wsReconnectMax"))
	}
	if e.RequestTimeout <= 0 {
		errs = append(errs, errors.New("exchange.requestTimeout must be positive"))
	}
	if e.WSIdleTimeout <= 0 {
		errs = append(errs, errors.New("exchange.wsIdleTimeout must be positive"))
	}
	// Heartbeat обязан успеть вызвать ответ до истечения дедлайна молчания,
	// иначе молчащий канал будет переподключаться бесконечно.
	if e.WSHeartbeatInterval <= 0 || e.WSHeartbeatInterval >= e.WSIdleTimeout {
		errs = append(errs, fmt.Errorf("exchange.wsHeartbeatInterval (%s) must be within (0, wsIdleTimeout=%s)",
			e.WSHeartbeatInterval, e.WSIdleTimeout))
	}
	return errs
}

func (r RateLimit) validate() []error {
	var errs []error
	// Биржевой предел ~22 rps на аккаунт; выше настраивать бессмысленно и вредно.
	if r.SustainedRPS <= 0 || r.SustainedRPS > 22 {
		errs = append(errs, fmt.Errorf("rateLimit.sustainedRps (%.2f) must be within (0, 22]", r.SustainedRPS))
	}
	if r.Burst <= 0 || r.Burst > 20 {
		errs = append(errs, fmt.Errorf("rateLimit.burst (%d) must be within (0, 20]", r.Burst))
	}
	if r.SafetyReservePct < 0 || r.SafetyReservePct >= 100 {
		errs = append(errs, fmt.Errorf("rateLimit.safetyReservePct (%.1f) must be within [0, 100)", r.SafetyReservePct))
	}
	return errs
}

func (r Reward) validate() []error {
	var errs []error
	if r.BandBps <= 0 {
		errs = append(errs, fmt.Errorf("reward.bandBps (%s) must be positive", r.BandBps))
	}
	if r.MaxWeightPct < r.MinWeightPct {
		errs = append(errs, fmt.Errorf("reward.maxWeightPct (%.1f) must not be less than minWeightPct (%.1f)", r.MaxWeightPct, r.MinWeightPct))
	}
	if r.MinWeightPct < 0 {
		errs = append(errs, errors.New("reward.minWeightPct must not be negative"))
	}
	if len(r.UptimeTiers) == 0 {
		errs = append(errs, errors.New("reward.uptimeTiers: at least one tier is required"))
	}
	// Пороги идут по убыванию: первым проверяется самый выгодный.
	for i, t := range r.UptimeTiers {
		if t.ThresholdPct <= 0 || t.ThresholdPct > 100 {
			errs = append(errs, fmt.Errorf("reward.uptimeTiers[%d].thresholdPct (%.1f) must be within (0, 100]", i, t.ThresholdPct))
		}
		if t.Multiplier <= 0 {
			errs = append(errs, fmt.Errorf("reward.uptimeTiers[%d].multiplier (%.2f) must be positive", i, t.Multiplier))
		}
		if i > 0 && t.ThresholdPct >= r.UptimeTiers[i-1].ThresholdPct {
			errs = append(errs, fmt.Errorf("reward.uptimeTiers must be ordered by descending threshold: [%d]=%.1f >= [%d]=%.1f",
				i, t.ThresholdPct, i-1, r.UptimeTiers[i-1].ThresholdPct))
		}
	}
	return errs
}

func (r Risk) validate() []error {
	var errs []error
	if r.MaxDailyLossUSD <= 0 {
		errs = append(errs, errors.New("risk.maxDailyLossUsd must be positive"))
	}
	// 100 не запрещается на бумаге, но означает отсутствие резерва под
	// закрывающий ордер после залива — оставлено как жёсткий верхний потолок,
	// а не как рекомендация.
	if r.UtilizationPct <= 0 || r.UtilizationPct > 100 {
		errs = append(errs, fmt.Errorf(
			"risk.utilizationPct (%.1f) must be within (0, 100]", r.UtilizationPct))
	}
	if r.MaxFillsPerHour <= 0 {
		errs = append(errs, errors.New("risk.maxFillsPerHour must be positive"))
	}
	if r.KillSwitchCooldown < 0 {
		errs = append(errs, fmt.Errorf(
			"risk.killSwitchCooldown (%s) must not be negative", r.KillSwitchCooldown))
	}
	// Нижняя граница не косметическая. Снятие переносит начало отсчёта убытка,
	// то есть каждое окно заново разрешает потерять maxDailyLossUsd. Cooldown
	// короче минуты означал бы, что предел суток измеряется тактом цикла.
	if r.KillSwitchCooldown > 0 && r.KillSwitchCooldown < time.Minute {
		errs = append(errs, fmt.Errorf(
			"risk.killSwitchCooldown (%s) must be at least a minute", r.KillSwitchCooldown))
	}
	if r.KillSwitchCooldown > 0 && r.KillSwitchMaxAutoResumes <= 0 {
		errs = append(errs, errors.New(
			"risk.killSwitchMaxAutoResumes must be positive when killSwitchCooldown is set: "+
				"a cooldown that is never granted only hides the setting"))
	}
	if r.MarginMode != trading.Cross && r.MarginMode != trading.Isolated {
		errs = append(errs, fmt.Errorf("risk.marginMode: want cross or isolated, got %q", r.MarginMode))
	}
	switch r.FlattenPolicy {
	case FlattenImmediateMarket:
	case FlattenMakerThenMarket:
		if r.MakerUnwindTimeout <= 0 {
			errs = append(errs, errors.New(
				"risk.makerUnwindTimeout must be positive with flattenPolicy maker_then_market"))
		}
	default:
		errs = append(errs, fmt.Errorf(
			"risk.flattenPolicy: want %q or %q, got %q",
			FlattenImmediateMarket, FlattenMakerThenMarket, r.FlattenPolicy))
	}
	if r.MakerUnwindTimeout < 0 || r.MakerUnwindBps < 0 {
		errs = append(errs, errors.New("risk: makerUnwindTimeout and makerUnwindBps must not be negative"))
	}
	if r.FlattenDebounce < 0 {
		errs = append(errs, errors.New("risk.flattenDebounce must not be negative"))
	}
	if r.PostFillCooldown < 0 {
		errs = append(errs, errors.New("risk.postFillCooldown must not be negative"))
	}
	if r.FillBurstLimit < 2 {
		errs = append(errs, errors.New("risk.fillBurstLimit must be at least 2"))
	}
	if r.FillBurstWindow <= 0 {
		errs = append(errs, errors.New("risk.fillBurstWindow must be positive"))
	}
	if r.FillBurstCooldown <= 0 {
		errs = append(errs, errors.New("risk.fillBurstCooldown must be positive"))
	}
	// Комиссии входят в независимую оценку стоимости залива. Ноль отключил бы
	// её молча, и защита осталась бы зависеть от одного непроверенного канала.
	if r.Fees.MakerBps < 0 || r.Fees.TakerBps <= 0 {
		errs = append(errs, fmt.Errorf(
			"risk.fees: makerBps must not be negative and takerBps must be positive, got %s and %s",
			r.Fees.MakerBps, r.Fees.TakerBps))
	}
	return errs
}

func (h Health) validate(band money.Bps) []error {
	var errs []error
	for name, d := range map[string]int64{
		"health.markStale":         int64(h.MarkStale),
		"health.reconcileInterval": int64(h.ReconcileInterval),
		"health.ackTimeout":        int64(h.AckTimeout),
		"health.heartbeatInterval": int64(h.HeartbeatInterval),
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	// Ноль разрешён: он возвращает безусловный съём по устаревшему mark.
	// Отрицательное значение — опечатка, которая молча делала бы то же самое.
	if h.MarkStaleHoldBps < 0 {
		errs = append(errs, fmt.Errorf("health.markStaleHoldBps must not be negative"))
	}
	// Порог за краем полосы награды не удерживал бы ничего полезного: ступень,
	// от которой ориентир ушёл на всю полосу, уже вне зачёта, а риск исполнения
	// несёт прежний.
	if band > 0 && h.MarkStaleHoldBps > band {
		errs = append(errs, fmt.Errorf(
			"health.markStaleHoldBps (%s) must not exceed the reward band (%s)",
			h.MarkStaleHoldBps, band))
	}
	return errs
}

func (s Symbol) validate(name string, band money.Bps, tiers []UptimeTier, live bool) []error {
	var errs []error
	prefix := "symbols." + name

	errs = append(errs, s.Economics.validate(prefix+".economics")...)
	errs = append(errs, s.Quote.validate(prefix+".quote")...)
	errs = append(errs, s.Appetite.validate(prefix+".appetite", band)...)
	errs = append(errs, validateLadderFits(prefix, s.Quote, s.Appetite)...)
	// Checked for disabled instruments too: a typo in the shutdown window
	// would otherwise surface only when the instrument is switched on, which
	// happens in production.
	errs = append(errs, validateOffHours(prefix+".program", s.Program)...)
	errs = append(errs, s.Uptime.validate(prefix+".uptime")...)
	if len(tiers) > 0 && s.Uptime.HardFloorPct < tiers[0].ThresholdPct {
		errs = append(errs, fmt.Errorf(
			"%s.uptime.hardFloorPct (%.1f) must not be below the best tier threshold (%.1f)",
			prefix, s.Uptime.HardFloorPct, tiers[0].ThresholdPct))
	}
	errs = append(errs, s.Volatility.validate(prefix+".volatility")...)
	errs = append(errs, validateLiquidity(s.Liquidity, prefix)...)
	// Checked for disabled instruments too, for the same reason as the shutdown
	// window: a typo would otherwise surface when the instrument is switched on.
	errs = append(errs, s.Reference.validate(prefix+".reference")...)
	if s.Enabled {
		errs = append(errs, s.Program.validate(prefix+".program")...)
	}

	if s.SymbolRisk.Leverage < 1 {
		errs = append(errs, fmt.Errorf("%s.risk.leverage must be at least 1", prefix))
	}
	// Ноль здесь — не «выключено», а деление на ноль в расчёте размера.
	if s.SymbolRisk.InitialMarginPct <= 0 || s.SymbolRisk.InitialMarginPct > 100 {
		errs = append(errs, fmt.Errorf(
			"%s.risk.initialMarginPct (%.4f) must be within (0, 100]: it is the share of order "+
				"notional the exchange locks, measured by test/live/margin_test.go",
			prefix, s.SymbolRisk.InitialMarginPct))
	}
	// Экономические ворота намеренно не проверяются здесь.
	//
	// Это политика, а не корректность конфигурации, и разница видна по тому,
	// чем оборачивается ошибка. Валидация роняет процесс на старте — а деплой
	// на такое падение откатывается на предыдущий образ и продолжает работать.
	// 10 августа так и вышло: три подряд выкатки упали на «allocation 400.00
	// USD exceeds the 60.00 USD calibration cap», каждая молча вернула образ
	// двухдневной давности, и прод двое суток крутил старый код при зелёном,
	// как казалось, контуре. Отказ, который выглядит как работающая система, —
	// худший из возможных.
	//
	// Ворота остаются там, где им место: в Engine.evaluate, где они запрещают
	// котировать с причиной в сводке и в mm_withdrawals_total{cause="economics"},
	// а после пяти минут молчания поднимаются в ERROR. Процесс при этом жив,
	// метрики отвечают, и видно, что именно он отказывается делать.
	return errs
}

func (e Economics) validate(prefix string) []error {
	var errs []error
	if e.RewardRateLowerBoundUSDPerMakerHour < 0 {
		errs = append(errs, fmt.Errorf("%s.rewardRateLowerBoundUsdPerMakerHour must not be negative", prefix))
	}
	if e.BreakEvenUSDPerMakerHour <= 0 {
		errs = append(errs, fmt.Errorf("%s.breakEvenUsdPerMakerHour must be positive", prefix))
	}
	return errs
}

func (q Quote) validate(prefix string) []error {
	var errs []error

	if q.LadderLayers < 1 {
		errs = append(errs, fmt.Errorf("%s.ladderLayers must be at least 1", prefix))
	}
	if q.LadderLayers > 1 && q.LadderSpreadBps <= 0 {
		errs = append(errs, fmt.Errorf("%s.ladderSpreadBps must be positive when ladderLayers > 1", prefix))
	}
	if q.MinRest < 0 || q.RequoteCooldown < 0 {
		errs = append(errs, fmt.Errorf("%s: minRest and requoteCooldown must not be negative", prefix))
	}
	if q.RefillLockout < 0 {
		errs = append(errs, fmt.Errorf("%s.refillLockout must not be negative", prefix))
	}
	if q.BalanceRejectBackoff < 0 {
		errs = append(errs, fmt.Errorf("%s.balanceRejectBackoff must not be negative", prefix))
	}
	if q.PostOnlyBufferTicks < 0 {
		errs = append(errs, fmt.Errorf("%s.postOnlyBufferTicks must not be negative", prefix))
	}
	return errs
}

// validateLiquidity сверяет ворота по глубине стакана.
//
// Ноль в ThinFraction — законное значение: оно выключает проверку. Единица
// и больше означали бы «тонко всегда», то есть молчание вместо котирования,
// и это почти наверняка опечатка.
func validateLiquidity(l Liquidity, prefix string) []error {
	var errs []error
	if l.ThinFraction < 0 || l.ThinFraction >= 1 {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity.thinFraction must be within [0, 1), got %.2f", prefix, l.ThinFraction))
	}
	if l.ThinFraction == 0 {
		return errs
	}
	if l.SevereFraction < 0 || l.SevereFraction >= l.ThinFraction {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity.severeFraction must be within [0, thinFraction=%.2f), got %.2f",
			prefix, l.ThinFraction, l.SevereFraction))
	}
	if l.RecoverFraction != 0 && (l.RecoverFraction <= l.ThinFraction || l.RecoverFraction >= 1) {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity.recoverFraction must be within (thinFraction=%.2f, 1) or 0, got %.2f",
			prefix, l.ThinFraction, l.RecoverFraction))
	}
	if l.ThinAfter < 0 || l.RecoverAfter < 0 {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity: thinAfter and recoverAfter must not be negative", prefix))
	}
	if l.Window <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity.window must be positive when thinFraction is set", prefix))
	}
	if l.SampleEvery < 0 {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity.sampleEvery must not be negative", prefix))
	}
	if l.SampleEvery > 0 && l.Window > 0 &&
		time.Duration(l.MinSamples)*l.SampleEvery > l.Window {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity: minSamples x sampleEvery (%s) exceeds the window (%s), the estimate can never mature",
			prefix, time.Duration(l.MinSamples)*l.SampleEvery, l.Window))
	}
	if l.MinSamples < 1 {
		errs = append(errs, fmt.Errorf(
			"%s.liquidity.minSamples must be at least 1 when thinFraction is set", prefix))
	}
	return errs
}

// validateLadderFits сверяет геометрию лесенки с мёртвой зоной и полосой награды.
//
// Обе проверки ловят настройку, при которой бот исправно работает и при этом
// гарантированно не делает того, ради чего написан.
//
// Мёртвая зона строится вокруг центра лесенки. Если она уже половины ширины
// лесенки, крайние ступени оказываются снаружи сразу после размещения и просят
// перестановки бесконечно — бюджет запросов уходит на дрожание.
//
// Полоса награды ограничивает дальнюю ступень: до неё от ближней ровно ширина
// лесенки, и если эта сумма не помещается между minBps и краем полосы, актор
// не сможет выставить ни одной допустимой конфигурации — он просто замолчит
// (policy.Ladder.Anchor), и понять почему по логу будет труднее, чем прочитать
// отказ при запуске.
func validateLadderFits(prefix string, q Quote, a Appetite) []error {
	ladder := policy.Ladder{Layers: q.LadderLayers, SpreadBps: q.LadderSpreadBps}
	half := ladder.HalfWidth()
	if half <= 0 {
		return nil
	}

	var errs []error
	if a.MinDeadBandBps < half {
		errs = append(errs, fmt.Errorf(
			"%s: the ladder half-width (%s) exceeds appetite.minDeadBandBps (%s): "+
				"the outer rungs would land outside the dead band and be requoted forever",
			prefix, half, a.MinDeadBandBps))
	}
	if width := 2 * half; a.MinBps+width > a.MaxBps {
		errs = append(errs, fmt.Errorf(
			"%s: the ladder is %s wide and does not fit between appetite.minBps (%s) and maxBps (%s)",
			prefix, width, a.MinBps, a.MaxBps))
	}
	return errs
}

// validate проверяет правило вычисления дистанции.
//
// Границы обязаны оставлять правилу место для манёвра и не выходить за полосу
// награды: за её пределами вес обнуляется, а не убывает.
func (a Appetite) validate(prefix string, band money.Bps) []error {
	var errs []error

	if a.JumpMultiple <= 0 {
		errs = append(errs, fmt.Errorf("%s.jumpMultiple must be positive", prefix))
	}
	if a.FloorMarginBps < 0 {
		errs = append(errs, fmt.Errorf("%s.floorMarginBps must not be negative", prefix))
	}
	if a.DeadBandJumps <= 0 {
		errs = append(errs, fmt.Errorf("%s.deadBandJumps must be positive", prefix))
	}
	if a.MinDeadBandBps <= 0 {
		errs = append(errs, fmt.Errorf("%s.minDeadBandBps must be positive", prefix))
	}
	if a.ReentryDeadBandBps < 0 {
		errs = append(errs, fmt.Errorf("%s.reentryDeadBandBps must not be negative", prefix))
	}
	if a.TargetRelaxAfter < 0 {
		errs = append(errs, fmt.Errorf("%s.targetRelaxAfter must not be negative", prefix))
	}
	// Порог опасности обязан оставаться ниже мёртвой зоны, иначе аварийный съём
	// накрывает её целиком: сторона снимается на обычном шуме, и присутствие
	// обнуляется вместо того, чтобы защищаться. Восстановление порядка в
	// policy.Thresholds спасёт от бессмыслицы, но молча — а параметр, который
	// молча не действует, хуже отсутствующего.
	if a.PanicFraction < 0 || a.PanicFraction >= 0.9 {
		errs = append(errs, fmt.Errorf(
			"%s.panicFraction must be in [0, 0.9), got %.2f", prefix, a.PanicFraction))
	}
	// Сужение не вправе съесть полосу целиком: иначе после первого же отказа
	// вернуться будет некуда, и гистерезис превратится в остановку навсегда.
	if a.MaxBps-a.ReentryDeadBandBps <= a.MinBps {
		errs = append(errs, fmt.Errorf(
			"%s: reentryDeadBandBps (%s) leaves no room between minBps (%s) and maxBps (%s)",
			prefix, a.ReentryDeadBandBps, a.MinBps, a.MaxBps))
	}
	if a.MinBps < 0 {
		errs = append(errs, fmt.Errorf("%s.minBps must not be negative", prefix))
	}
	if a.MaxBps <= a.MinBps {
		errs = append(errs, fmt.Errorf("%s: require minBps < maxBps, got %s >= %s", prefix, a.MinBps, a.MaxBps))
	}
	if a.MaxBps > band {
		errs = append(errs, fmt.Errorf(
			"%s.maxBps (%s) falls outside the reward band reward.bandBps (%s): quotes would stop counting",
			prefix, a.MaxBps, band))
	}
	return errs
}

// validate проверяет параметры начисления инструмента.
func (p Program) validate(prefix string) []error {
	var errs []error

	for name, raw := range map[string]string{"unitSize": p.UnitSize, "perSideCap": p.PerSideCap} {
		value, err := decimal.NewFromString(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.%s: %q is not a number", prefix, name, raw))
			continue
		}
		if !value.IsPositive() {
			errs = append(errs, fmt.Errorf("%s.%s must be positive for an enabled symbol, got %s", prefix, name, raw))
		}
	}
	if p.MaxMakerHoursPerHour <= 0 {
		errs = append(errs, fmt.Errorf("%s.maxMakerHoursPerHour must be positive for an enabled symbol", prefix))
	}
	return errs
}

// validateOffHours checks the shutdown schedule of the reference market.
//
// A multiplier without a window and a window without a multiplier are equally
// useless and almost always mean a half-written setting, so both are required
// together. A mistake here costs a weekend: the bot would treat the hour as a
// full one while earning a tenth of it at unchanged fill risk.
func validateOffHours(prefix string, p Program) []error {
	var errs []error

	schedule, err := p.Schedule()
	if err != nil {
		errs = append(errs, fmt.Errorf("%s.offHours: %w", prefix, err))
		return errs
	}

	if p.OffHoursMultiplier < 0 || p.OffHoursMultiplier > 1 {
		errs = append(errs, fmt.Errorf(
			"%s.offHoursMultiplier (%.2f) must be within [0, 1]; one means a 24/7 market",
			prefix, p.OffHoursMultiplier))
	}

	reduced := p.OffHoursMultiplier >= 0 && p.OffHoursMultiplier < 1
	scheduled := !schedule.Weekly.IsZero() || len(schedule.Holidays) > 0

	switch {
	case reduced && !scheduled:
		errs = append(errs, fmt.Errorf(
			"%s: offHoursMultiplier is %.2f but no offHours window is set, so it would never apply",
			prefix, p.OffHoursMultiplier))
	case scheduled && !reduced:
		errs = append(errs, fmt.Errorf(
			"%s: an offHours window is set but offHoursMultiplier is %.2f, so the window has no effect",
			prefix, p.OffHoursMultiplier))
	}
	return errs
}

// Policy converts the configured windows into the domain form.
func (n News) Policy() calendar.Policy {
	return calendar.Policy{
		DefensiveBefore: n.DefensiveBefore,
		DefensiveAfter:  n.DefensiveAfter,
		HardBefore:      n.HardBefore,
		HardAfter:       n.HardAfter,
	}
}

// Filter converts the configured selection into the domain form.
func (n News) Filter() calendar.Filter {
	impacts := make([]calendar.Impact, 0, len(n.Impacts))
	for _, raw := range n.Impacts {
		impacts = append(impacts, calendar.ParseImpact(raw))
	}
	return calendar.Filter{Currencies: n.Currencies, Impacts: impacts}
}

// validate checks the news blackout.
//
// An enabled guard with an empty selection is the failure worth catching here:
// it starts, fetches, logs a healthy calendar and protects nothing, and the
// only way to notice is to still be standing in the book when the number
// prints.
func (n News) validate() []error {
	if !n.Enabled {
		return nil
	}

	var errs []error
	if err := n.Policy().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("news: %w", err))
	}
	if len(n.Currencies) == 0 {
		errs = append(errs, errors.New(
			"news.currencies is empty, so the guard would match no release at all"))
	}
	if len(n.Impacts) == 0 {
		errs = append(errs, errors.New(
			"news.impacts is empty, so the guard would match no release at all"))
	}
	for _, raw := range n.Impacts {
		switch calendar.ParseImpact(raw) {
		case calendar.ImpactHigh, calendar.ImpactMedium, calendar.ImpactLow, calendar.ImpactHoliday:
		default:
			errs = append(errs, fmt.Errorf(
				"news.impacts: unknown value %q, known: high, medium, low, holiday", raw))
		}
	}
	if n.Refresh <= 0 {
		errs = append(errs, errors.New("news.refresh must be positive"))
	}
	// Расписание, устаревающее раньше, чем его успевают перечитать, в режиме
	// fail-closed останавливало бы котирование между двумя удачными чтениями.
	if n.MaxAge <= n.Refresh {
		errs = append(errs, fmt.Errorf(
			"news.maxAge (%s) must exceed news.refresh (%s), or the calendar is stale between reads",
			n.MaxAge, n.Refresh))
	}
	if n.Timeout <= 0 {
		errs = append(errs, errors.New("news.timeout must be positive"))
	}
	if n.DefensiveInsetBps < 0 {
		errs = append(errs, errors.New("news.defensiveInsetBps must not be negative"))
	}
	// Множитель больше единицы означал бы наращивание размера перед публикацией,
	// то есть прямо противоположное назначению позы.
	if n.DefensiveSizeMultiple <= 0 || n.DefensiveSizeMultiple > 1 {
		errs = append(errs, fmt.Errorf(
			"news.defensiveSizeMultiple (%.2f) must be within (0, 1]", n.DefensiveSizeMultiple))
	}
	if n.URL != "" {
		if u, err := url.Parse(n.URL); err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("news.url: malformed URL %q", n.URL))
		}
	}
	return errs
}

// validate checks the external reference feed.
//
// A disabled feed is left alone: naming a venue for an instrument that is not
// watched costs nothing. An enabled one must be complete, because the failure
// it would otherwise produce is silent — a feed that never connects looks
// exactly like a feed on a quiet market.
func (r Reference) validate(prefix string) []error {
	if !r.Enabled {
		return nil
	}

	var errs []error
	if !slices.Contains(ReferenceProviders(), r.Provider) {
		errs = append(errs, fmt.Errorf("%s.provider: unsupported value %q, known: %s",
			prefix, r.Provider, strings.Join(ReferenceProviders(), ", ")))
	}
	if strings.TrimSpace(r.Symbol) == "" {
		errs = append(errs, fmt.Errorf(
			"%s.symbol is required for an enabled feed: it names the instrument on the venue, for example BTCUSDT",
			prefix))
	}
	// A threshold at or past the mark tick makes the feed pointless without
	// saying so: the reference would be trusted right up to the moment the
	// official price arrives and supersedes it anyway.
	if r.StaleAfter <= 0 || r.StaleAfter > markTick {
		errs = append(errs, fmt.Errorf(
			"%s.staleAfter (%s) must be within (0, %s]: the feed exists to see inside the mark tick",
			prefix, r.StaleAfter, markTick))
	}
	return errs
}

// markTick — измеренный шаг канала price, см. PLAN.md §1.4.
const markTick = 3 * time.Second

func (u Uptime) validate(prefix string) []error {
	var errs []error
	if u.HardFloorPct <= 0 || u.HardFloorPct > 100 {
		errs = append(errs, fmt.Errorf("%s.hardFloorPct (%.1f) must be within (0, 100]", prefix, u.HardFloorPct))
	}
	if u.TargetPct < u.HardFloorPct || u.TargetPct > 100 {
		errs = append(errs, fmt.Errorf("%s.targetPct (%.1f) must be within [hardFloorPct, 100]", prefix, u.TargetPct))
	}
	if u.ReservePct < 0 || u.ReservePct >= 100 {
		errs = append(errs, fmt.Errorf("%s.reservePct (%.1f) must be within [0, 100)", prefix, u.ReservePct))
	}
	return errs
}

func (v Volatility) validate(prefix string) []error {
	var errs []error

	if v.Window <= 0 {
		errs = append(errs, fmt.Errorf("%s.window must be positive", prefix))
	}
	if v.MinSamples < 2 {
		errs = append(errs, fmt.Errorf("%s.minSamples must be at least 2", prefix))
	}
	// Зрелость не может наступать раньше готовности: иначе поправка
	// на незрелость никогда не применяется.
	if v.FullConfidenceAt < v.MinSamples {
		errs = append(errs, fmt.Errorf("%s: require minSamples <= fullConfidenceAt, got %d > %d",
			prefix, v.MinSamples, v.FullConfidenceAt))
	}
	// Обязателен, в отличие от memoryHalfLife: ноль здесь не отключает защиту,
	// а тихо возвращает старую уязвимость целиком — см. поле в config.go.
	if v.WindowHalfLife <= 0 {
		errs = append(errs, fmt.Errorf("%s.windowHalfLife must be positive", prefix))
	}
	// Не короче — это уже проверено выше. Не длиннее или равно Window — иначе
	// затухание не успевает случиться быстрее, чем событие и так выпало бы
	// из сырого окна, и вся правка от 19 августа не делает ничего.
	if v.WindowHalfLife > 0 && v.WindowHalfLife >= v.Window {
		errs = append(errs, fmt.Errorf(
			"%s: windowHalfLife (%s) must be shorter than window (%s), "+
				"or the fast record never releases faster than the raw window would have",
			prefix, v.WindowHalfLife, v.Window))
	}
	if v.MemoryHalfLife < 0 {
		errs = append(errs, fmt.Errorf("%s.memoryHalfLife must not be negative", prefix))
	}
	// Память короче окна бессмысленна: она затухла бы раньше, чем окно успело
	// забыть тот же скачок, и нижней границей никогда бы не стала.
	if v.MemoryHalfLife > 0 && v.MemoryHalfLife < v.Window {
		errs = append(errs, fmt.Errorf(
			"%s: memoryHalfLife (%s) shorter than window (%s) can never bind the estimate",
			prefix, v.MemoryHalfLife, v.Window))
	}
	// Below one the bound would put the record under the very window maximum it
	// is meant to outlive, which is the same as switching the memory off — but
	// silently, and only in the calm periods where it is needed most.
	if v.MemoryWindowMultiple != 0 && v.MemoryWindowMultiple < 1 {
		errs = append(errs, fmt.Errorf(
			"%s.memoryWindowMultiple (%.2f) must be at least 1, or 0 to disable the bound",
			prefix, v.MemoryWindowMultiple))
	}
	return errs
}
