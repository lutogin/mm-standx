package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nd/mm-standx/internal/domain/money"
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
	errs = append(errs, c.Health.validate()...)

	enabled := 0
	for name, sym := range c.Symbols {
		if sym.Enabled {
			enabled++
		}
		errs = append(errs, sym.validate(name, c.Reward.BandBps)...)
	}
	if enabled == 0 && c.IsLive() {
		errs = append(errs, errors.New("runtime.mode=live but no symbol is enabled"))
	}

	return errors.Join(errs...)
}

func (r Runtime) validate() []error {
	var errs []error
	if r.Mode != ModeDryRun && r.Mode != ModeLive {
		errs = append(errs, fmt.Errorf("runtime.mode: want %q or %q, got %q", ModeDryRun, ModeLive, r.Mode))
	}
	switch strings.ToLower(r.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("runtime.logLevel: unsupported value %q", r.LogLevel))
	}
	if r.LogFormat != "json" && r.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("runtime.logFormat: want json or text, got %q", r.LogFormat))
	}
	if r.StatsInterval <= 0 {
		errs = append(errs, errors.New("runtime.statsInterval must be positive"))
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
	if r.MaxNotionalUSD <= 0 {
		errs = append(errs, errors.New("risk.maxNotionalUsd must be positive"))
	}
	if r.MaxFillsPerHour <= 0 {
		errs = append(errs, errors.New("risk.maxFillsPerHour must be positive"))
	}
	if r.MarginMode != trading.Cross && r.MarginMode != trading.Isolated {
		errs = append(errs, fmt.Errorf("risk.marginMode: want cross or isolated, got %q", r.MarginMode))
	}
	switch r.FlattenPolicy {
	case "immediate_market", "passive_then_market", "hybrid":
	default:
		errs = append(errs, fmt.Errorf("risk.flattenPolicy: unsupported value %q", r.FlattenPolicy))
	}
	if r.FlattenDebounce < 0 {
		errs = append(errs, errors.New("risk.flattenDebounce must not be negative"))
	}
	return errs
}

func (h Health) validate() []error {
	var errs []error
	for name, d := range map[string]int64{
		"health.markStale":         int64(h.MarkStale),
		"health.disconnectPull":    int64(h.DisconnectPull),
		"health.reconcileInterval": int64(h.ReconcileInterval),
		"health.ackTimeout":        int64(h.AckTimeout),
		"health.heartbeatInterval": int64(h.HeartbeatInterval),
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	return errs
}

func (s Symbol) validate(name string, band money.Bps) []error {
	var errs []error
	prefix := "symbols." + name

	if s.Enabled && s.AllocationUSD <= 0 {
		errs = append(errs, fmt.Errorf("%s.allocationUsd must be positive for an enabled symbol", prefix))
	}
	errs = append(errs, s.Quote.validate(prefix+".quote", band)...)
	errs = append(errs, s.Uptime.validate(prefix+".uptime")...)
	errs = append(errs, s.Volatility.validate(prefix+".volatility")...)

	if s.SymbolRisk.Leverage < 1 {
		errs = append(errs, fmt.Errorf("%s.risk.leverage must be at least 1", prefix))
	}
	return errs
}

func (q Quote) validate(prefix string, band money.Bps) []error {
	var errs []error

	// Пороги обязаны идти строго по возрастанию: на этом порядке держится
	// весь гистерезис из PLAN.md §4.1.
	ordered := []struct {
		name string
		val  money.Bps
	}{
		{"pullBps", q.PullBps},
		{"panicBps", q.PanicBps},
		{"minKeepBps", q.MinKeepBps},
		{"targetBps", q.TargetBps},
		{"maxKeepBps", q.MaxKeepBps},
	}
	if q.PullBps < 0 {
		errs = append(errs, fmt.Errorf("%s.pullBps must not be negative", prefix))
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i].val <= ordered[i-1].val {
			errs = append(errs, fmt.Errorf("%s: require %s < %s, got %s >= %s",
				prefix, ordered[i-1].name, ordered[i].name, ordered[i-1].val, ordered[i].val))
		}
	}
	if q.MaxKeepBps > band {
		errs = append(errs, fmt.Errorf("%s.maxKeepBps (%s) falls outside the reward band reward.bandBps (%s): orders would stop counting",
			prefix, q.MaxKeepBps, band))
	}

	if q.LadderLayers < 1 {
		errs = append(errs, fmt.Errorf("%s.ladderLayers must be at least 1", prefix))
	}
	if q.LadderLayers > 1 {
		if q.LadderSpreadBps <= 0 {
			errs = append(errs, fmt.Errorf("%s.ladderSpreadBps must be positive when ladderLayers > 1", prefix))
		}
		// Лесенка центрирована вокруг targetBps и обязана целиком помещаться
		// в удерживаемый диапазон, иначе крайние ступени будут перекотировываться вечно.
		half := money.Bps(float64(q.LadderLayers-1)) * q.LadderSpreadBps / 2
		if q.TargetBps-half < q.MinKeepBps || q.TargetBps+half > q.MaxKeepBps {
			errs = append(errs, fmt.Errorf(
				"%s: ladder of %d x %s around %s does not fit within [%s, %s]; reduce ladderSpreadBps or widen the range",
				prefix, q.LadderLayers, q.LadderSpreadBps, q.TargetBps, q.MinKeepBps, q.MaxKeepBps))
		}
	}
	if q.MinRest < 0 || q.RequoteCooldown < 0 {
		errs = append(errs, fmt.Errorf("%s: minRest and requoteCooldown must not be negative", prefix))
	}
	return errs
}

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
	if v.EWMAHalfLife <= 0 {
		errs = append(errs, fmt.Errorf("%s.ewmaHalfLife must be positive", prefix))
	}
	if v.WidenSigmaBps <= 0 {
		errs = append(errs, fmt.Errorf("%s.widenSigmaBps must be positive", prefix))
	}
	if v.PullSigmaBps <= v.WidenSigmaBps {
		errs = append(errs, fmt.Errorf("%s: require widenSigmaBps < pullSigmaBps, got %s >= %s",
			prefix, v.WidenSigmaBps, v.PullSigmaBps))
	}
	if v.JumpBps <= 0 {
		errs = append(errs, fmt.Errorf("%s.jumpBps must be positive", prefix))
	}
	if v.WidenFactorK < 0 {
		errs = append(errs, fmt.Errorf("%s.widenFactorK must not be negative", prefix))
	}
	return errs
}
