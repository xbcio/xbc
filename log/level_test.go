package log

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireCurrentLevel pins the state SetLevel left behind. CurrentLevel reads
// package state shared by every test in this package, so each test that moves
// the level restores it at the end; the assertion here is one half of that
// contract.
func requireCurrentLevel(t *testing.T, want Level) {
	t.Helper()
	assert.Equal(t, want, CurrentLevel(), "level state must reflect the last SetLevel call")
}

// fileOnlyInits this package's default backend onto a per-test file sink and
// returns the file path. The observer core used elsewhere in this package's
// tests cannot stand in here: it is a separately built core that does not read
// the default backend's shared level, so it would never show what SetLevel
// does. Testing through the real backend is the whole point.
func fileOnlyInits(t *testing.T, level string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Level = level
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })
	return cfg.File.Path
}

// logFile reads back what the sink actually holds: Close flushes and closes
// it, so every write made before this call is complete on disk.
func logFile(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, Close())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// The reloader's whole reason to exist: a logger handed out before the reload
// keeps working after it, and observes the new level without being rebuilt.
func TestSetLevelIsObservedByAnAlreadyHandedOutLogger(t *testing.T) {
	path := fileOnlyInits(t, "info")
	defer SetLevel("info")

	held := L() // what an embedding application would have captured earlier

	got, err := SetLevel("error")
	require.NoError(t, err)
	assert.Equal(t, ErrorLevel, got)

	held.Debug("dropped debug")
	held.Info("dropped info")
	held.Error("kept error")

	data := logFile(t, path)
	assert.Contains(t, data, "kept error")
	assert.NotContains(t, data, "dropped info", "the captured logger must observe the new level")
	assert.NotContains(t, data, "dropped debug")
}

// Init is the composition root's one-shot path -- it replaces the backend and
// decides the level from the config, so the level survives it.
func TestInitAppliesConfiguredLevel(t *testing.T) {
	path := fileOnlyInits(t, "warn")
	defer SetLevel("info")

	L().Info("dropped")
	L().Warn("kept")

	data := logFile(t, path)
	assert.Contains(t, data, "kept")
	assert.NotContains(t, data, "dropped")
	requireCurrentLevel(t, WarnLevel)
}

// SetLevel is the reload path because Init cannot be: Init closes the previous
// file sink, so any logger still holding it would write into a closed file.
// This is the regression that keeps SetLevel from rotting back into a call to
// Init.
func TestSetLevelKeepsWritingThroughTheFileSink(t *testing.T) {
	path := fileOnlyInits(t, "info")
	defer SetLevel("info")

	held := L()

	got, err := SetLevel("error")
	require.NoError(t, err)
	assert.Equal(t, ErrorLevel, got)

	held.Debug("dropped debug")
	held.Error("kept error")

	data := logFile(t, path)
	assert.Contains(t, data, "kept error")
	assert.NotContains(t, data, "dropped debug",
		"the sink must still be open and filtered at the new level, not closed by the reload")
}

func TestSetLevelRejectsUnknownLevelWithoutMovingTheState(t *testing.T) {
	_, err := SetLevel("info")
	require.NoError(t, err)
	defer SetLevel("info")

	got, err := SetLevel("verbose")

	require.Error(t, err, "a misspelled level must error, never silently degrade")
	assert.Equal(t, InfoLevel, got, "the parsed zero value travels with the error")
	requireCurrentLevel(t, InfoLevel)
}

// SetLevel before Init, or after Close, must not be lost: the next Init reads
// the config, so what matters is that the facade reports the level instead of
// failing on a missing backend.
func TestSetLevelWithoutALiveBackendStillRecordsTheLevel(t *testing.T) {
	SetLogger(Nop())
	defer SetLevel("info")

	got, err := SetLevel("debug")

	require.NoError(t, err)
	assert.Equal(t, DebugLevel, got)
	requireCurrentLevel(t, DebugLevel)
}

func TestSetLevelAcceptsEverySpellingParseLevelDoes(t *testing.T) {
	defer SetLevel("info")
	cases := map[string]Level{
		"debug":   DebugLevel,
		"WARN":    WarnLevel,
		" error ": ErrorLevel,
	}
	for in, want := range cases {
		got, err := SetLevel(in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q", in)
		requireCurrentLevel(t, want)
	}
}
