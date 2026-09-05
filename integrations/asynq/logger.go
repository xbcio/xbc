package asynq

import (
	"fmt"

	xbclog "github.com/xbcio/xbc/log"
)

type asynqLogger struct{ logger xbclog.Logger }

func (l asynqLogger) Debug(args ...interface{}) { l.logger.Debug(fmt.Sprint(args...)) }
func (l asynqLogger) Info(args ...interface{})  { l.logger.Info(fmt.Sprint(args...)) }
func (l asynqLogger) Warn(args ...interface{})  { l.logger.Warn(fmt.Sprint(args...)) }
func (l asynqLogger) Error(args ...interface{}) { l.logger.Error(fmt.Sprint(args...)) }

// Fatal deliberately logs at Error rather than terminating the process. XBC
// owns process lifecycle; a third-party worker logger must not bypass its
// rollback and graceful-shutdown path with os.Exit.
func (l asynqLogger) Fatal(args ...interface{}) { l.logger.Error(fmt.Sprint(args...)) }
