// Package metrics публикует показатели работы бота в формате Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/ports"
)

// namespace — общий префикс всех показателей.
const namespace = "mm"

// Prometheus — реализация ports.Metrics поверх client_golang.
//
// Все коллекторы создаются в конструкторе, а не по требованию: набор
// показателей фиксирован, а ленивое создание означало бы, что метрика
// появляется в выдаче только после первого события. Дашборд с пропадающими
// панелями хуже, чем дашборд с нулями: ноль — это утверждение, отсутствие —
// неоднозначность.
type Prometheus struct {
	registry *prometheus.Registry

	uptime     *prometheus.GaugeVec
	makerHours *prometheus.GaugeVec
	distance   *prometheus.HistogramVec
	liveQuotes *prometheus.GaugeVec
	quoting    *prometheus.GaugeVec

	target     *prometheus.GaugeVec
	halfSpread *prometheus.GaugeVec
	bookDepth  *prometheus.GaugeVec
	jump       *prometheus.GaugeVec
	staleness  *prometheus.GaugeVec

	fills         *prometheus.CounterVec
	shadowFills   *prometheus.CounterVec
	flattenCost   *prometheus.CounterVec
	realizedPnL   prometheus.Gauge
	estimatedCost prometheus.Gauge
	fillsPerHour  prometheus.Gauge
	killSwitch    prometheus.Gauge

	ordersSent   *prometheus.CounterVec
	ordersFailed *prometheus.CounterVec
	aloRejects   *prometheus.CounterVec
	rateTokens   prometheus.Gauge
	reconnects   *prometheus.CounterVec
	withdrawals  *prometheus.CounterVec
}

// New собирает и регистрирует коллекторы.
func New() *Prometheus {
	p := &Prometheus{registry: prometheus.NewRegistry()}

	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(
			prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	}
	plain := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(
			prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
	}

	p.uptime = gauge("quote_uptime_ratio",
		"Share of the current hour with qualifying quotes on both sides", "symbol")
	p.makerHours = gauge("estimated_maker_hours",
		"Our own estimate of Maker Hours accrued in the current hour", "symbol")
	// Полоса награды заканчивается на 10 bps, поэтому шаг в половину базисного
	// пункта разрешает всю интересную часть распределения, а последний бакет
	// собирает ошибки расчёта дистанции.
	p.distance = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "order_distance_bps",
		Help:      "Distance from mark price at which quotes were placed",
		Buckets:   prometheus.LinearBuckets(0, 0.5, 24),
	}, []string{"symbol", "side"})
	p.liveQuotes = gauge("live_quotes", "Ladder rungs currently resting", "symbol", "side")
	p.quoting = gauge("quoting", "Whether the actor is quoting right now", "symbol")

	p.target = gauge("target_distance_bps", "Computed target distance", "symbol")
	p.halfSpread = gauge("half_spread_bps", "Half of the order book spread", "symbol")
	p.bookDepth = gauge("book_depth_usd",
		"Notional resting within the reward band, by side", "symbol", "side")
	p.jump = gauge("jump_bps", "Risk measure: mark price move per tick, adjusted", "symbol")
	p.staleness = gauge("mark_staleness_seconds", "Age of the last mark price", "symbol")

	p.fills = counter("fills_total", "Quotes hit by the market", "symbol", "side")
	p.shadowFills = counter("shadow_fills_total",
		"Dry run: times the market reached a quote, an upper bound on fills", "symbol", "side")
	p.flattenCost = counter("flatten_cost_usd_total",
		"Estimated cost of closing positions after fills", "symbol")
	p.realizedPnL = plain("realized_pnl_usd",
		"Realized result for the current UTC day as reported by the exchange")
	p.estimatedCost = plain("estimated_cost_usd",
		"Independent estimate of fill cost for the current UTC day")
	p.fillsPerHour = plain("fills_last_hour", "Fills within the trailing hour")
	p.killSwitch = plain("kill_switch", "1 when trading is halted by the risk guard")

	p.ordersSent = counter("orders_sent_total", "Trading commands sent", "symbol", "side", "kind")
	p.ordersFailed = counter("orders_failed_total",
		"Trading commands that never left the process", "symbol", "kind")
	p.aloRejects = counter("alo_rejects_total",
		"Post-only rejections: the distance was too aggressive", "symbol")
	p.rateTokens = plain("rate_limit_tokens", "Request credits available in the shaper")
	p.reconnects = counter("stream_reconnects_total", "Stream reconnections", "stream")
	p.withdrawals = counter("withdrawals_total", "Emergency quote withdrawals", "symbol", "cause")

	p.registry.MustRegister(
		p.uptime, p.makerHours, p.distance, p.liveQuotes, p.quoting,
		p.target, p.halfSpread, p.bookDepth, p.jump, p.staleness,
		p.fills, p.shadowFills, p.flattenCost, p.realizedPnL, p.estimatedCost, p.fillsPerHour, p.killSwitch,
		p.ordersSent, p.ordersFailed, p.aloRejects, p.rateTokens, p.reconnects, p.withdrawals,
	)
	return p
}

// Registry открывает доступ к реестру для HTTP-обработчика.
func (p *Prometheus) Registry() *prometheus.Registry { return p.registry }

func (p *Prometheus) QuoteUptime(s market.Symbol, ratio float64) {
	p.uptime.WithLabelValues(s.String()).Set(ratio)
}

func (p *Prometheus) EstimatedMakerHours(s market.Symbol, mh float64) {
	p.makerHours.WithLabelValues(s.String()).Set(mh)
}

func (p *Prometheus) QuoteDistance(s market.Symbol, side market.Side, bps float64) {
	p.distance.WithLabelValues(s.String(), string(side)).Observe(bps)
}

func (p *Prometheus) LiveQuotes(s market.Symbol, side market.Side, n int) {
	p.liveQuotes.WithLabelValues(s.String(), string(side)).Set(float64(n))
}

func (p *Prometheus) Quoting(s market.Symbol, yes bool) {
	p.quoting.WithLabelValues(s.String()).Set(boolValue(yes))
}

func (p *Prometheus) TargetDistance(s market.Symbol, bps float64) {
	p.target.WithLabelValues(s.String()).Set(bps)
}

func (p *Prometheus) HalfSpread(s market.Symbol, bps float64) {
	p.halfSpread.WithLabelValues(s.String()).Set(bps)
}

func (p *Prometheus) BookDepth(s market.Symbol, side market.Side, usd float64) {
	p.bookDepth.WithLabelValues(s.String(), string(side)).Set(usd)
}

func (p *Prometheus) Jump(s market.Symbol, bps float64) {
	p.jump.WithLabelValues(s.String()).Set(bps)
}

func (p *Prometheus) MarkStaleness(s market.Symbol, seconds float64) {
	p.staleness.WithLabelValues(s.String()).Set(seconds)
}

func (p *Prometheus) Fill(s market.Symbol, side market.Side) {
	p.fills.WithLabelValues(s.String(), string(side)).Inc()
}

func (p *Prometheus) ShadowFill(s market.Symbol, side market.Side) {
	p.shadowFills.WithLabelValues(s.String(), string(side)).Inc()
}

// FlattenCost принимает только неотрицательные значения: счётчик Prometheus
// монотонен, и попытка уменьшить его паникует. Отрицательная стоимость
// закрытия означала бы ошибку в расчёте, и ронять из-за неё торгующий процесс
// несоразмерно.
func (p *Prometheus) FlattenCost(s market.Symbol, usd float64) {
	if usd <= 0 {
		return
	}
	p.flattenCost.WithLabelValues(s.String()).Add(usd)
}

func (p *Prometheus) RealizedPnL(usd float64)   { p.realizedPnL.Set(usd) }
func (p *Prometheus) EstimatedCost(usd float64) { p.estimatedCost.Set(usd) }
func (p *Prometheus) FillsLastHour(n int)       { p.fillsPerHour.Set(float64(n)) }
func (p *Prometheus) KillSwitch(tripped bool)   { p.killSwitch.Set(boolValue(tripped)) }

func (p *Prometheus) OrderSent(s market.Symbol, side market.Side, kind string) {
	p.ordersSent.WithLabelValues(s.String(), string(side), kind).Inc()
}

func (p *Prometheus) OrderFailed(s market.Symbol, kind string) {
	p.ordersFailed.WithLabelValues(s.String(), kind).Inc()
}

func (p *Prometheus) ALOReject(s market.Symbol) {
	p.aloRejects.WithLabelValues(s.String()).Inc()
}

func (p *Prometheus) RateLimitTokens(v float64) { p.rateTokens.Set(v) }

func (p *Prometheus) StreamReconnect(stream string) {
	p.reconnects.WithLabelValues(stream).Inc()
}

func (p *Prometheus) Withdrawal(s market.Symbol, cause ports.WithdrawCause) {
	p.withdrawals.WithLabelValues(s.String(), string(cause)).Inc()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

var _ ports.Metrics = (*Prometheus)(nil)

// Snapshot собирает текущие значения всех зарегистрированных показателей.
//
// Гистограммы раскладываются на счётчики суммы и числа наблюдений: по их
// разностям за час восстанавливается среднее, а полное распределение
// в почасовой свёртке избыточно — для него есть сам эндпойнт.
func (p *Prometheus) Snapshot() []ports.MetricSample {
	families, err := p.registry.Gather()
	if err != nil {
		return nil
	}

	var out []ports.MetricSample
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			add := func(name string, kind ports.MetricKind, v float64) {
				out = append(out, ports.MetricSample{
					Name: name, Labels: labels, Kind: kind, Value: v,
				})
			}

			switch {
			case m.GetGauge() != nil:
				add(f.GetName(), ports.KindGauge, m.GetGauge().GetValue())
			case m.GetCounter() != nil:
				add(f.GetName(), ports.KindCounter, m.GetCounter().GetValue())
			case m.GetHistogram() != nil:
				add(f.GetName()+"_sum", ports.KindCounter, m.GetHistogram().GetSampleSum())
				add(f.GetName()+"_count", ports.KindCounter, float64(m.GetHistogram().GetSampleCount()))
			}
		}
	}
	return out
}

var _ ports.MetricsSource = (*Prometheus)(nil)
