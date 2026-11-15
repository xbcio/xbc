package log

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// infoFormat is the shape recommended for an application's ctx-less,
// printf-style wrapper: take the global logger, add one caller skip for the
// wrapper's own frame through the optional CallerSkipper capability, and log.
//
// The type assertion matters: a third-party backend that does not implement
// CallerSkipper still receives the record -- only the recorded caller is less
// accurate -- while the default backend points at the business call site.
func infoFormat(format string, args ...any) {
	l := L()
	if cs, ok := l.(CallerSkipper); ok {
		l = cs.WithCallerSkip(1)
	}
	if l.Enabled(InfoLevel) {
		l.Info(fmt.Sprintf(format, args...))
	}
}

// TestCallerSkipForPackageLevelWrapper pins that the wrapper shape above keeps
// caller pointing at the line that called the wrapper, not at the wrapper
// itself.
func TestCallerSkipForPackageLevelWrapper(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	_, _, line, _ := runtime.Caller(0)
	infoFormat("placed %s", "order") // line + 1

	require.Len(t, logs.All(), 1)
	e := logs.All()[0]
	assert.Equal(t, "placed order", e.Message)
	assert.Equal(t, line+1, e.Caller.Line, "a package-level wrapper must skip its own frame")
	assert.Contains(t, e.Caller.File, "caller_wrapper_test.go")
}
