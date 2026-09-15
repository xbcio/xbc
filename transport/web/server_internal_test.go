package web

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/log"
)

// This file holds the server tests that cannot move to package web_test.
// setGinMode takes and observes gin's own process-global mode, so asserting on
// it requires naming gin types that never cross transport/web's public surface
// -- see Options and EngineFactory for what does. Everything else about the
// Server is exercised externally through enginetest, in server_test.go.

type modeLogger struct {
	log.Logger
	debug bool
}

func (l modeLogger) Enabled(level log.Level) bool {
	return l.debug && level == log.DebugLevel
}

func TestSetGinModeUsesLoggerCapabilityNotGlobalConfig(t *testing.T) {
	previous := gin.Mode()
	t.Cleanup(func() { gin.SetMode(previous) })

	setGinMode(modeLogger{Logger: log.Nop(), debug: true})
	assert.Equal(t, gin.DebugMode, gin.Mode())

	setGinMode(modeLogger{Logger: log.Nop()})
	assert.Equal(t, gin.ReleaseMode, gin.Mode())
}
