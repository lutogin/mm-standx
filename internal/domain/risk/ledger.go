// Package risk ведёт учёт того, во что обходится стратегия, и решает,
// когда торговлю следует прекратить.
//
// Пакет чистый: время приходит параметром, состояние сериализуемо целиком.
// Сериализуемость здесь не удобство, а требование: счётчик дневного убытка,
// который обнуляется при перезапуске, не защищает ни от чего. Цикл
// «сработала защита → процесс перезапустили → счётчик чист» — это способ
// потерять депозит за несколько итераций, а не предохранитель.
package risk

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/nd/mm-standx/internal/domain/market"
)

// Limits — пороги, при достижении которых торговля прекращается.
type Limits struct {
	// MaxDailyLossUSD — накопленный за сутки UTC убыток, после которого
	// котирование прекращается до вмешательства человека.
	MaxDailyLossUSD float64

	// MaxFillsPerHour — предел числа заливов в скользящем часе.
	//
	// Это не денежный лимит, а детектор рассогласования модели с рынком.
	// Стратегия построена на том, что исполнения редки; частые заливы
	// означают, что дистанция подобрана неверно, и продолжать — значит
	// платить за проверку гипотезы, которая уже опровергнута.
	MaxFillsPerHour int

	// TripOnDrawdown включает остановку по дневному убытку. Выключенный
	// лимит по-прежнему считается и публикуется в метриках, но не тормозит.
	TripOnDrawdown bool
}

// Fill — одно исполнение нашей котировки.
type Fill struct {
	Symbol market.Symbol
	At     time.Time
	// CostUSD — оценка стоимости этого исполнения по известным ставкам
	// комиссий и измеренному спреду.
	CostUSD decimal.Decimal
	// StartsEpisode отличает первое исполнение серии от её продолжения.
	//
	// Частота считается эпизодами, а не исполнениями: свип даёт десятки
	// частичных филлов подряд, и считать их поштучно значило бы измерять
	// размер чужого ордера, а не частоту собственных ошибок. Стоимость,
	// наоборот, суммируется по всем исполнениям — платим за каждое.
	StartsEpisode bool
}

// Ledger накапливает стоимость стратегии и выносит вердикт.
type Ledger struct {
	limits Limits
	state  State
}

// State — сериализуемое состояние учёта.
//
// Поля экспортированы и снабжены тегами json намеренно: снимок пишется на диск
// и должен читаться человеком без специальных средств. Разбирать состояние
// защитного контура в момент инцидента приходится под давлением, и формат,
// требующий инструмента, в этот момент бесполезен.
type State struct {
	// Day — сутки UTC, к которым относится накопленный результат.
	Day time.Time `json:"day"`

	// ReportedNetUSD — сумма (PnL − комиссия) по данным биржи.
	ReportedNetUSD decimal.Decimal `json:"reported_net_usd"`

	// EstimatedCostUSD — независимая оценка стоимости по числу эпизодов.
	//
	// Дублирование намеренно. Канал сделок биржи не проверен длительной
	// эксплуатацией, а часть документации StandX уже оказалась неверной
	// (PLAN.md §13). Защита, целиком зависящая от одного непроверенного
	// источника, отказывает молча: нулевой убыток выглядит одинаково и когда
	// стратегия прибыльна, и когда канал не доставляет сообщений.
	EstimatedCostUSD decimal.Decimal `json:"estimated_cost_usd"`

	// TradeCount — сколько сообщений о сделках учтено за сутки. Ноль при
	// ненулевом числе эпизодов означает неисправность канала.
	TradeCount int `json:"trade_count"`

	// FirstEpisodeAt — момент первого залива суток.
	//
	// Нужен, чтобы отличить неисправный канал сделок от обычного порядка
	// доставки. Сообщение об исполнении ордера и сообщение о сделке приходят
	// разными каналами, и первое штатно опережает второе. Без отсрочки
	// подозрение срабатывает на каждом первом заливе — то есть ровно тогда,
	// когда проверять ещё нечего.
	FirstEpisodeAt time.Time `json:"first_episode_at"`

	// Episodes — заливы за последний час, для скользящего счётчика.
	Episodes []Fill `json:"episodes"`

	// EpisodesToday — счётчик за сутки, только для наблюдаемости.
	EpisodesToday int `json:"episodes_today"`

	// Tripped и TrippedReason переживают перезапуск: снятие защиты —
	// осознанное действие человека, а не побочный эффект рестарта.
	Tripped       bool   `json:"tripped"`
	TrippedReason string `json:"tripped_reason,omitempty"`
	// TrippedAt — момент срабатывания, для разбора инцидента.
	TrippedAt time.Time `json:"tripped_at"`
}

// New создаёт учёт с заданными лимитами.
func New(limits Limits) *Ledger {
	return &Ledger{limits: limits}
}

// Restore возвращает учёт к сохранённому состоянию.
//
// Состояние чужих суток отбрасывается целиком, включая флаг остановки: лимит
// дневной, и новый день начинается с чистого листа по определению.
func (l *Ledger) Restore(s State, now time.Time) {
	if !s.Day.Equal(day(now)) {
		l.state = State{Day: day(now)}
		return
	}
	l.state = s
	l.evict(now)
}

// State возвращает снимок для сохранения.
func (l *Ledger) State() State { return l.state }

// OnTrade учитывает сообщение биржи об исполнении.
//
// Комиссия вычитается по модулю: у программы нет мейкерских выплат, комиссия
// всегда расход (PLAN.md §1.3), а знак в сообщении биржи не проверен.
// Ошибиться здесь в сторону оптимизма значит завысить остаток лимита.
func (l *Ledger) OnTrade(now time.Time, pnl, fee decimal.Decimal) {
	l.roll(now)
	l.state.ReportedNetUSD = l.state.ReportedNetUSD.Add(pnl).Sub(fee.Abs())
	l.state.TradeCount++
}

// OnFill учитывает исполнение нашей котировки.
func (l *Ledger) OnFill(now time.Time, f Fill) {
	l.roll(now)
	l.state.EstimatedCostUSD = l.state.EstimatedCostUSD.Add(f.CostUSD.Abs())
	if f.StartsEpisode {
		l.state.Episodes = append(l.state.Episodes, f)
		l.state.EpisodesToday++
		if l.state.FirstEpisodeAt.IsZero() {
			l.state.FirstEpisodeAt = f.At
		}
	}
	l.evict(now)
}

// Verdict — текущая оценка риска.
type Verdict struct {
	// Tripped сообщает, что торговля должна быть прекращена.
	Tripped bool
	// Reason описывает причину остановки.
	Reason string
	// LossUSD — принятая к учёту величина убытка (положительная).
	LossUSD float64
	// ReportedNetUSD — результат по данным биржи, знаковый.
	ReportedNetUSD float64
	// EstimatedCostUSD — независимая оценка стоимости заливов.
	EstimatedCostUSD float64
	// FillsLastHour — число заливов в скользящем часе.
	FillsLastHour int
	// EpisodesToday — число заливов за сутки.
	EpisodesToday int
	// TradeChannelSuspect сообщает, что заливы были, а сообщений о сделках нет.
	TradeChannelSuspect bool
}

// Assess выносит вердикт и, при достижении лимита, взводит остановку.
//
// Взведённая остановка не снимается сама: вернуть бота в работу может только
// человек, разобравшийся в причине. Автоматическое восстановление означало бы,
// что защита срабатывает ровно столько раз, сколько нужно, чтобы исчерпать
// депозит по частям.
func (l *Ledger) Assess(now time.Time) Verdict {
	l.roll(now)
	l.evict(now)

	v := Verdict{
		ReportedNetUSD:      f(l.state.ReportedNetUSD),
		EstimatedCostUSD:    f(l.state.EstimatedCostUSD),
		FillsLastHour:       len(l.state.Episodes),
		EpisodesToday:       l.state.EpisodesToday,
		TradeChannelSuspect: l.tradeChannelSuspect(now),
	}

	// Из двух оценок берётся худшая. Биржевая точнее, когда работает,
	// но её молчание не должно читаться как отсутствие убытка.
	reportedLoss := -f(l.state.ReportedNetUSD)
	v.LossUSD = max(reportedLoss, v.EstimatedCostUSD)

	switch {
	case l.state.Tripped:
		v.Tripped, v.Reason = true, l.state.TrippedReason

	case l.limits.TripOnDrawdown && l.limits.MaxDailyLossUSD > 0 &&
		v.LossUSD >= l.limits.MaxDailyLossUSD:
		l.trip(now, fmt.Sprintf(
			"daily loss %.2f USD reached the %.2f limit (exchange reported %.2f, cost model %.2f)",
			v.LossUSD, l.limits.MaxDailyLossUSD, v.ReportedNetUSD, v.EstimatedCostUSD))
		v.Tripped, v.Reason = true, l.state.TrippedReason

	case l.limits.MaxFillsPerHour > 0 && v.FillsLastHour >= l.limits.MaxFillsPerHour:
		l.trip(now, fmt.Sprintf(
			"%d fills in the last hour reached the %d limit: the distance model disagrees with the market",
			v.FillsLastHour, l.limits.MaxFillsPerHour))
		v.Tripped, v.Reason = true, l.state.TrippedReason
	}
	return v
}

// TradeChannelGrace — сколько ждать сообщения о сделке после первого залива.
//
// Порядок доставки между каналами не гарантирован, и сообщение об исполнении
// ордера штатно опережает сообщение о сделке на десятки миллисекунд. Отсрочка
// отделяет эту разницу от настоящей неисправности, оставаясь достаточно
// короткой, чтобы молчащий канал был замечен в том же эпизоде.
const TradeChannelGrace = 30 * time.Second

// tradeChannelSuspect сообщает, что заливы были, а сделок канал не принёс.
func (l *Ledger) tradeChannelSuspect(now time.Time) bool {
	if l.state.EpisodesToday == 0 || l.state.TradeCount > 0 {
		return false
	}
	// Состояние прошлого запуска могло не знать момента первого залива: поле
	// появилось позже. Подозревать канал не на чем — дождёмся нового эпизода.
	if l.state.FirstEpisodeAt.IsZero() {
		return false
	}
	return now.Sub(l.state.FirstEpisodeAt) >= TradeChannelGrace
}

// Trip взводит остановку по внешней причине.
func (l *Ledger) Trip(now time.Time, reason string) {
	l.roll(now)
	l.trip(now, reason)
}

// Tripped сообщает, взведена ли остановка.
func (l *Ledger) Tripped() bool { return l.state.Tripped }

func (l *Ledger) trip(now time.Time, reason string) {
	if l.state.Tripped {
		return
	}
	l.state.Tripped = true
	l.state.TrippedReason = reason
	l.state.TrippedAt = now
}

// roll переводит учёт на новые сутки UTC.
func (l *Ledger) roll(now time.Time) {
	today := day(now)
	if l.state.Day.Equal(today) {
		return
	}
	// Эпизоды скользящего часа переносятся: их окно короче суток, и обнулять
	// его в полночь значило бы слепнуть к частым заливам ровно на час.
	kept := l.state.Episodes
	l.state = State{Day: today, Episodes: kept}
	l.evict(now)
}

// evict выбрасывает эпизоды старше часа.
func (l *Ledger) evict(now time.Time) {
	cutoff := now.Add(-time.Hour)
	kept := l.state.Episodes[:0]
	for _, e := range l.state.Episodes {
		if e.At.After(cutoff) {
			kept = append(kept, e)
		}
	}
	l.state.Episodes = kept
}

func day(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func f(d decimal.Decimal) float64 {
	v, _ := d.Float64()
	return v
}
