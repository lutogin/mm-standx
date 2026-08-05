package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nd/mm-standx/internal/adapters/store"
)

type snapshot struct {
	Loss    float64 `json:"loss"`
	Tripped bool    `json:"tripped"`
}

func TestRoundTrip(t *testing.T) {
	s, err := store.NewFile(filepath.Join(t.TempDir(), "state"))
	require.NoError(t, err)

	var absent snapshot
	found, err := s.Load("risk", &absent)
	require.NoError(t, err)
	require.False(t, found, "a first run is not an error")

	require.NoError(t, s.Save("risk", snapshot{Loss: 12.5, Tripped: true}))

	var restored snapshot
	found, err = s.Load("risk", &restored)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, snapshot{Loss: 12.5, Tripped: true}, restored)
}

// Overwriting must not leave a half-written file behind: the snapshot has to be
// readable after a kill -9 at any moment, since that is the case it exists for.
func TestSaveLeavesNoPartialFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.NewFile(dir)
	require.NoError(t, err)

	for i := range 20 {
		require.NoError(t, s.Save("risk", snapshot{Loss: float64(i)}))
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "temporary files must not accumulate")
	require.Equal(t, "risk.json", entries[0].Name())

	var restored snapshot
	_, err = s.Load("risk", &restored)
	require.NoError(t, err)
	require.Equal(t, 19.0, restored.Loss)
}

// A corrupt snapshot is reported rather than silently treated as absent:
// zeroing the loss counter because a file failed to parse is exactly the
// outcome the store exists to prevent.
func TestCorruptSnapshotIsAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.NewFile(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "risk.json"), []byte("{ broken"), 0o600))

	var restored snapshot
	found, err := s.Load("risk", &restored)
	require.Error(t, err)
	require.False(t, found)
}
