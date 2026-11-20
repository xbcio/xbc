//go:build cgo

package gormx

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormlogger "gorm.io/gorm/logger"
)

// TestOpenUsesTheInjectedLogger proves the code-only Logger field reaches GORM:
// the same value lands in db.Config and receives the statements Open's caller
// executes. It is cgo-only because it needs the SQLite driver to open a real
// database; without cgo the driver is a stub.
func TestOpenUsesTheInjectedLogger(t *testing.T) {
	logger := &recordingLogger{}
	cfg := sqliteConfig(t)
	cfg.Logger = logger

	db, err := Open(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(db) })

	assert.Same(t, logger, db.Config.Logger)
	assert.Empty(t, logger.recorded(), "opening the pool must not emit a statement trace")

	require.NoError(t, db.Exec("CREATE TABLE logger_probe (id INTEGER PRIMARY KEY)").Error)
	traces := logger.recorded()
	require.NotEmpty(t, traces, "the injected logger must receive executed SQL")
	assert.Contains(t, strings.Join(traces, "\n"), "logger_probe")
}

// TestOpenKeepsTheDefaultLoggerWhenNoneIsInjected proves the nil path preserves
// GORM's behavior instead of leaving Config.Logger nil.
func TestOpenKeepsTheDefaultLoggerWhenNoneIsInjected(t *testing.T) {
	cfg := sqliteConfig(t)

	db, err := Open(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(db) })

	assert.Same(t, gormlogger.Default, db.Config.Logger)
}

// Compile-time guard that the test logger satisfies the injected contract.
var _ gormlogger.Interface = (*recordingLogger)(nil)
