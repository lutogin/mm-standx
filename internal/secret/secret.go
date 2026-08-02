// Package secret хранит чувствительные строки так, чтобы они не утекли в логи.
package secret

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

const redacted = "[REDACTED]"

// Secret — строка, которая никогда не печатается.
//
// Реализует Stringer, json.Marshaler и slog.LogValuer, поэтому случайно вывести
// её через %v, json.Marshal или структурный лог невозможно. Получить значение
// можно только явным вызовом Reveal.
type Secret struct{ v string }

func New(v string) Secret { return Secret{v: v} }

// Reveal возвращает исходное значение. Каждый вызов — место, требующее внимания
// при ревью.
func (s Secret) Reveal() string { return s.v }

func (s Secret) IsEmpty() bool { return s.v == "" }

func (s Secret) String() string { return redacted }

func (s Secret) GoString() string { return redacted }

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

func (s *Secret) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("unmarshal secret: %w", err)
	}
	s.v = v
	return nil
}

func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
