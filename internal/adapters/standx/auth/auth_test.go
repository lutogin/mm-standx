package auth

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/secret"
)

// Известный тест-вектор из EIP-155: этой паре ключ→адрес доверяет вся экосистема,
// поэтому она годится как якорь для проверки вывода адреса.
const (
	knownPrivateKey = "4646464646464646464646464646464646464646464646464646464646464646"
	knownAddress    = "0x9d8a62f656a8d1615c1294fd71e9cfb3e4855a4f"
)

func TestWalletDerivesKnownAddress(t *testing.T) {
	for _, key := range []string{knownPrivateKey, "0x" + knownPrivateKey} {
		w, err := NewWallet(secret.New(key))
		require.NoError(t, err)
		require.Equal(t, knownAddress, strings.ToLower(w.Address().String()))
	}
}

func TestWalletRejectsMalformedKeys(t *testing.T) {
	cases := map[string]string{
		"пустой":           "",
		"не hex":           "0xZZZZ",
		"слишком короткий": "0x4646",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewWallet(secret.New(key))
			require.Error(t, err)
		})
	}
}

// Подпись personal_sign проверяется восстановлением адреса — ровно так, как это
// делает сервер. Захардкоженная константа подписи проверяла бы лишь саму себя.
func TestSignPersonalRecoversToSigner(t *testing.T) {
	w, err := NewWallet(secret.New(knownPrivateKey))
	require.NoError(t, err)

	messages := []string{
		"",
		"hello",
		"standx.com wants you to sign in with your Ethereum account:\n0x9d8A...",
		strings.Repeat("длинное сообщение с юникодом ", 40),
	}

	for _, msg := range messages {
		sig, err := w.SignPersonal(msg)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(sig, "0x"))
		// 65 байт: r(32) + s(32) + v(1)
		require.Len(t, sig, 2+130)

		raw, err := hex.DecodeString(strings.TrimPrefix(sig, "0x"))
		require.NoError(t, err)
		require.Contains(t, []byte{27, 28}, raw[64], "v должно быть 27 или 28")

		recovered, err := RecoverPersonal(msg, sig)
		require.NoError(t, err)
		require.Equal(t, strings.ToLower(w.Address().String()), strings.ToLower(recovered.String()))
	}
}

func TestSignPersonalIsDeterministic(t *testing.T) {
	w, err := NewWallet(secret.New(knownPrivateKey))
	require.NoError(t, err)

	first, err := w.SignPersonal("standx")
	require.NoError(t, err)
	second, err := w.SignPersonal("standx")
	require.NoError(t, err)

	// RFC 6979: подпись детерминирована, что делает её воспроизводимой в тестах.
	require.Equal(t, first, second)
}

func TestRecoverPersonalRejectsTamperedMessage(t *testing.T) {
	w, err := NewWallet(secret.New(knownPrivateKey))
	require.NoError(t, err)

	sig, err := w.SignPersonal("оригинал")
	require.NoError(t, err)

	recovered, err := RecoverPersonal("подмена", sig)
	// Восстановление математически возможно, но даёт чужой адрес.
	if err == nil {
		require.NotEqual(t, strings.ToLower(w.Address().String()), strings.ToLower(recovered.String()))
	}
}

func TestSessionSignsAndVerifies(t *testing.T) {
	const fixedNow = int64(1_754_897_028_000)
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}

	s, err := NewSessionFromSeed(seed, func() int64 { return fixedNow })
	require.NoError(t, err)
	require.NotEmpty(t, s.RequestID())

	body := []byte(`{"symbol":"BTC-USD","side":"buy","qty":"0.1"}`)
	headers, err := s.Sign(body)
	require.NoError(t, err)

	require.Equal(t, "v1", headers.Version)
	require.Equal(t, fixedNow, headers.Timestamp)
	require.Len(t, headers.RequestID, 36, "x-request-id должен быть UUID")
	require.True(t, VerifySignature(s.PublicKey(), headers, body))

	// Подпись обязана ломаться при изменении любого компонента сообщения.
	require.False(t, VerifySignature(s.PublicKey(), headers, []byte(`{"symbol":"ETH-USD"}`)))

	wrongTimestamp := headers
	wrongTimestamp.Timestamp = fixedNow + 1
	require.False(t, VerifySignature(s.PublicKey(), wrongTimestamp, body))

	wrongID := headers
	wrongID.RequestID = "00000000-0000-4000-8000-000000000000"
	require.False(t, VerifySignature(s.PublicKey(), wrongID, body))
}

func TestSignPayloadFormat(t *testing.T) {
	got := signPayload("v1", "abc", 1700000000123, []byte(`{"a":1}`))
	require.Equal(t, `v1,abc,1700000000123,{"a":1}`, string(got))

	require.Equal(t, "v1,id,0,x", string(signPayload("v1", "id", 0, []byte("x"))))
	require.Equal(t, "v1,id,-5,x", string(signPayload("v1", "id", -5, []byte("x"))))
}

func TestNewUUIDv4Format(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id, err := newUUIDv4()
		require.NoError(t, err)
		require.Len(t, id, 36)
		require.Equal(t, byte('4'), id[14], "версия UUID должна быть 4")
		require.Contains(t, "89ab", string(id[19]), "вариант должен быть RFC 4122")
		require.False(t, seen[id], "UUID должны быть уникальны")
		seen[id] = true
	}
}
