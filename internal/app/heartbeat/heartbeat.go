// Package heartbeat публикует признак жизни процесса на диск.
//
// Файл читает сторожевой процесс (cmd/guardian). Смысл в том, что у StandX нет
// отмены ордеров по разрыву соединения: если бот умрёт, его котировки останутся
// в стакане и продолжат исполняться, а следить за ними будет некому.
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nd/mm-standx/internal/domain/market"
	"github.com/nd/mm-standx/internal/ports"
)

// State — содержимое файла биения.
type State struct {
	// At — момент последней записи. Сторож судит о жизни именно по нему.
	At time.Time `json:"at"`
	// PID — идентификатор процесса, для диагностики.
	PID int `json:"pid"`
	// Symbols — инструменты, на которых бот котирует. Сторож снимает ордера
	// именно по ним, а не по всем подряд.
	Symbols []string `json:"symbols"`
	// Live сообщает, отправлял ли процесс настоящие ордера.
	Live bool `json:"live"`
}

// Writer периодически обновляет файл биения.
type Writer struct {
	path     string
	interval time.Duration
	symbols  []market.Symbol
	live     bool
	clock    ports.Clock
	log      *slog.Logger
	// actors отвечает, тикают ли акторы.
	//
	// Без него запись означала только «горутина жива», а сторож принимал это
	// за «торговля идёт». Разница стоила тридцати пяти минут простоя на живом
	// счёте 13 августа при контейнере в состоянии healthy.
	actors ports.LivenessSource
	// stalled помнит, что запись уже остановлена: жалоба нужна одна на эпизод,
	// а такт здесь секундный.
	stalled bool

	cancel context.CancelFunc
	done   chan struct{}
}

func New(
	path string,
	interval time.Duration,
	symbols []market.Symbol,
	live bool,
	actors ports.LivenessSource,
	clock ports.Clock,
	log *slog.Logger,
) *Writer {
	return &Writer{
		path:     path,
		interval: interval,
		symbols:  symbols,
		live:     live,
		actors:   actors,
		clock:    clock,
		log:      log.With("component", "heartbeat"),
		done:     make(chan struct{}),
	}
}

func (w *Writer) Name() string { return "heartbeat" }

func (w *Writer) Start(context.Context) error {
	if w.path == "" {
		close(w.done)
		return nil
	}
	if err := w.write(); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	go func() {
		defer close(w.done)
		w.loop(ctx)
	}()
	return nil
}

// Stop удаляет файл.
//
// Корректное завершение снимает котировки самостоятельно, поэтому оставленный
// файл только сбивал бы сторожа с толку: он не должен отличать «умер» от
// «штатно вышел» по возрасту записи.
func (w *Writer) Stop(ctx context.Context) error {
	if w.cancel != nil {
		w.cancel()
		select {
		case <-w.done:
		case <-ctx.Done():
			return fmt.Errorf("heartbeat: did not stop in time: %w", ctx.Err())
		}
	}
	if w.path == "" {
		return nil
	}
	if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
		w.log.Warn("failed to remove heartbeat file", "path", w.path, "error", err)
	}
	return nil
}

func (w *Writer) loop(ctx context.Context) {
	ticker := w.clock.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := w.write(); err != nil {
				w.log.Error("failed to write heartbeat", "error", err)
			}
		}
	}
}

// write обновляет файл, если акторы тикают.
//
// Молчащий актор останавливает запись, а не пишет «мне плохо»: сторож судит
// по возрасту отметки, и любой формат жалобы, кроме её отсутствия, требовал бы
// от него понимать новое поле. Замерший файл он трактует единственным нужным
// способом — снимает котировки, которые больше некому вести.
//
// Признак жизни при этом умеет и восстанавливаться: актор, вернувшийся к работе
// после паузы, снова двигает отметку, и запись возобновляется. Останавливающая
// защёлка здесь была бы тем же дефектом, который эта серия правок и закрывает.
func (w *Writer) write() error {
	if w.actors != nil {
		if symbol, silent, stale := w.actors.Stalest(w.clock.Now()); stale {
			if !w.stalled {
				w.stalled = true
				w.log.Error("actor stopped ticking, heartbeat is frozen",
					"symbol", symbol,
					"silent", silent.Truncate(time.Millisecond).String(),
					"effect", "the guardian will withdraw quotes once the file goes stale")
			}
			return nil
		}
		if w.stalled {
			w.stalled = false
			w.log.Warn("actors are ticking again, heartbeat resumed")
		}
	}

	symbols := make([]string, 0, len(w.symbols))
	for _, s := range w.symbols {
		symbols = append(symbols, s.String())
	}

	raw, err := json.Marshal(State{
		At:      w.clock.Now(),
		PID:     os.Getpid(),
		Symbols: symbols,
		Live:    w.live,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Запись атомарна: сторож не должен прочитать наполовину обновлённый файл
	// и решить по обрывку, что процесс умер.
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, w.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// Read загружает состояние из файла. Используется сторожевым процессом.
func Read(path string) (State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return state, nil
}

var _ ports.Runnable = (*Writer)(nil)
