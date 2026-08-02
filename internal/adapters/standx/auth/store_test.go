package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sampleSession() *CachedSession {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return &CachedSession{
		Address:     "0x9d8A62f656a8d1615C1294fd71e9CFb3E4855A4F",
		Chain:       "bsc",
		Token:       "header.payload.signature",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		SessionSeed: base64.StdEncoding.EncodeToString(seed),
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "session.json")
	store := NewFileStore(path)
	want := sampleSession()

	require.NoError(t, store.Save(want))

	got, err := store.Load()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, want.Token, got.Token)
	require.Equal(t, want.SessionSeed, got.SessionSeed)
	require.WithinDuration(t, want.ExpiresAt, got.ExpiresAt, time.Second)
}

// Файл содержит действующие торговые учётные данные, поэтому читать его
// вправе только владелец.
func TestStoreWritesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	require.NoError(t, NewFileStore(path).Save(sampleSession()))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// Файл, доступный посторонним, мог быть прочитан кем угодно. Его содержимое
// считается скомпрометированным, и восстанавливаться из него нельзя.
func TestStoreRejectsWorldReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	store := NewFileStore(path)
	require.NoError(t, store.Save(sampleSession()))
	require.NoError(t, os.Chmod(path, 0o644))

	_, err := store.Load()
	require.ErrorContains(t, err, "unsafe permissions")
}

func TestStoreTreatsMissingFileAsEmpty(t *testing.T) {
	got, err := NewFileStore(filepath.Join(t.TempDir(), "absent.json")).Load()
	require.NoError(t, err, "a missing file is the normal first-run situation")
	require.Nil(t, got)
}

func TestStoreReportsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := NewFileStore(path).Load()
	require.Error(t, err)
}

func TestValidRejectsUnusableSessions(t *testing.T) {
	now := time.Now()
	const lead = time.Hour

	cases := map[string]func(*CachedSession){
		"expired":             func(c *CachedSession) { c.ExpiresAt = now.Add(-time.Minute) },
		"expires within lead": func(c *CachedSession) { c.ExpiresAt = now.Add(30 * time.Minute) },
		"missing token":       func(c *CachedSession) { c.Token = "" },
		"missing session key": func(c *CachedSession) { c.SessionSeed = "" },
		"different wallet":    func(c *CachedSession) { c.Address = "0xdeadbeef" },
		"different chain":     func(c *CachedSession) { c.Chain = "solana" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			session := sampleSession()
			mutate(session)
			require.False(t, session.Valid(now, lead, sampleSession().Address, "bsc"))
		})
	}
}

func TestValidAcceptsFreshSession(t *testing.T) {
	session := sampleSession()
	require.True(t, session.Valid(time.Now(), time.Hour, session.Address, "bsc"))

	// Регистр адреса не должен влиять: EIP-55 и нижний регистр обозначают
	// один и тот же кошелёк.
	require.True(t, session.Valid(time.Now(), time.Hour, "0x9d8a62f656a8d1615c1294fd71e9cfb3e4855a4f", "bsc"))
}

func TestValidHandlesNilSession(t *testing.T) {
	var session *CachedSession
	require.False(t, session.Valid(time.Now(), time.Hour, "", ""))
}

func TestSeedDecoding(t *testing.T) {
	seed, err := sampleSession().Seed()
	require.NoError(t, err)
	require.Len(t, seed, ed25519.SeedSize)

	short := sampleSession()
	short.SessionSeed = base64.StdEncoding.EncodeToString([]byte("short"))
	_, err = short.Seed()
	require.ErrorContains(t, err, "seed")
}

// Ключ сессии восстанавливается ровно тем же: иначе сохранённый токен
// обесценился бы, поскольку биржа связала его с конкретным публичным ключом.
func TestRestoredSessionKeepsIdentity(t *testing.T) {
	original := sampleSession()
	seed, err := original.Seed()
	require.NoError(t, err)

	first, err := NewSessionFromSeed(seed, func() int64 { return 0 })
	require.NoError(t, err)
	second, err := NewSessionFromSeed(seed, func() int64 { return 0 })
	require.NoError(t, err)

	require.Equal(t, first.RequestID(), second.RequestID())
	require.Equal(t, first.Seed(), second.Seed())
}

func TestNopStore(t *testing.T) {
	got, err := NopStore{}.Load()
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, NopStore{}.Save(sampleSession()))
}
