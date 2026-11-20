package gormx

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormlogger "gorm.io/gorm/logger"
)

// recordingLogger captures the SQL GORM hands to Trace. It is defined here
// rather than beside the cgo-only test so both build configurations compile.
type recordingLogger struct {
	mu     sync.Mutex
	traces []string
}

func (l *recordingLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }

func (l *recordingLogger) Info(context.Context, string, ...interface{}) {}

func (l *recordingLogger) Warn(context.Context, string, ...interface{}) {}

func (l *recordingLogger) Error(context.Context, string, ...interface{}) {}

func (l *recordingLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.mu.Lock()
	l.traces = append(l.traces, sql)
	l.mu.Unlock()
}

func (l *recordingLogger) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.traces...)
}

// TestLoggerIsExcludedFromConfigurationBinding pins the documented boundary:
// Logger is a code-only field, so a configuration section that names it is
// rejected instead of silently ignored or decoded.
func TestLoggerIsExcludedFromConfigurationBinding(t *testing.T) {
	environment := testEnvironment(t, map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"primary": map[string]any{
					"driver": "sqlite",
					"dsn":    "file:logger-bind?mode=memory&cache=shared",
					"logger": "custom",
				},
			},
		},
	})

	var cfg Config
	err := environment.Bind("plugins.gorm.primary", &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "logger")
}

// TestPrepareConfigPreservesTheInjectedLogger proves that preparation, which
// runs on the direct Open path and during plugin planning, does not replace the
// code-supplied logger.
func TestPrepareConfigPreservesTheInjectedLogger(t *testing.T) {
	logger := &recordingLogger{}
	cfg := sqliteConfig(t)
	cfg.Logger = logger

	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Same(t, logger, prepared.Logger)

	assert.Nil(t, DefaultConfig().Logger)
}
