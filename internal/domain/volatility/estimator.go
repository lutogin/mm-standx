// Package volatility оценивает, насколько далеко mark price способен уйти
// за один такт.
//
// Оценка нужна правилу дистанции (policy.Appetite) и определяет, насколько
// глубоко в стакане стоит котировка. Пакет чистый: время приходит параметром.
package volatility

import (
	"math"
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
// Максимум окна вдобавок обладает нужным поведением во времени: после крупного
// скачка осторожность сохраняется до конца окна, а затем спадает сама.
type Estimator struct {
	window         time.Duration
	minSamples     int
	fullAt         int
	halfLife       time.Duration
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

	// memory — затухающий рекорд: максимум скачка, забываемый экспоненциально
	// с полураспадом halfLife. Живёт дольше окна и служит нижней границей меры
	// риска, см. Config.MemoryHalfLife.
	memory   money.Bps
	memoryAt time.Time
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
		halfLife:       cfg.MemoryHalfLife,
		windowMultiple: cfg.MemoryWindowMultiple,
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
	if gap > gapTolerance*tick {
		return
	}

	jump, err := money.RelativeBps(e.lastMark, mark)
	if err != nil {
		return
	}
	e.samples = append(e.samples, sample{at: at, jump: jump.Abs()})
	e.remember(jump.Abs(), at)
	e.evict(at)
}

// remember обновляет затухающий рекорд.
func (e *Estimator) remember(jump money.Bps, at time.Time) {
	if e.halfLife <= 0 {
		return
	}
	e.memory = max(e.decayed(at), jump)
	e.memoryAt = at
}

// decayed возвращает рекорд, приведённый к моменту now.
//
// Метод не изменяет состояние: оценка запрашивается на каждом служебном тике,
// а мутация ради чтения превратила бы частоту опроса в параметр модели риска.
func (e *Estimator) decayed(now time.Time) money.Bps {
	if e.halfLife <= 0 || e.memory <= 0 || e.memoryAt.IsZero() {
		return 0
	}
	age := now.Sub(e.memoryAt)
	if age <= 0 {
		return e.memory
	}
	return e.memory * money.Bps(math.Exp2(-age.Seconds()/e.halfLife.Seconds()))
}

const (
	// gapMemory — сколько интервалов участвует в оценке такта.
	gapMemory = 32
	// minGapsForTick — сколько интервалов нужно, чтобы такт считался известным.
	minGapsForTick = 8
	// gapTolerance — во сколько тактов должен уложиться интервал, чтобы
	// считаться обычным, а не пропуском.
	gapTolerance = 3
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
	// JumpBps — принятая мера риска: больший из максимума окна и памяти.
	JumpBps money.Bps
	// WindowMaxBps — максимум скачка в скользящем окне.
	WindowMaxBps money.Bps
	// MemoryBps — затухающий рекорд, приведённый к текущему моменту.
	// Отдельное поле нужно, чтобы по метрикам было видно, какая из двух
	// величин связывает оценку прямо сейчас.
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

	out := Estimate{Samples: len(e.samples), MemoryBps: e.decayed(now)}
	out.Tick, _ = e.tick()
	if out.Samples == 0 {
		out.Inflation = 1
		out.JumpBps = out.MemoryBps
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

	// Память служит нижней границей: спокойное окно не отменяет того,
	// что рынок уже показывал несколькими часами раньше. Но и требовать
	// сколько угодно она не вправе — см. windowMultiple.
	out.JumpBps = max(out.WindowMaxBps, e.bounded(out))

	// Малая выборка систематически недооценивает хвост: по n замерам виден
	// в лучшем случае квантиль (1 − 1/n), всё что реже — не наблюдалось вовсе.
	// Погрешность квантильной оценки убывает как 1/√n, поэтому запас
	// раздувается в той же пропорции.
	out.Inflation = 1 + (1 - out.Confidence)
	return out
}

// bounded limits what the decaying record may claim over the live window.
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
// by a multiple of the window maximum buys the same property in one comparison:
// the memory converges to what the market currently shows instead of to zero on
// a schedule.
//
// The bound is deliberately not applied while the estimate is still warming up.
// An empty or short window says nothing about the market, and reading its
// maximum as evidence of calm would erase the record exactly when it is the only
// thing left — right after a reconnect, which tends to coincide with the
// turbulence that caused it.
// A window whose maximum is exactly zero is the second exemption. A market that
// has not moved at all across the whole window carries no information about
// magnitude, and multiplying that nothing by anything still yields nothing —
// the very failure the record was written to cover. There the record stands
// unbounded and the absolute floor (policy.Bounds.MinBps) backs it up.
func (e *Estimator) bounded(out Estimate) money.Bps {
	if e.windowMultiple <= 0 || !out.Ready || out.WindowMaxBps <= 0 {
		return out.MemoryBps
	}
	return min(out.MemoryBps, out.WindowMaxBps*money.Bps(e.windowMultiple))
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
// Затухающий рекорд при этом сохраняется намеренно. Разрыв соединения —
// не довод в пользу того, что рынок успокоился; скорее наоборот, всплески
// нагрузки на биржу и всплески волатильности приходят вместе. Забыв память,
// бот вернулся бы после реконнекта с заниженной мерой риска и подошёл бы
// к рынку ближе именно в тот момент, когда для этого меньше всего оснований.
func (e *Estimator) Reset() {
	e.samples = e.samples[:0]
	e.lastMark = money.Price{}
	e.lastAt = time.Time{}
}
