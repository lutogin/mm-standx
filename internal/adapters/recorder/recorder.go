// Package recorder сохраняет сырые сообщения потоков на диск.
//
// Записи — топливо для интеграционных тестов: fake-биржа воспроизводит настоящую
// траекторию mark price вместо синтетической, поэтому измеренные аптайм и частота
// заливов соответствуют реальному рынку.
package recorder

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nd/mm-standx/internal/ports"
)

// flushEvery — через сколько записей сбрасывать буфер на диск.
// Потеря последних сообщений при аварии некритична, а посимвольная запись
// на потоке в сотни сообщений в секунду обходится дорого.
const flushEvery = 500

// Entry — одна запись журнала.
type Entry struct {
	Stream   string          `json:"stream"`
	Received int64           `json:"ts_us"` // микросекунды unix, локальное время получения
	Payload  json.RawMessage `json:"payload"`
}

// FileRecorder пишет NDJSON с почасовой ротацией файлов.
//
// Почасовая нарезка выбрана не случайно: программа наград считает аптайм внутри
// календарного часа, поэтому файл записи совпадает с единицей расчёта.
type FileRecorder struct {
	dir   string
	clock ports.Clock

	mu         sync.Mutex
	file       *os.File
	writer     *bufio.Writer
	hour       time.Time
	sinceFlush int
}

// NewFile создаёт рекордер, пишущий в указанный каталог.
func NewFile(dir string, clock ports.Clock) (*FileRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: create directory %s: %w", dir, err)
	}
	return &FileRecorder{dir: dir, clock: clock}, nil
}

// Record сохраняет одно сообщение.
func (r *FileRecorder) Record(stream string, received time.Time, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.rotate(received); err != nil {
		return err
	}

	entry := Entry{
		Stream:   stream,
		Received: received.UnixMicro(),
		Payload:  json.RawMessage(payload),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("recorder: marshal entry: %w", err)
	}
	if _, err := r.writer.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("recorder: write: %w", err)
	}

	r.sinceFlush++
	if r.sinceFlush >= flushEvery {
		r.sinceFlush = 0
		return r.writer.Flush()
	}
	return nil
}

// rotate открывает новый файл при переходе через границу часа.
func (r *FileRecorder) rotate(at time.Time) error {
	hour := at.UTC().Truncate(time.Hour)
	if r.file != nil && hour.Equal(r.hour) {
		return nil
	}
	if err := r.closeCurrent(); err != nil {
		return err
	}

	name := filepath.Join(r.dir, hour.Format("2006-01-02T15")+".ndjson")
	file, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("recorder: open %s: %w", name, err)
	}
	r.file, r.writer, r.hour, r.sinceFlush = file, bufio.NewWriterSize(file, 64<<10), hour, 0
	return nil
}

// Close сбрасывает буфер и закрывает текущий файл.
func (r *FileRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeCurrent()
}

func (r *FileRecorder) closeCurrent() error {
	if r.file == nil {
		return nil
	}
	flushErr := r.writer.Flush()
	closeErr := r.file.Close()
	r.file, r.writer = nil, nil

	if flushErr != nil {
		return fmt.Errorf("recorder: flush buffer: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("recorder: close file: %w", closeErr)
	}
	return nil
}

// Nop — рекордер, который ничего не пишет. Используется, когда запись выключена.
type Nop struct{}

func (Nop) Record(string, time.Time, []byte) error { return nil }
func (Nop) Close() error                           { return nil }

var _ ports.Recorder = (*FileRecorder)(nil)
var _ ports.Recorder = Nop{}
