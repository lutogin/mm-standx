// Package stats сохраняет историю показателей на диск.
//
// Реестр показателей живёт в памяти процесса и отвечает по HTTP одним
// мгновенным снимком. Без внешнего сборщика это означает, что за неделю работы
// останется одно число «сейчас» вместо ряда, и вопрос «как мы отработали
// во вторник» окажется без ответа.
//
// Поднимать ради этого Prometheus с хранилищем несоразмерно задаче: рядов
// два десятка, а разбирать их будет человек по ssh. Поэтому сборщик пишет
// файл, и файл этот забирается с сервера как обычный файл.
package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nd/mm-standx/internal/ports"
)

// Collector накапливает замеры и раз в час сбрасывает свёртку на диск.
//
// Час выбран не произвольно: биржа считает начисление по календарным часам UTC,
// поэтому именно час — естественная единица отчёта. Свёртка по получасам
// не соответствовала бы ничему, а по суткам скрыла бы, что рабочими были
// только вечерние часы.
type Collector struct {
	dir      string
	source   ports.MetricsSource
	clock    ports.Clock
	log      *slog.Logger
	interval time.Duration

	hour   time.Time
	series map[string]*series

	// carry — последнее значение каждого накопительного счётчика, перенесённое
	// через границу часа.
	//
	// Без него приращение считалось от первого значения, увиденного внутри
	// часа, а ряд с метками материализуется в реестре только при первом
	// событии. Первое событие каждой серии поэтому становилось базой и
	// пропадало из отчёта: залив 6 августа в 14:34 дал в свёртке delta 0.
	carry map[string]float64

	// lastCurrent — когда снимок текущего часа записывался в последний раз.
	lastCurrent time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// series — накопитель по одному ряду (имя показателя плюс набор меток).
type series struct {
	name   string
	labels map[string]string
	kind   ports.MetricKind

	// values — замеры мгновенной величины за текущий час.
	values []float64
	// first и last — границы накопительного счётчика за час.
	first, last float64
	seen        bool
}

// New создаёт сборщик. Пустой каталог отключает его.
func New(
	dir string,
	interval time.Duration,
	source ports.MetricsSource,
	clk ports.Clock,
	log *slog.Logger,
) *Collector {
	return &Collector{
		dir:      dir,
		source:   source,
		clock:    clk,
		log:      log.With("component", "stats"),
		interval: interval,
		series:   make(map[string]*series),
		carry:    make(map[string]float64),
		done:     make(chan struct{}),
	}
}

func (c *Collector) Name() string { return "stats" }

func (c *Collector) Start(context.Context) error {
	if c.dir == "" {
		c.log.Info("hourly statistics are disabled: no directory configured")
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("stats: create %s: %w", c.dir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.hour = c.clock.Now().UTC().Truncate(time.Hour)

	go func() {
		defer close(c.done)
		c.loop(ctx)
	}()
	c.log.Info("hourly statistics enabled", "dir", c.dir, "sample", c.interval)
	return nil
}

func (c *Collector) Stop(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()

	select {
	case <-c.done:
	case <-ctx.Done():
		return fmt.Errorf("stats: did not stop in time: %w", ctx.Err())
	}
	// Незавершённый час всё равно записывается, помеченный как неполный:
	// потерять последние сорок минут работы перед разбором инцидента —
	// ровно то, ради чего сбор и заводился.
	c.flush(c.clock.Now(), true)
	return nil
}

func (c *Collector) loop(ctx context.Context) {
	ticker := c.clock.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			c.sample(c.clock.Now())
		}
	}
}

// sample снимает показания и, при смене часа, записывает свёртку.
func (c *Collector) sample(now time.Time) {
	if hour := now.UTC().Truncate(time.Hour); !hour.Equal(c.hour) {
		c.flush(now, false)
		c.hour = hour
	}

	for _, s := range c.source.Snapshot() {
		key := seriesKey(s.Name, s.Labels)
		acc, ok := c.series[key]
		if !ok {
			acc = &series{name: s.Name, labels: s.Labels, kind: s.Kind}
			// База накопительного счётчика — то, чем он закончил прошлый час,
			// а не то, чем начал этот. Ряда могло не быть в реестре вовсе:
			// он появляется вместе с первым событием, и это событие обязано
			// попасть в отчёт.
			if s.Kind == ports.KindCounter {
				acc.first, acc.seen = c.carry[key], true
			}
			c.series[key] = acc
		}

		switch s.Kind {
		case ports.KindCounter:
			if !acc.seen {
				acc.first, acc.seen = s.Value, true
			}
			acc.last = s.Value
		default:
			acc.values = append(acc.values, s.Value)
		}
	}
	c.writeCurrent(now)
}

const (
	// currentName — файл с незавершённым часом.
	currentName = "current.json"

	// currentEvery — как часто перезаписывается снимок текущего часа.
	//
	// Реже, чем снимаются показания: замеры копятся в памяти, а на диск
	// уходит только результат. Писать файл каждые пятнадцать секунд значило бы
	// трепать диск ради данных, которые всё равно смотрят раз в несколько
	// минут. Торгового цикла это в любом случае не касается — сборщик живёт
	// в своей горутине.
	currentEvery = 10 * time.Minute
)

// writeCurrent перезаписывает снимок часа, который идёт прямо сейчас.
//
// Свёртка по построению готова только к концу часа, и без этого файла первые
// до пятидесяти девяти минут работы смотреть было бы нечего — состояние,
// неотличимое от поломки для того, кто только что запустил бота и хочет
// убедиться, что он считает.
//
// Файл перезаписывается целиком, а не дописывается: он описывает одно
// состояние, а не ряд. История копится в почасовых файлах.
func (c *Collector) writeCurrent(now time.Time) {
	// Первый снимок пишется сразу: смысл файла в том, чтобы сразу после
	// запуска было видно, что бот считает.
	if !c.lastCurrent.IsZero() && now.Sub(c.lastCurrent) < currentEvery {
		return
	}
	c.lastCurrent = now

	records := c.snapshot(true)
	if len(records) == 0 {
		return
	}
	if err := c.replace(currentName, records); err != nil {
		c.log.Error("failed to write the current hour snapshot", "error", err)
	}
}

// snapshot превращает накопители в строки отчёта, не трогая накопленное.
func (c *Collector) snapshot(partial bool) []Record {
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Record, 0, len(keys))
	for _, k := range keys {
		if r, ok := c.series[k].record(c.hour, partial); ok {
			out = append(out, r)
		}
	}
	return out
}

// Record — одна строка отчёта: показатель за один час.
type Record struct {
	// Hour — начало часа UTC, к которому относятся числа.
	Hour string `json:"hour"`
	// Metric и Labels однозначно определяют ряд.
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels,omitempty"`
	// Samples — сколько замеров вошло в час.
	Samples int `json:"samples"`

	// Delta заполняется у накопительных счётчиков: сколько событий
	// произошло за час.
	Delta *float64 `json:"delta,omitempty"`

	// Квантили мгновенных величин. Медиана описывает обычное состояние,
	// края — то, ради чего отчёт и читают: заливает нас не в среднем.
	Min *float64 `json:"min,omitempty"`
	P05 *float64 `json:"p05,omitempty"`
	P50 *float64 `json:"p50,omitempty"`
	P95 *float64 `json:"p95,omitempty"`
	Max *float64 `json:"max,omitempty"`

	// Partial отмечает час, оборванный остановкой процесса.
	Partial bool `json:"partial,omitempty"`
}

// flush записывает свёртку накопленного часа и очищает накопители.
func (c *Collector) flush(now time.Time, partial bool) {
	if c.dir == "" || len(c.series) == 0 {
		return
	}

	records := c.snapshot(partial)
	// Счётчики переносятся в следующий час до очистки накопителей.
	for key, acc := range c.series {
		if acc.kind == ports.KindCounter && acc.seen {
			c.carry[key] = acc.last
		}
	}
	c.series = make(map[string]*series)

	if err := c.append(records); err != nil {
		c.log.Error("failed to write hourly statistics", "error", err)
		return
	}
	c.log.Info("hourly statistics written",
		"hour", c.hour.Format(time.RFC3339), "series", len(records), "partial", partial)
	_ = now
}

// record превращает накопитель в строку отчёта.
func (s *series) record(hour time.Time, partial bool) (Record, bool) {
	r := Record{
		Hour:    hour.Format(time.RFC3339),
		Metric:  s.name,
		Labels:  s.labels,
		Partial: partial,
	}

	if s.kind == ports.KindCounter {
		if !s.seen {
			return r, false
		}
		// Счётчик мог обнулиться при перезапуске процесса; отрицательная
		// разность означала бы не отрицательное число событий, а рестарт.
		delta := max(s.last-s.first, 0)
		r.Delta = &delta
		r.Samples = 1
		return r, true
	}

	if len(s.values) == 0 {
		return r, false
	}
	v := append([]float64(nil), s.values...)
	sort.Float64s(v)

	at := func(q float64) *float64 {
		i := min(int(q*float64(len(v))), len(v)-1)
		return &v[i]
	}
	r.Samples = len(v)
	r.Min, r.P05, r.P50, r.P95, r.Max = &v[0], at(0.05), at(0.50), at(0.95), &v[len(v)-1]
	return r, true
}

// append дописывает строки в файл суток.
//
// Файл на сутки, а не один общий: недельный отчёт иначе пришлось бы качать
// целиком ради одного дня, а разрезать его на сервере нечем.
func (c *Collector) append(records []Record) error {
	path := filepath.Join(c.dir, "hourly-"+c.hour.Format("2006-01-02")+".ndjson")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return f.Sync()
}

// replace перезаписывает файл целиком, атомарно.
//
// Через временный файл с переименованием: читатель, забирающий снимок в тот же
// момент, получит либо прежнее состояние, либо новое, но не половину.
func (c *Collector) replace(name string, records []Record) error {
	tmp, err := os.CreateTemp(c.dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			tmp.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return os.Rename(tmp.Name(), filepath.Join(c.dir, name))
}

// seriesKey строит устойчивый ключ ряда: имя плюс метки в отсортированном виде.
func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return name + "{" + strings.Join(parts, ",") + "}"
}

var _ ports.Runnable = (*Collector)(nil)
