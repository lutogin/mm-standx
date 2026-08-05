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
	window     time.Duration
	minSamples int
	fullAt     int
	halfLife   time.Duration

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

	// MemoryHalfLife — полураспад памяти о крупных скачках.
	//
	// Существует потому, что поправка на зрелость оценки не закрывает случай,
	// ради которого написана. Inflation умножает измеренный скачок, а на
	// замершем рынке замеры идут исправно и оценка объявляет себя зрелой при
	// скачке, близком к нулю: любой множитель на нуле даёт ноль. Ровно этот
	// случай и описывает выходные, когда базовые рынки закрыты, — а понедельник
	// приходит с той же волатильностью, что была в пятницу.
	//
	// Память устроена как затухающий рекорд: max(наблюдённый скачок,
	// прежний рекорд с поправкой на затухание). Для меры, которая сама является
	// максимумом, это честнее сглаживания — усреднение размывало бы ровно тот
	// выброс, ради запоминания которого механизм и нужен.
	//
	// Ноль отключает память: оценка сводится к максимуму окна.
	MemoryHalfLife time.Duration
}

func New(cfg Config) *Estimator {
	return &Estimator{
		window:     cfg.Window,
		minSamples: cfg.MinSamples,
		fullAt:     max(cfg.FullConfidenceAt, cfg.MinSamples),
		halfLife:   cfg.MemoryHalfLife,
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

	// Память служит нижней границей: спокойное окно не отменяет того,
	// что рынок уже показывал несколькими часами раньше.
	out.JumpBps = max(out.WindowMaxBps, out.MemoryBps)

	out.Confidence = min(float64(out.Samples)/float64(e.fullAt), 1)
	out.Ready = out.Samples >= e.minSamples

	// Малая выборка систематически недооценивает хвост: по n замерам виден
	// в лучшем случае квантиль (1 − 1/n), всё что реже — не наблюдалось вовсе.
	// Погрешность квантильной оценки убывает как 1/√n, поэтому запас
	// раздувается в той же пропорции.
	out.Inflation = 1 + (1 - out.Confidence)
	return out
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
