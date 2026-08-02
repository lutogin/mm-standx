// Package auth реализует двухключевую схему доступа StandX.
//
// Ключ кошелька доказывает личность один раз при получении JWT. Эфемерный
// ed25519-ключ сессии подписывает каждый торговый запрос — приватный ключ кошелька
// в горячем пути не участвует.
package auth

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/nd/mm-standx/internal/secret"
)

// eip191Prefix — префикс personal_sign из EIP-191. Он не даёт использовать подпись
// логина как подпись транзакции.
const eip191Prefix = "\x19Ethereum Signed Message:\n"

// Address — адрес EVM-кошелька в шестнадцатеричном виде с префиксом 0x.
type Address string

func (a Address) String() string { return string(a) }

// Wallet подписывает сообщения ключом EVM-кошелька по схеме personal_sign.
type Wallet struct {
	priv    *secp256k1.PrivateKey
	address Address
}

// NewWallet разбирает приватный ключ из шестнадцатеричной строки
// (с префиксом 0x или без) и вычисляет соответствующий адрес.
func NewWallet(privateKey secret.Secret) (*Wallet, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(privateKey.Reveal()), "0x")
	if raw == "" {
		return nil, fmt.Errorf("wallet: приватный ключ не задан")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("wallet: приватный ключ не является hex-строкой: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("wallet: ожидалось 32 байта приватного ключа, получено %d", len(b))
	}

	priv := secp256k1.PrivKeyFromBytes(b)
	return &Wallet{priv: priv, address: addressFromPubKey(priv.PubKey())}, nil
}

// Address возвращает адрес кошелька.
func (w *Wallet) Address() Address { return w.address }

// SignPersonal подписывает сообщение по схеме EIP-191 personal_sign и возвращает
// 65-байтовую подпись в формате r‖s‖v, где v равно 27 или 28.
//
// Именно этот формат ожидает StandX: он совпадает с результатом
// ethers.Wallet.signMessage из примеров документации.
func (w *Wallet) SignPersonal(message string) (string, error) {
	hash := personalHash(message)

	// SignCompact отдаёт recoveryCode‖R‖S, где recoveryCode = 27 + recid
	// для несжатого ключа. Ethereum ожидает обратный порядок: R‖S‖v.
	compact := ecdsa.SignCompact(w.priv, hash, false)
	if len(compact) != 65 {
		return "", fmt.Errorf("wallet: неожиданная длина подписи %d", len(compact))
	}

	var sig [65]byte
	copy(sig[:64], compact[1:])
	sig[64] = compact[0]

	return "0x" + hex.EncodeToString(sig[:]), nil
}

// RecoverPersonal восстанавливает адрес подписанта из сообщения и подписи.
//
// Повторяет проверку, которую делает сервер, и используется в тестах, чтобы
// убедиться в корректности формата подписи без хрупких захардкоженных констант.
func RecoverPersonal(message, signature string) (Address, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(signature, "0x"))
	if err != nil {
		return "", fmt.Errorf("recover: подпись не является hex-строкой: %w", err)
	}
	if len(raw) != 65 {
		return "", fmt.Errorf("recover: ожидалось 65 байт подписи, получено %d", len(raw))
	}

	// Обратное преобразование R‖S‖v → recoveryCode‖R‖S.
	compact := make([]byte, 65)
	compact[0] = raw[64]
	copy(compact[1:], raw[:64])

	pub, _, err := ecdsa.RecoverCompact(compact, personalHash(message))
	if err != nil {
		return "", fmt.Errorf("recover: %w", err)
	}
	return addressFromPubKey(pub), nil
}

// personalHash строит хеш EIP-191: keccak256("\x19Ethereum Signed Message:\n" + len + message).
func personalHash(message string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(eip191Prefix))
	h.Write([]byte(strconv.Itoa(len(message))))
	h.Write([]byte(message))
	return h.Sum(nil)
}

// addressFromPubKey берёт последние 20 байт keccak256 от несжатого публичного
// ключа без ведущего байта 0x04.
func addressFromPubKey(pub *secp256k1.PublicKey) Address {
	uncompressed := pub.SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:])
	return Address("0x" + hex.EncodeToString(h.Sum(nil)[12:]))
}
