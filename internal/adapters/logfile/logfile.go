// Package logfile keeps a bounded copy of the process log on disk.
//
// The container already rotates stdout through the json-file driver, but those
// files live under /var/lib/docker, are owned by root and are named after the
// container id. Reading them means knowing that id and holding root on the
// host — precisely what one does not want to be assembling while a live run is
// going wrong. The copy written here sits next to docker-compose.yml, survives
// `docker compose down`, and is fetched with plain scp.
//
// Bounded, because an unbounded log on a trading box is a disk-full incident
// waiting for the least convenient moment: the process that dies is the one
// holding orders in the market.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Writer appends to a file, rotating it once it outgrows a size limit.
//
// Writes are unbuffered on purpose. A buffered logger loses exactly the lines
// that explain a crash, which are the only lines anyone reads a crash log for.
// The cost is one syscall per record, and at the volume this process produces
// — a status line every fifteen seconds plus events — that is not a cost.
type Writer struct {
	path     string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	file *os.File
	size int64
}

// New opens path for appending, creating the parent directory if needed.
//
// maxSizeMB bounds one file; keep is how many rotated files are retained
// besides the live one. Total footprint is therefore maxSizeMB*(keep+1).
func New(path string, maxSizeMB, keep int) (*Writer, error) {
	if maxSizeMB <= 0 {
		return nil, fmt.Errorf("logfile: max size must be positive, got %d", maxSizeMB)
	}
	if keep < 0 {
		return nil, fmt.Errorf("logfile: keep must not be negative, got %d", keep)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("logfile: create directory %s: %w", dir, err)
		}
	}

	w := &Writer{path: path, maxBytes: int64(maxSizeMB) << 20, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// Write appends one record, rotating first if it would not fit.
//
// A failed rotation is not a reason to drop the record: an oversized file is a
// nuisance, a missing line is the thing that was being looked for. The write is
// attempted against whatever file is currently open, and the rotation error is
// returned only if the write itself also fails.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("logfile: write %s: %w", w.path, err)
	}
	return n, nil
}

// Close releases the file. Nothing is buffered, so this loses no records.
//
// The process deliberately does not call it on shutdown: withdrawing quotes and
// closing the position happen after the signal, and that is the stretch whose
// log matters most. The method exists for tests and for callers that own the
// writer's lifetime themselves.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeCurrent()
}

func (w *Writer) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logfile: open %s: %w", w.path, err)
	}
	// Appending to a file left by the previous run is intended: a restart is
	// the most interesting moment in the log, and starting a fresh file there
	// would put the two halves of one story in two places. Size is read from
	// the file so the limit accounts for what is already on disk.
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("logfile: stat %s: %w", w.path, err)
	}
	w.file, w.size = file, info.Size()
	return nil
}

// rotate shifts the numbered backups up by one and starts an empty file.
//
// Shifting runs from the oldest down so that no rename overwrites a file that
// has not been moved yet. The last backup is dropped by being renamed over.
func (w *Writer) rotate() error {
	if err := w.closeCurrent(); err != nil {
		return err
	}
	if w.keep == 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logfile: remove %s: %w", w.path, err)
		}
		return w.open()
	}

	for i := w.keep; i > 0; i-- {
		older := fmt.Sprintf("%s.%d", w.path, i)
		newer := w.path
		if i > 1 {
			newer = fmt.Sprintf("%s.%d", w.path, i-1)
		}
		if err := os.Rename(newer, older); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logfile: rotate %s: %w", newer, err)
		}
	}
	return w.open()
}

func (w *Writer) closeCurrent() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file, w.size = nil, 0
	if err != nil {
		return fmt.Errorf("logfile: close %s: %w", w.path, err)
	}
	return nil
}
