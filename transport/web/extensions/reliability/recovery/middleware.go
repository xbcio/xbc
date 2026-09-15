package recovery

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"runtime/debug"
	"syscall"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		state := p.state.Load()
		logger := corelog.L()
		includeStack := true
		if state != nil {
			logger = state.logger
			includeStack = state.stack
		}

		fields := []any{
			"method", c.Request().Method,
			"path", c.Request().URL.Path,
			"panic_type", reflect.TypeOf(recovered).String(),
			"response_written", c.Writer().Written(),
		}
		if route, ok := web.CurrentRoute(c); ok {
			fields = append(fields, "route", route.Path)
		}
		if includeStack {
			fields = append(fields, "stack", string(debug.Stack()))
		}

		if brokenConnection(recovered) {
			logger.Warn("http request aborted after connection failure", fields...)
			c.Abort()
			return
		}

		// Never log the recovered value itself: panic strings frequently contain
		// credentials or user-controlled data. The type and stack are enough to
		// diagnose the code path without copying request secrets into logs.
		logger.Error("http request panic recovered", fields...)
		if c.Writer().Written() {
			// The status/body are already on the wire and cannot safely be replaced.
			c.Abort()
			return
		}
		writeInternalServerError(c)
	}()

	c.Next()
	return nil
}

func writeInternalServerError(c *web.Ctx) {
	web.AbortProblem(c, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
}

func brokenConnection(value any) bool {
	err, ok := value.(error)
	if !ok {
		return false
	}
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, http.ErrAbortHandler)
}
