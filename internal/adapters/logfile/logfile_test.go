package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// oneMB is the smallest limit New accepts, which makes it the unit every test
// here sizes its records against.
const oneMB = 1 << 20

func record(n int) []byte { return append([]byte(strings.Repeat("x", n-1)), '\n') }

func TestWriterRotatesOnceTheLimitIsReached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmbot.log")
	w, err := New(path, 1, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	// Two records of two thirds of a megabyte cannot share a file.
	big := record(2 * oneMB / 3)
	_, err = w.Write(big)
	require.NoError(t, err)
	_, err = w.Write(big)
	require.NoError(t, err)

	live, err := os.Stat(path)
	require.NoError(t, err)
	require.EqualValues(t, len(big), live.Size(), "the live file holds only the second record")

	rotated, err := os.Stat(path + ".1")
	require.NoError(t, err)
	require.EqualValues(t, len(big), rotated.Size(), "the first record moved to the backup")
}

func TestWriterKeepsOnlyTheConfiguredNumberOfBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmbot.log")
	const keep = 2
	w, err := New(path, 1, keep)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	// Every record fills a file on its own, so each write past the first
	// rotates: five writes are four rotations.
	for i := range 5 {
		_, err := w.Write(record(oneMB))
		require.NoErrorf(t, err, "write %d", i)
	}

	for i := 1; i <= keep; i++ {
		_, err := os.Stat(fmt.Sprintf("%s.%d", path, i))
		require.NoErrorf(t, err, "backup %d must exist", i)
	}
	_, err = os.Stat(fmt.Sprintf("%s.%d", path, keep+1))
	require.True(t, os.IsNotExist(err), "nothing is kept past the limit")
}

func TestWriterDropsEverythingWhenNoBackupsAreKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmbot.log")
	w, err := New(path, 1, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	_, err = w.Write(record(oneMB))
	require.NoError(t, err)
	_, err = w.Write([]byte("after the wrap\n"))
	require.NoError(t, err)

	_, err = os.Stat(path + ".1")
	require.True(t, os.IsNotExist(err), "keep=0 leaves no backups")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "after the wrap\n", string(content))
}

// A restart must not start a new file: the two halves of one incident belong
// together, and the size already on disk still counts against the limit.
func TestWriterAppendsToTheFileLeftByThePreviousRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mmbot.log")
	require.NoError(t, os.WriteFile(path, []byte("previous run\n"), 0o644))

	w, err := New(path, 1, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	_, err = w.Write([]byte("this run\n"))
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "previous run\nthis run\n", string(content))
}

func TestNewRejectsMeaninglessBounds(t *testing.T) {
	dir := t.TempDir()

	_, err := New(filepath.Join(dir, "a.log"), 0, 1)
	require.ErrorContains(t, err, "max size")

	_, err = New(filepath.Join(dir, "b.log"), 1, -1)
	require.ErrorContains(t, err, "keep")
}

func TestNewCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "mmbot.log")
	w, err := New(path, 1, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	_, err = os.Stat(path)
	require.NoError(t, err)
}
