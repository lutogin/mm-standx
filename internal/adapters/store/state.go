// Package store сохраняет снимки состояния на диск.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nd/mm-standx/internal/ports"
)

// FileStore хранит снимки отдельными файлами JSON в одном каталоге.
type FileStore struct {
	dir string
}

// NewFile создаёт хранилище, создавая каталог при необходимости.
func NewFile(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", dir, err)
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}

func (s *FileStore) Load(name string, into any) (bool, error) {
	data, err := os.ReadFile(s.path(name))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read state %s: %w", name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return false, fmt.Errorf("decode state %s: %w", name, err)
	}
	return true, nil
}

// Save пишет во временный файл и переименовывает его поверх целевого.
//
// Переименование в пределах файловой системы атомарно, поэтому смерть процесса
// в любой момент оставляет на диске целый снимок — прежний или новый. Запись
// поверх существующего файла такой гарантии не даёт, а порча снимка защитного
// контура означала бы потерю счётчика убытка ровно тогда, когда он нужен.
func (s *FileStore) Save(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state %s: %w", name, err)
	}

	target := s.path(name)
	tmp, err := os.CreateTemp(s.dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file for %s: %w", name, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state %s: %w", name, err)
	}
	// Синхронизация до переименования: без неё переименование может стать
	// видимым раньше данных, и после отключения питания на месте снимка
	// окажется файл нулевой длины.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("commit state %s: %w", name, err)
	}
	return nil
}

var _ ports.StateStore = (*FileStore)(nil)
