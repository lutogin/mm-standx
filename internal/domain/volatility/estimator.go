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
// Максимум окна вдобавок обладает нужным поведением во времени: после крупного
// скачка осторожность сохраняется до конца окна, а затем спадает сама.
type Estimator struct {
	window     time.Duration
	minSamples int
	fullAt     int

	samples  []sample
	lastMark money.Price
	lastAt   time.Time
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
}

func New(cfg Config) *Estimator {
	return &Estimator{
		window:     cfg.Window,
		minSamples: cfg.MinSamples,
		fullAt:     max(cfg.FullConfidenceAt, cfg.MinSamples),
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
	// Пропуск в потоке делает разность несопоставимой с обычным тактом:
	// за пропущенное время рынок мог пройти сколько угодно, и записывать это
	// как «скачок за такт» значило бы завысить оценку без основания.
	if gap := at.Sub(e.lastAt); gap <= 0 || gap > 3*e.tickHint() {
		return
	}

	jump, err := money.RelativeBps(e.lastMark, mark)
	if err != nil {
		return
	}
	e.samples = append(e.samples, sample{at: at, jump: jump.Abs()})
	e.evict(at)
}

// tickHint возвращает ожидаемую длительность такта. Значение зашито
// по измерению: канал price обновляется ровно раз в три секунды.
func (e *Estimator) tickHint() time.Duration { return 3 * time.Second }

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
	// JumpBps — максимальный скачок в окне, основная мера риска.
	JumpBps money.Bps
	// P95Bps — 95-й перцентиль, только для наблюдаемости.
	P95Bps money.Bps
	// Samples — сколько замеров в окне.
	Samples int
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

	out := Estimate{Samples: len(e.samples)}
	if out.Samples == 0 {
		out.Inflation = 1
		return out
	}

	jumps := make([]money.Bps, 0, len(e.samples))
	for _, s := range e.samples {
		jumps = append(jumps, s.jump)
		if s.jump > out.JumpBps {
			out.JumpBps = s.jump
		}
	}
	sort.Slice(jumps, func(i, j int) bool { return jumps[i] < jumps[j] })
	out.P95Bps = jumps[min(int(0.95*float64(len(jumps))), len(jumps)-1)]

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

// Reset забывает историю.
//
// Вызывается при разрыве потока: после паузы прежние замеры описывают другой
// рынок, а склейка через разрыв дала бы ложный скачок.
func (e *Estimator) Reset() {
	e.samples = e.samples[:0]
	e.lastMark = money.Price{}
	e.lastAt = time.Time{}
}
