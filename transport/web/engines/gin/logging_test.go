package gin

import (
	"testing"

	ginlib "github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// capturingLogger records what gin's diagnostic sinks produced, and answers
// Enabled from a fixed capability so the mode decision is observable without
// reaching for a real logger's configuration.
type capturingLogger struct {
	log.Logger
	debug  bool
	info   []string
	errors []string
}

func (l *capturingLogger) Enabled(level log.Level) bool {
	return l.debug && level == log.DebugLevel
}

func (l *capturingLogger) Info(message string, _ ...any)  { l.info = append(l.info, message) }
func (l *capturingLogger) Error(message string, _ ...any) { l.errors = append(l.errors, message) }

// restoreProcessGlobals returns gin's process state to what it was, so one
// test's mode choice cannot decide another test's assertions.
func restoreProcessGlobals(t *testing.T) {
	t.Helper()
	mode := ginlib.Mode()
	out, errOut := ginlib.DefaultWriter, ginlib.DefaultErrorWriter
	t.Cleanup(func() {
		ginlib.SetMode(mode)
		ginlib.DefaultWriter, ginlib.DefaultErrorWriter = out, errOut
	})
}

func TestProcessGlobalsFollowLoggerCapability(t *testing.T) {
	restoreProcessGlobals(t)

	applyProcessGlobals(&capturingLogger{Logger: log.Nop(), debug: true})
	assert.Equal(t, ginlib.DebugMode, ginlib.Mode(), "logger 接受 debug 时 gin 必须进 debug 模式")

	applyProcessGlobals(&capturingLogger{Logger: log.Nop()})
	assert.Equal(t, ginlib.ReleaseMode, ginlib.Mode(), "logger 不接受 debug 时 gin 必须进 release 模式")
}

// TestProcessGlobalsRouteGinOutputIntoTheLogger is the half the old internal
// test never covered: setting the mode without redirecting the sinks would
// leave gin writing to the process's standard streams, which is exactly the
// behaviour moving this into the adapter is supposed to preserve.
func TestProcessGlobalsRouteGinOutputIntoTheLogger(t *testing.T) {
	restoreProcessGlobals(t)
	logger := &capturingLogger{Logger: log.Nop()}

	applyProcessGlobals(logger)
	_, err := ginlib.DefaultWriter.Write([]byte("routine notice\n"))
	require.NoError(t, err)
	_, err = ginlib.DefaultErrorWriter.Write([]byte("failure notice\n"))
	require.NoError(t, err)
	_, err = ginlib.DefaultWriter.Write([]byte("\n"))
	require.NoError(t, err)

	assert.Equal(t, []string{"routine notice"}, logger.info, "gin 的常规输出必须落到 Info，且去掉结尾换行")
	assert.Equal(t, []string{"failure notice"}, logger.errors, "gin 的错误输出必须落到 Error")
}

// TestProcessGlobalsToleratesAnAbsentLogger pins the nil branch: web.Options is
// a plain struct an application can build by hand, so the adapter cannot assume
// Server filled every field.
func TestProcessGlobalsToleratesAnAbsentLogger(t *testing.T) {
	restoreProcessGlobals(t)
	require.NotPanics(t, func() { applyProcessGlobals(nil) })
	assert.Equal(t, ginlib.ReleaseMode, ginlib.Mode(), "无 logger 时必须取安静的 release 模式")
}
