package gin

import (
	"strings"

	ginlib "github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/log"
)

// applyProcessGlobals points gin's own diagnostic output at xbc's logger and
// picks gin's mode from what that logger will actually accept.
//
// Both are gin process state rather than per-engine settings, which is exactly
// why they belong here. transport/web has no way to express either without
// naming gin, and an engine with no such output simply never reads
// Options.Logger.
//
// Mode follows the logger's capability rather than a configuration key of its
// own: two switches could disagree, and gin's debug output is only worth
// producing when something is willing to print it.
func applyProcessGlobals(logger log.Logger) {
	if logger == nil {
		logger = log.Nop()
	}
	ginlib.DefaultWriter = logWriter{logger: logger, level: log.InfoLevel}
	ginlib.DefaultErrorWriter = logWriter{logger: logger, level: log.ErrorLevel}
	if logger.Enabled(log.DebugLevel) {
		ginlib.SetMode(ginlib.DebugMode)
		return
	}
	ginlib.SetMode(ginlib.ReleaseMode)
}

// logWriter adapts gin's io.Writer diagnostic sinks to a log.Logger. gin ends
// every line with a newline that a structured logger would otherwise record as
// part of the message.
type logWriter struct {
	logger log.Logger
	level  log.Level
}

func (w logWriter) Write(p []byte) (int, error) {
	message := strings.TrimRight(string(p), "\n")
	if message != "" {
		if w.level == log.ErrorLevel {
			w.logger.Error(message)
		} else {
			w.logger.Info(message)
		}
	}
	return len(p), nil
}
