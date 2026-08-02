package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/mr-tron/base58"

	"github.com/nd/mm-standx/internal/ports"
)

// signVersion — версия схемы подписи тела запроса.
const signVersion = "v1"

// Session — эфемерная ключевая пара ed25519, которой подписывается каждый
// торговый запрос.
//
// Ключ живёт только в памяти процесса и создаётся заново при каждом запуске:
// он привязывается к JWT на этапе prepare-signin, поэтому его компрометация
// ограничена временем жизни токена.
type Session struct {
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	requestID string
	now       func() int64
}

// NewSession генерирует новую пару ключей сессии.
//
// nowMillis подставляется извне, чтобы подпись была воспроизводима в тестах.
func NewSession(nowMillis func() int64) (*Session, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("session: генерация ключа ed25519: %w", err)
	}
	return &Session{
		priv:      priv,
		pub:       pub,
		requestID: base58.Encode(pub),
		now:       nowMillis,
	}, nil
}

// NewSessionFromSeed восстанавливает сессию из фиксированного зерна.
// Предназначена исключительно для тестов с воспроизводимыми подписями.
func NewSessionFromSeed(seed []byte, nowMillis func() int64) (*Session, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("session: ожидалось %d байт зерна, получено %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Session{priv: priv, pub: pub, requestID: base58.Encode(pub), now: nowMillis}, nil
}

// RequestID возвращает base58 публичного ключа сессии.
//
// Это значение уходит в prepare-signin как поле requestId и не имеет отношения
// к заголовку x-request-id, который генерируется на каждый запрос отдельно.
func (s *Session) RequestID() string { return s.requestID }

// PublicKey возвращает публичный ключ сессии.
func (s *Session) PublicKey() ed25519.PublicKey { return s.pub }

// Sign подписывает тело запроса и возвращает заголовки подписи.
//
// Подписывается ровно тот байтовый срез, который уйдёт в сеть: повторная
// сериализация JSON может дать другой порядок полей и сломать проверку на сервере.
func (s *Session) Sign(body []byte) (ports.SignatureHeaders, error) {
	requestID, err := newUUIDv4()
	if err != nil {
		return ports.SignatureHeaders{}, fmt.Errorf("session: генерация x-request-id: %w", err)
	}
	timestamp := s.now()

	sig := ed25519.Sign(s.priv, signPayload(signVersion, requestID, timestamp, body))

	return ports.SignatureHeaders{
		Version:   signVersion,
		RequestID: requestID,
		Timestamp: timestamp,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// signPayload собирает подписываемое сообщение "{version},{id},{timestamp},{body}".
func signPayload(version, requestID string, timestamp int64, body []byte) []byte {
	msg := make([]byte, 0, len(version)+len(requestID)+len(body)+32)
	msg = append(msg, version...)
	msg = append(msg, ',')
	msg = append(msg, requestID...)
	msg = append(msg, ',')
	msg = appendInt(msg, timestamp)
	msg = append(msg, ',')
	msg = append(msg, body...)
	return msg
}

func appendInt(dst []byte, v int64) []byte {
	if v == 0 {
		return append(dst, '0')
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return append(dst, buf[i:]...)
}

// VerifySignature проверяет подпись тела запроса. Используется в fake-бирже,
// чтобы интеграционные тесты действительно прогоняли путь подписи, а не обходили его.
func VerifySignature(pub ed25519.PublicKey, h ports.SignatureHeaders, body []byte) bool {
	sig, err := base64.StdEncoding.DecodeString(h.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, signPayload(h.Version, h.RequestID, h.Timestamp, body), sig)
}

// NewRequestID генерирует UUID версии 4. Используется для session_id, который
// обязан совпадать у REST-клиента и ws-клиента, иначе результаты матчинга
// не придут ни в один из них.
func NewRequestID() (string, error) { return newUUIDv4() }

// newUUIDv4 генерирует случайный UUID версии 4 без внешних зависимостей.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122

	const hexDigits = "0123456789abcdef"
	out := make([]byte, 36)
	for i, j := 0, 0; i < 16; i++ {
		if j == 8 || j == 13 || j == 18 || j == 23 {
			out[j] = '-'
			j++
		}
		out[j] = hexDigits[b[i]>>4]
		out[j+1] = hexDigits[b[i]&0x0f]
		j += 2
	}
	return string(out), nil
}
