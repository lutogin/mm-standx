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
	//
	// Достижение предела приостанавливает котирование до тех пор, пока эпизоды
	// не выйдут из скользящего часа, и не взводит остановку: измеряемая
	// величина спадает сама, а реакция, которая сама не спадает, превращает
	// минутный всплеск в потерянные сутки. См. Ledger.pause.
	MaxFillsPerHour int

	// TripOnDrawdown включает остановку по дневному убытку. Выключенный
	// лимит по-прежнему считается и публикуется в метриках, но не тормозит.
	TripOnDrawdown bool

	// Cooldown — сколько остановка по дневному убытку держится, прежде чем
	// снимется сама. Ноль возвращает прежнее поведение: до полуночи UTC.
	//
	// Заведено потому, что прежняя несоразмерность стоила суток присутствия
	// за один залив. 27 августа свип одной ступени дал 71 USD при лимите 60
	// и снял котирование на 9 ч 14 мин — до полуночи UTC. Защита сработала
	// по замыслу, но цена ответа не имела отношения к цене события: убыток
	// был разовым и уже реализованным, а платили за него присутствием.
	//
	// Снятие переносит начало отсчёта убытка на момент снятия (см. release):
	// иначе первая же оценка после паузы увидела бы ту же величину и взвела
	// бы остановку снова, то есть пауза была бы вечной.
	Cooldown time.Duration

	// MaxAutoResumes — сколько раз за сутки UTC остановка снимается сама.
	//
	// Предел существует потому, что перенос отсчёта превращает дневной лимит
	// в оконный: без потолка на число окон сутки стоили бы MaxDailyLossUSD
	// столько раз, сколько их помещается в двадцать четыре часа. Исчерпание
	// возвращает прежнее поведение — следующая остановка держится до полуночи
	// UTC, и худший день ограничен (MaxAutoResumes+1) × MaxDailyLossUSD.
	MaxAutoResumes int
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

	// OpenQty и OpenCashUSD — незакрытая нога: сколько базового актива куплено
	// сверх проданного и во что это обошлось.
	//
	// Держатся отдельно от результата намеренно. Стратегия возвращается
	// в плоскую позицию за доли секунды, поэтому ненулевое значение здесь
	// живёт мгновения; застрявшее говорит, что какое-то исполнение до нас
	// не дошло, и результат за сутки занижен на эту ногу.
	OpenQty     decimal.Decimal `json:"open_qty"`
	OpenCashUSD decimal.Decimal `json:"open_cash_usd"`

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
	// TrippedAt — момент срабатывания, для разбора инцидента. Не очищается
	// при снятии: вопрос «когда сработало в прошлый раз» переживает снятие.
	TrippedAt time.Time `json:"tripped_at"`

	// TrippedUntil — момент, в который остановка снимется сама.
	//
	// Нулевое значение означает, что сама она не снимется: так взводятся
	// остановки по внешней причине (нечитаемый снимок, решение человека)
	// и остановка по убытку после того, как автоснятия за сутки исчерпаны.
	// Снимок прошлой версии без этого поля читается именно так — консервативно.
	TrippedUntil time.Time `json:"tripped_until,omitempty"`

	// AutoResumesToday — сколько раз за эти сутки остановка снялась сама.
	AutoResumesToday int `json:"auto_resumes_today"`

	// WindowBaseNetUSD и WindowBaseCostUSD — счётчики на момент последнего
	// автоснятия. Лимит меряется от них, а не от начала суток.
	//
	// Держатся отдельно от самих счётчиков намеренно: ReportedNetUSD остаётся
	// дневным итогом и печатается как дневной итог. Обнулять его при снятии
	// значило бы потерять ответ на вопрос, во что обошлись сутки целиком.
	WindowBaseNetUSD  decimal.Decimal `json:"window_base_net_usd"`
	WindowBaseCostUSD decimal.Decimal `json:"window_base_cost_usd"`
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

// Execution — одно исполнение, приведённое к деньгам.
type Execution struct {
	// QtyBase — знаковое количество: покупка положительна, продажа отрицательна.
	//
	// Нужно, чтобы отличить незакрытую ногу от результата. Денежный поток сам
	// по себе результатом не является: после покупки он показывает минус на весь
	// нотионал, и до встречной продажи это не убыток, а открытая позиция.
	QtyBase decimal.Decimal
	// CashUSD — знаковый денежный поток: продажа приносит, покупка уносит.
	CashUSD decimal.Decimal
	// FeeUSD — комиссия, всегда расход.
	FeeUSD decimal.Decimal
}

// OnTrade учитывает исполнение по фактическим деньгам.
//
// Считать приходится самим. Канал `trade` не несёт ни комиссии, ни результата:
// это уведомление об исполнении с ценой, количеством и флагом стороны сделки.
// Прежний вариант читал два отсутствующих поля, получал нули и оставлял
// биржевую половину учёта мёртвой — восемнадцать сделок за сутки при
// `reported_net_usd: 0`.
//
// Накапливается денежный поток, а не результат сделки. Стратегия по замыслу
// возвращается в плоскую позицию после каждого эпизода, и в этот момент
// накопленный поток равен реализованному результату в точности. Между
// исполнением и закрытием он расходится на переоценку позиции, но позиция
// живёт доли секунды.
//
// Комиссия вычитается по модулю: мейкерских выплат у программы нет, комиссия
// всегда расход (PLAN.md §1.3). Ошибиться здесь в сторону оптимизма значит
// завысить остаток лимита.
func (l *Ledger) OnTrade(now time.Time, e Execution) {
	l.roll(now)
	l.state.TradeCount++

	l.state.OpenQty = l.state.OpenQty.Add(e.QtyBase)
	l.state.OpenCashUSD = l.state.OpenCashUSD.Add(e.CashUSD).Sub(e.FeeUSD.Abs())

	// Поток становится результатом только когда позиция вернулась в ноль.
	//
	// Иначе первое же исполнение эпизода выглядит как убыток на весь нотионал:
	// покупка 0.07 унции золота это минус 302 USD, и остановка по дневному
	// убытку в 30 срабатывает через 340 мс после залива, ещё до встречной
	// продажи. Ровно это и случилось 7 августа.
	if l.state.OpenQty.IsZero() {
		l.state.ReportedNetUSD = l.state.ReportedNetUSD.Add(l.state.OpenCashUSD)
		l.state.OpenCashUSD = decimal.Zero
	}
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
	// OpenLegUSD — незакрытая нога в деньгах. Ненулевая дольше секунды означает
	// потерянное сообщение об исполнении и заниженный результат.
	OpenLegUSD float64

	// PausedUntil — момент, до которого котирование приостановлено частотой
	// заливов. Нулевое значение означает, что паузы нет.
	//
	// Пауза — не остановка. Она снимается сама, когда эпизоды выходят
	// из скользящего часа, и не требует человека.
	PausedUntil time.Time
	// PauseReason описывает паузу человекочитаемо.
	PauseReason string

	// TrippedUntil — момент, в который остановка снимется сама. Нулевое
	// значение при Tripped означает, что сама она не снимется.
	TrippedUntil time.Time
	// AutoResumesLeft — сколько автоснятий остаётся за эти сутки.
	AutoResumesLeft int
}

// Paused сообщает, действует ли пауза на момент now.
func (v Verdict) Paused(now time.Time) bool {
	return !v.PausedUntil.IsZero() && now.Before(v.PausedUntil)
}

// Assess выносит вердикт и, при достижении лимита, взводит остановку.
//
// Остановка по дневному убытку снимается сама по истечении Cooldown, и это
// правка прежнего решения, а не отступление от него. Прежде она держалась
// до полуночи UTC, и возражение против автоснятия было такое: защита,
// снимающая себя, срабатывает ровно столько раз, сколько нужно, чтобы
// исчерпать депозит по частям. Возражение верное, и отвечает на него не
// длительность паузы, а MaxAutoResumes: число окон за сутки ограничено,
// поэтому худший день по-прежнему ограничен суммой, а не временем суток.
//
// Что осталось прежним: остановка по внешней причине — нечитаемый снимок,
// решение человека — не снимается сама никогда (см. Trip). Там условие
// не спадает со временем, и снимать его должен тот, кто в нём разобрался.
func (l *Ledger) Assess(now time.Time) Verdict {
	l.roll(now)
	l.evict(now)
	l.release(now)

	v := Verdict{
		ReportedNetUSD:      f(l.state.ReportedNetUSD),
		EstimatedCostUSD:    f(l.state.EstimatedCostUSD),
		FillsLastHour:       len(l.state.Episodes),
		EpisodesToday:       l.state.EpisodesToday,
		TradeChannelSuspect: l.tradeChannelSuspect(now),
		OpenLegUSD:          f(l.state.OpenCashUSD),
	}

	// The exchange result is the measure of the day's loss. The cost model
	// substitutes for it only while the trade channel is silent.
	//
	// The two are not the same quantity. ReportedNetUSD is a result and can be
	// positive; EstimatedCostUSD is gross turnover cost, it only ever grows, and
	// nothing in it subtracts the reward the strategy is paid. Taking the worse
	// of the two unconditionally therefore halts on volume rather than on loss:
	// at ~6 bps per fill and a 10 USD limit that is a dozen fills, reached in a
	// normal day by a strategy that is making money. The dual accounting was
	// written for a broken trade channel (see EstimatedCostUSD), and that is the
	// only case where it may speak for the exchange.
	//
	// Меряется окно, а не сутки: после автоснятия отсчёт начинается заново
	// (см. release). Пока автоснятий не было, база нулевая и окно совпадает
	// с сутками — прежнее поведение.
	reportedLoss := -f(l.state.ReportedNetUSD.Sub(l.state.WindowBaseNetUSD))
	v.LossUSD = reportedLoss
	if v.TradeChannelSuspect {
		v.LossUSD = max(reportedLoss, f(l.state.EstimatedCostUSD.Sub(l.state.WindowBaseCostUSD)))
	}

	// Частота заливов приостанавливает котирование, но не останавливает его.
	v.PausedUntil, v.PauseReason = l.pause(now)

	switch {
	case l.state.Tripped:
		v.Tripped, v.Reason = true, l.state.TrippedReason

	case l.limits.TripOnDrawdown && l.limits.MaxDailyLossUSD > 0 &&
		v.LossUSD >= l.limits.MaxDailyLossUSD:
		l.tripReleasable(now, fmt.Sprintf(
			"daily loss %.2f USD reached the %.2f limit (exchange reported %.2f, cost model %.2f)",
			v.LossUSD, l.limits.MaxDailyLossUSD, v.ReportedNetUSD, v.EstimatedCostUSD))
		v.Tripped, v.Reason = true, l.state.TrippedReason
	}

	v.TrippedUntil = l.state.TrippedUntil
	v.AutoResumesLeft = l.autoResumesLeft()
	return v
}

// release снимает остановку по дневному убытку, когда её срок истёк.
//
// Перенос базы обязателен и не является поблажкой. Величина, взведшая
// остановку, реализована и никуда не денется до полуночи UTC; оставить отсчёт
// от начала суток значило бы, что первая же оценка после снятия увидит тот же
// убыток и взведёт остановку снова — в тот же миг, навсегда, с точностью до
// такта цикла. Отсчёт поэтому начинается заново, а от бесконечного повторения
// защищает не он, а MaxAutoResumes.
func (l *Ledger) release(now time.Time) {
	if !l.state.Tripped || l.state.TrippedUntil.IsZero() || now.Before(l.state.TrippedUntil) {
		return
	}
	l.state.Tripped = false
	l.state.TrippedReason = ""
	l.state.TrippedUntil = time.Time{}
	l.state.AutoResumesToday++
	l.state.WindowBaseNetUSD = l.state.ReportedNetUSD
	l.state.WindowBaseCostUSD = l.state.EstimatedCostUSD
}

// autoResumesLeft — сколько автоснятий осталось за эти сутки.
func (l *Ledger) autoResumesLeft() int {
	if l.limits.Cooldown <= 0 {
		return 0
	}
	return max(0, l.limits.MaxAutoResumes-l.state.AutoResumesToday)
}

// pause сообщает, до какого момента частота заливов запрещает котировать.
//
// MaxFillsPerHour измеряет скользящий час — величину, которая спадает сама.
// Раньше её достижение взводило ту же остановку, что и дневной убыток, и это
// был главный источник зависаний: условие исчезало через час вместе с
// эпизодами, а флаг оставался до полуночи UTC и переживал перезапуск. Всплеск
// на минуту выключал бота на сутки.
//
// Ответ на транзиентное рассогласование обязан быть транзиентным. Пауза длится
// ровно до момента, когда из окна выпадет столько эпизодов, что счётчик
// опустится ниже предела, — то есть ни на секунду дольше, чем держится сама
// причина. Дневной убыток при этом по-прежнему останавливает торговлю
// насовсем: там условие не спадает, и снимать его должен человек.
func (l *Ledger) pause(now time.Time) (time.Time, string) {
	limit := l.limits.MaxFillsPerHour
	if limit <= 0 || len(l.state.Episodes) < limit {
		return time.Time{}, ""
	}

	// Выпадение эпизода с этим индексом опускает счётчик до limit-1.
	oldest := l.state.Episodes[len(l.state.Episodes)-limit]
	until := oldest.At.Add(time.Hour)
	if !until.After(now) {
		return time.Time{}, ""
	}
	return until, fmt.Sprintf(
		"%d fills in the last hour reached the %d limit: the distance model disagrees "+
			"with the market, standing down until %s",
		len(l.state.Episodes), limit, until.UTC().Format(time.RFC3339))
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
//
// Такая остановка не снимается сама ни при каком Cooldown. Причина у неё
// не денежная и со временем не спадает: нечитаемый снимок через полтора часа
// читаемым не станет, а решение человека отменяет человек.
func (l *Ledger) Trip(now time.Time, reason string) {
	l.roll(now)
	l.trip(now, reason)
}

// Tripped сообщает, взведена ли остановка.
func (l *Ledger) Tripped() bool { return l.state.Tripped }

// tripReleasable взводит остановку, которая снимется сама по истечении
// Cooldown, — если автоснятия за сутки ещё не исчерпаны. Когда исчерпаны,
// остановка получается обычной: до полуночи UTC.
func (l *Ledger) tripReleasable(now time.Time, reason string) {
	if l.state.Tripped {
		return
	}
	l.trip(now, reason)
	if l.autoResumesLeft() > 0 {
		l.state.TrippedUntil = now.Add(l.limits.Cooldown)
	}
}

func (l *Ledger) trip(now time.Time, reason string) {
	if l.state.Tripped {
		return
	}
	l.state.Tripped = true
	l.state.TrippedReason = reason
	l.state.TrippedAt = now
	l.state.TrippedUntil = time.Time{}
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
