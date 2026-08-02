package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// fileMode — права доступа к файлу сессии. Файл содержит действующие учётные
// данные, поэтому читать его вправе только владелец.
const fileMode fs.FileMode = 0o600

// CachedSession — выданный токен вместе с ключом сессии, которым он подписан.
//
// Хранить их порознь бессмысленно: биржа связывает токен с публичным ключом
// ed25519 на этапе prepare-signin, поэтому чужой ключ не подпишет запрос
// под этим токеном.
//
// Смысл кэширования — в разнице ценности ключей. Приватный ключ кошелька нужен
// ровно один раз, при логине; всё остальное подписывает эфемерный ключ сессии.
// Сохранив пару, бот переживает перезапуск без ключа кошелька вообще.
//
// Утечка кэша ограничена: он истекает вместе с токеном и не позволяет вывести
// средства — вывод требует транзакции, подписанной кошельком. Украсть можно
// торговую активность, но не деньги.
type CachedSession struct {
	Address     string    `json:"address"`
	Chain       string    `json:"chain"`
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expires_at"`
	SessionSeed string    `json:"session_seed"`
}

// Valid сообщает, пригоден ли кэш для использования прямо сейчас.
func (c *CachedSession) Valid(now time.Time, lead time.Duration, address, chain string) bool {
	switch {
	case c == nil, c.Token == "", c.SessionSeed == "":
		return false
	case !now.Add(lead).Before(c.ExpiresAt):
		return false
	// Кэш от другого кошелька или другой сети торговал бы не тем счётом.
	case address != "" && !equalFold(c.Address, address):
		return false
	case chain != "" && c.Chain != chain:
		return false
	}
	return true
}

// Seed возвращает зерно ключа сессии.
func (c *CachedSession) Seed() ([]byte, error) {
	seed, err := base64.StdEncoding.DecodeString(c.SessionSeed)
	if err != nil {
		return nil, fmt.Errorf("session store: decode seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("session store: want %d bytes of seed, got %d", ed25519.SeedSize, len(seed))
	}
	return seed, nil
}

// SessionStore сохраняет и восстанавливает сессию между запусками.
type SessionStore interface {
	Load() (*CachedSession, error)
	Save(*CachedSession) error
}

// FileStore хранит сессию в файле с правами 0600.
type FileStore struct{ path string }

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Load читает сессию. Отсутствие файла ошибкой не считается.
func (s *FileStore) Load() (*CachedSession, error) {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}
	// Файл с расширенными правами мог быть прочитан кем угодно, поэтому его
	// содержимое считаем скомпрометированным и просто игнорируем.
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("session store: file %s has unsafe permissions %04o, want 0600",
			s.path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("session store: %w", err)
	}
	var cached CachedSession
	if err := json.Unmarshal(raw, &cached); err != nil {
		return nil, fmt.Errorf("session store: parse %s: %w", s.path, err)
	}
	return &cached, nil
}

// Save записывает сессию атомарно, чтобы прерванная запись не оставила
// файл, из которого потом нельзя восстановиться.
func (s *FileStore) Save(session *CachedSession) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("session store: create directory: %w", err)
	}
	raw, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("session store: marshal: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, fileMode); err != nil {
		return fmt.Errorf("session store: write: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("session store: rename: %w", err)
	}
	return nil
}

// NopStore отключает сохранение сессии.
type NopStore struct{}

func (NopStore) Load() (*CachedSession, error) { return nil, nil }
func (NopStore) Save(*CachedSession) error     { return nil }

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

var (
	_ SessionStore = (*FileStore)(nil)
	_ SessionStore = NopStore{}
)
