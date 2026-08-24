package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": DebugLevel, "DEBUG": DebugLevel, " Debug ": DebugLevel,
		"info": InfoLevel, "INFO": InfoLevel,
		"":     InfoLevel, // Empty string means "unspecified", not "unknown", so it falls to info; see ParseLevel's doc comment
		"warn": WarnLevel, "warning": WarnLevel, "WARN": WarnLevel,
		"error": ErrorLevel, "ERROR": ErrorLevel,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		require.NoError(t, err, "输入 %q", in)
		assert.Equal(t, want, got, "输入 %q", in)
	}

	_, err := ParseLevel("verbose")
	assert.Error(t, err, "未知级别必须报错，不能静默降级到 info")
}

func TestLevelString(t *testing.T) {
	assert.Equal(t, "DEBUG", DebugLevel.String())
	assert.Equal(t, "INFO", InfoLevel.String())
	assert.Equal(t, "WARN", WarnLevel.String())
	assert.Equal(t, "ERROR", ErrorLevel.String())

	// Unknown levels fall through to the default branch's fallback format, without panicking or returning an empty string.
	assert.Equal(t, "LEVEL(99)", Level(99).String(), "越界正值应兜底")
	assert.Equal(t, "LEVEL(-5)", Level(-5).String(), "越界负值同样应兜底")
}

// Level's numeric values must align with zapcore, so bindings can type-convert directly.
// This is an implicit contract; the test pins it down.
func TestLevelNumericallyMatchesZapcore(t *testing.T) {
	assert.EqualValues(t, zapcore.DebugLevel, DebugLevel)
	assert.EqualValues(t, zapcore.InfoLevel, InfoLevel)
	assert.EqualValues(t, zapcore.WarnLevel, WarnLevel)
	assert.EqualValues(t, zapcore.ErrorLevel, ErrorLevel)
}

func TestNopLoggerSatisfiesFacadeAndNeverPanics(t *testing.T) {
	var l Logger = Nop()
	assert.NotPanics(t, func() {
		l.Debug("d", "k", 1)
		l.Info("i")
		l.Warn("w", "k")
		l.Error("e", "k", nil)
		l.With("a", 1).Info("chained")
	})
	assert.False(t, l.Enabled(ErrorLevel), "Nop 对所有级别都不启用")
}
