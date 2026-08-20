// Package volatility оценивает, насколько далеко mark price способен уйти
// за один такт.
//
// Оценка нужна правилу дистанции (policy.Appetite) и определяет, насколько
// глубоко в стакане стоит котировка. Пакет чистый: время приходит параметром.
package volatility

import (
	"sort"
	"time"

	"github.com/nd/mm-standx/internal/domain/money"
)

// sample — один наблюдённый скачок.
type sample struct {
	at   time.Time
	jump money.Bps
}

// Estimator ведёт скользящее окно скачков mark price между тактами.
//
// В качестве меры риска берётся максимум окна, а не высокий перцентиль.
// Причина в измерениях: у BTC 95-й перцентиль скачка составил 0.45 bps
// при максимуме 4.40 за те же два часа, то есть хвост в десять раз толще
// тела распределения (PLAN.md §1.4). Правило, откалиброванное по перцентилю,
// было бы пробито первым же выбросом.
//
// Максимум вдобавок обладает нужным поведением во времени: после крупного
// скачка осторожность сохраняется и спадает сама. С 19 августа берётся
// не сырой максимум окна, а его затухающая версия (поле fast): та же
// нечувствительность к телу распределения, но без плоской полки на всё окно
// и обрыва на его краю.
type Estimator struct {
	window         time.Duration
	minSamples     int
	fullAt         int
	windowMultiple float64

	samples  []sample
	lastMark money.Price
	lastAt   time.Time

	// gaps — недавние интервалы между обновлениями, по ним измеряется такт.
	//
	// Такт нельзя было оставить константой. Три секунды измерены на BTC, ETH
	// и металлах, но у менее ликвидных инструментов цена идёт заметно реже,
	// и фильтр, отбрасывающий интервалы длиннее трёх тактов, выбрасывал у них
	// почти все наблюдения. Оценка не набирала минимума и не выходила
	// из прогрева никогда — инструмент молчал не потому, что опасен,
	// а потому что мерка была чужая.
	gaps []time.Duration

	// fast — быстрый затухающий рекорд, основной источник JumpBps.
	//
	// До 19 августа эту роль играл сырой максимум скользящего окна (см. поле
	// window выше): плоская полка на всё время, что тик остаётся в пределах
	// окна, обрыв разом, когда выпадает. Обрыв — не спад: рынок мог остыть
	// много раньше, а мера риска об этом не узнавала, пока не пройдёт всё
	// окно. Измерено на боевом счёте 19 августа при window: 8m: 52% плато
	// держались дольше пяти минут, и почти все длинные — одно и то же число
	// без изменений всё время, то есть один тик, а не серия. Короткий
	// полураспад вместо жёсткого окна отпускает такой тик за минуты, а не
	// ждёт края окна.
	//
	// Повторный сопоставимый скачок до полного затухания взводит запись
	// заново — тем самым же механизмом, что и memory. Настоящую серию это
	// не отпускает: 19 августа тем же вечером час 15:00 UTC дал один тик
	// на 26.37 bps при полностью пустой стороне стакана (depth_ask_usd: 0) —
	// ровно то, ради чего мера и существует, и короткий полураспад эту
	// реакцию не ослабляет: величина скачка та же, отпускает только время.
	fast decayingMax

	// memory — медленный затухающий рекорд, внешний пол для fast.
	//
	// Полураспад на порядки длиннее — часы, не минуты — и назначение другое:
	// не «что произошло только что», а «не забывать всплеск быстрее, чем
	// он того заслуживает», см. Config.MemoryHalfLife и bounded.
	memory decayingMax
}

// Config — параметры оценщика.
type Config struct {
	// Window — глубина скользящего окна.
	Window time.Duration
	// MinSamples — сколько замеров нужно, чтобы вообще начать котировать.
	//
	// Ниже этого порога оценка не считается пригодной, и правильное поведение —
	// не выставлять ордера. Это же условие срабатывает после переподключения,
	// когда история обнулилась.
	MinSamples int
	// FullConfidenceAt — при каком числе замеров оценка считается зрелой.
	// Между MinSamples и этим значением требуемый запас искусственно завышается.
	FullConfidenceAt int

	// WindowHalfLife is the half-life of the fast record that primarily drives
	// JumpBps — see Estimator.fast.
	//
	// Required, not optional like MemoryHalfLife: zero here would not merely
	// disable a floor, it would collapse the whole outer max() to whatever the
	// slow memory term computes unbounded (bounded's fastMax<=0 exemption),
	// silently reviving the multi-hour drag memory's own window bound exists
	// to prevent. A load-bearing parameter should not have a quiet off switch.
	//
	// Choose it meaningfully shorter than Window: at or above Window it stops
	// being faster than the behaviour it replaces and the fix does nothing.
	WindowHalfLife time.Duration

	// MemoryHalfLife is the half-life of the record of large jumps.
	//
	// It exists because the maturity correction cannot cover the case it was
	// written for. Inflation multiplies the measured jump, but on a frozen
	// market the samples keep arriving, the estimate declares itself mature at
	// a jump close to zero, and any multiple of zero is zero.
	//
	// The original justification was the weekend: the reference markets shut,
	// and Monday arrives with the volatility Friday had. **That reason is
	// obsolete** — the shutdown is now described directly by reward.Schedule,
	// and instruments whose reference market closes are simply not quoted while
	// it is (PLAN.md §12.14). Do not defend this parameter with the weekend.
	//
	// What still justifies it is shorter and sharper: the window is twenty
	// minutes wide and forgets a burst completely, while volatility clusters —
	// the minutes after a spike are the ones most likely to hold the next one.
	// The live fill of 6 August took exactly that shape: hours of sub-basis-point
	// ticks, then 5.38 bps in a single tick that swept all three rungs in 51 ms.
	// A shorter memory would have walked the quotes back in towards the market
	// precisely then.
	//
	// The cost is real and visible: through 6 August the memory, not the
	// measured window, was what kept the actor out of the book for hours at a
	// stretch. Whether that trade is worth it is arithmetic, not taste, and it
	// needs two numbers we do not have yet — the price of a Maker Hour and the
	// fill rate at a given distance. Until both are measured, leave it alone.
	//
	// The memory is a decaying record: max(observed jump, previous record after
	// decay). For a measure that is itself a maximum this is more honest than
	// smoothing, which would blur away the very outlier the mechanism exists to
	// remember.
	//
	// Zero disables the memory: the estimate reduces to the window maximum.
	MemoryHalfLife time.Duration

	// MemoryWindowMultiple bounds the record by the live window: the memory may
	// never claim more than this multiple of the window maximum.
	//
	// It exists because the half-life alone cannot express what the record is
	// for. Six hours was chosen to carry Friday's volatility across the weekend,
	// and it does not even do that — 51 hours of decay leave 0.3% of the record.
	// What it does instead is hand one outlier control of the risk measure for
	// most of a working day: on 7 August a one-minute spike kept the actor out
	// of the book for 8.4 hours while the window reported a tenth of the value.
	//
	// The bound replaces the guess with a comparison. Details and the parallel
	// with EWMA and GARCH are in the estimator's bounded method.
	//
	// Zero disables the bound and restores the unconditional record.
	MemoryWindowMultiple float64
}

func New(cfg Config) *Estimator {
	return &Estimator{
		window:         cfg.Window,
		minSamples:     cfg.MinSamples,
		fullAt:         max(cfg.FullConfidenceAt, cfg.MinSamples),
		windowMultiple: cfg.MemoryWindowMultiple,
		fast:           decayingMax{halfLife: cfg.WindowHalfLife},
		memory:         decayingMax{halfLife: cfg.MemoryHalfLife},
	}
}

// Observe принимает очередной такт mark price.
//
// Первый вызов после создания или после разрыва задаёт точку отсчёта и скачка
// не даёт: разность с ценой часовой давности не характеризует такт.
func (e *Estimator) Observe(mark money.Price, at time.Time) {
	defer func() { e.lastMark, e.lastAt = mark, at }()

	if !e.lastMark.IsPositive() || !mark.IsPositive() {
		return
	}
	gap := at.Sub(e.lastAt)
	if gap <= 0 {
		return
	}
	e.observeGap(gap)

	// Пока такт инструмента не измерен, судить о том, нормален ли интервал,
	// не по чему. Наблюдения в этот период идут только на измерение такта:
	// записать их как скачки значило бы рискнуть занести в долгую память
	// склейку через разрыв, а она держится часами.
	tick, known := e.tick()
	if !known {
		return
	}
	// Пропуск в потоке делает разность несопоставимой с обычным тактом:
	// за пропущенное время рынок мог пройти сколько угодно, и записывать это
	// как «скачок за такт» значило бы завысить оценку без основания.
	//
	// Длина пропуска при этом различает два разных случая, и раньше они были
	// склеены. Мигание соединения на секунду оставляет окно верным описанием
	// рынка — это тот же рынок, мы просто моргнули. Настоящий перерыв делает
	// накопленные замеры описанием другого рынка, и держать их нельзя.
	// Признаком служит измеренный пропуск, а не факт переподключения: 10 августа
	// биржа отдала 36 разрывов за восемь часов, почти все короче секунды
	// (502 на редиале), и каждый обнулял двадцать минут наблюдений.
	if gap > gapTolerance*tick {
		if gap > windowStaleGap*tick {
			e.samples = e.samples[:0]
		}
		return
	}

	jump, err := money.RelativeBps(e.lastMark, mark)
	if err != nil {
		return
	}
	e.samples = append(e.samples, sample{at: at, jump: jump.Abs()})
	// Оба рекорда получают наблюдение одинаково — каждый решает сам, забыть
	// его быстро или держать часами.
	e.fast.observe(jump.Abs(), at)
	e.memory.observe(jump.Abs(), at)
	e.evict(at)
}

const (
	// gapMemory — сколько интервалов участвует в оценке такта.
	gapMemory = 32
	// minGapsForTick — сколько интервалов нужно, чтобы такт считался известным.
	minGapsForTick = 8
	// gapTolerance — во сколько тактов должен уложиться интервал, чтобы
	// считаться обычным, а не пропуском.
	gapTolerance = 3
	// windowStaleGap — с какого пропуска окно перестаёт описывать текущий рынок.
	//
	// Двадцать тактов — около минуты на золоте. Ниже этого перерыв не даёт
	// оснований забывать наблюдения: рынок за минуту не становится другим,
	// а прогрев заново стоит минуты присутствия, то есть ровно того, чем
	// стратегия зарабатывает.
	windowStaleGap = 20
)

// observeGap запоминает интервал между обновлениями.
func (e *Estimator) observeGap(gap time.Duration) {
	e.gaps = append(e.gaps, gap)
	if len(e.gaps) > gapMemory {
		e.gaps = append(e.gaps[:0], e.gaps[1:]...)
	}
}

// tick возвращает измеренный такт инструмента.
//
// Берётся медиана, а не среднее: одиночный разрыв соединения растянул бы
// среднее настолько, что после него нормальными считались бы любые пропуски —
// то есть фильтр отключил бы сам себя ровно тогда, когда он нужен.
func (e *Estimator) tick() (time.Duration, bool) {
	if len(e.gaps) < minGapsForTick {
		return 0, false
	}
	sorted := append(make([]time.Duration, 0, len(e.gaps)), e.gaps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2], true
}

func (e *Estimator) evict(now time.Time) {
	cutoff := now.Add(-e.window)
	keep := 0
	for i, s := range e.samples {
		if s.at.After(cutoff) {
			keep = i
			break
		}
		keep = i + 1
	}
	if keep > 0 {
		e.samples = append(e.samples[:0], e.samples[keep:]...)
	}
}

// Estimate — снимок оценки.
type Estimate struct {
	// JumpBps — принятая мера риска: больший из fast и ограниченной memory.
	JumpBps money.Bps
	// FastBps — быстрый затухающий рекорд, приведённый к текущему моменту.
	// Обычно и есть JumpBps: см. Estimator.fast.
	FastBps money.Bps
	// WindowMaxBps — сырой максимум скачка в скользящем окне, без затухания.
	// Держится только для наблюдаемости и сравнения с FastBps в логах —
	// решение больше не принимается по этому полю напрямую.
	WindowMaxBps money.Bps
	// MemoryBps — медленный затухающий рекорд, приведённый к текущему моменту.
	// Отдельное поле нужно, чтобы по метрикам было видно, какая из величин
	// связывает оценку прямо сейчас.
	MemoryBps money.Bps
	// P95Bps — 95-й перцентиль, только для наблюдаемости.
	P95Bps money.Bps
	// Samples — сколько замеров в окне.
	Samples int
	// Tick — измеренный такт инструмента: окно, в котором мы слепы.
	Tick time.Duration
	// Confidence — зрелость оценки от 0 до 1.
	Confidence float64
	// Ready сообщает, что оценке можно доверять для выставления ордеров.
	Ready bool
	// Inflation — множитель запаса, компенсирующий незрелость оценки.
	Inflation float64
}

// Estimate возвращает текущую оценку.
func (e *Estimator) Estimate(now time.Time) Estimate {
	e.evict(now)

	fast := e.fast.decayed(now)
	out := Estimate{Samples: len(e.samples), FastBps: fast, MemoryBps: e.memory.decayed(now)}
	out.Tick, _ = e.tick()
	if out.Samples == 0 {
		out.Inflation = 1
		out.JumpBps = max(fast, e.bounded(out, fast))
		return out
	}

	jumps := make([]money.Bps, 0, len(e.samples))
	for _, s := range e.samples {
		jumps = append(jumps, s.jump)
		if s.jump > out.WindowMaxBps {
			out.WindowMaxBps = s.jump
		}
	}
	sort.Slice(jumps, func(i, j int) bool { return jumps[i] < jumps[j] })
	out.P95Bps = jumps[min(int(0.95*float64(len(jumps))), len(jumps)-1)]

	out.Confidence = min(float64(out.Samples)/float64(e.fullAt), 1)
	out.Ready = out.Samples >= e.minSamples

	// fast — основной источник, память служит нижней границей: спокойный
	// fast не отменяет того, что рынок уже показывал часами раньше. Но
	// и требовать сколько угодно она не вправе — см. bounded. Ограничение
	// считается от fast, а не от сырого WindowMaxBps: тот больше не участвует
	// в решении вообще, только в наблюдаемости.
	out.JumpBps = max(fast, e.bounded(out, fast))

	// Малая выборка систематически недооценивает хвост: по n замерам виден
	// в лучшем случае квантиль (1 − 1/n), всё что реже — не наблюдалось вовсе.
	// Погрешность квантильной оценки убывает как 1/√n, поэтому запас
	// раздувается в той же пропорции.
	out.Inflation = 1 + (1 - out.Confidence)
	return out
}

// bounded limits what the slow record may claim over the fast one.
//
// A decaying maximum has a property no volatility estimator should have: it
// does not revert. Subsequent observations cannot lower it — only the clock
// can. One outlier therefore governs the risk measure for a fixed stretch of
// wall time no matter what the market does afterwards, and on 7 August that
// stretch was 8.4 hours after a spike that lasted a minute, while the window
// had been reporting a tenth of the value for half an hour.
//
// Standard estimators — EWMA of squared returns, GARCH — avoid this by
// reverting towards the recent level: every calm observation pulls the estimate
// down, and decay counts in observations rather than hours. Bounding the record
// by a multiple of a faster reference buys the same property in one comparison:
// the slow memory converges to what the market currently shows instead of to
// zero on a schedule.
//
// Bounded by fast now, not by the raw window maximum. Before 19 August it was
// the latter, and that quietly defeated the whole point of fast having a short
// half-life: with windowMultiple back above 1 (needed for this bound to matter
// at all — see fast's own doc), bounded(memory) could sit at the still-flat raw
// maximum for as long as that one sample stayed in the window, and the outer
// max(fast, bounded(...)) would keep picking that flat value right through the
// stretch fast was supposed to be tapering. Bounding by fast instead means both
// terms feeding the outer max now move on the same short clock.
//
// The bound is deliberately not applied while the estimate is still warming up.
// An empty or short window says nothing about the market, and reading its
// maximum as evidence of calm would erase the record exactly when it is the only
// thing left — right after a reconnect, which tends to coincide with the
// turbulence that caused it.
// A fast record of exactly zero is the second exemption. A market that has not
// moved at all across its whole span carries no information about magnitude,
// and multiplying that nothing by anything still yields nothing — the very
// failure the record was written to cover. There the record stands unbounded
// and the absolute floor (policy.Bounds.MinBps) backs it up.
func (e *Estimator) bounded(out Estimate, fast money.Bps) money.Bps {
	if e.windowMultiple <= 0 || !out.Ready || fast <= 0 {
		return out.MemoryBps
	}
	return min(out.MemoryBps, fast*money.Bps(e.windowMultiple))
}

// EffectiveJump возвращает меру риска с поправкой на незрелость оценки.
//
// Это то значение, которое следует подавать в правило дистанции: пока данных
// мало, оно завышено, и котировка стоит дальше от рынка.
func (e Estimate) EffectiveJump() money.Bps {
	return e.JumpBps * money.Bps(e.Inflation)
}

// Reset забывает окно наблюдений.
//
// Вызывается при разрыве потока: после паузы прежние замеры описывают другой
// рынок, а склейка через разрыв дала бы ложный скачок. Право котировать
// зарабатывается заново, потому что Ready считается по окну.
//
// Оба затухающих рекорда — fast и memory — при этом сохраняются намеренно.
// Разрыв соединения не довод в пользу того, что рынок успокоился; скорее
// наоборот, всплески нагрузки на биржу и всплески волатильности приходят
// вместе. Забыв их, бот вернулся бы после реконнекта с заниженной мерой
// риска и подошёл бы к рынку ближе именно в тот момент, когда для этого
// меньше всего оснований.
func (e *Estimator) Reset() {
	e.samples = e.samples[:0]
	e.lastMark = money.Price{}
	e.lastAt = time.Time{}
}
