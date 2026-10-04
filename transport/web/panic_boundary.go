package web

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"runtime/debug"
	"syscall"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// panicBoundary is the outermost PhaseRecover middleware. The Server assembles
// it itself rather than selecting it as a plugin, for the same reason it
// assembles the error boundary and the authentication middleware: a
// framework-owned ordering pin makes it required, and a required stage must
// not be something a composition root can leave out or an operator can switch
// off. Its identity is reserved accordingly; reserved_middleware.go records
// why the stage could not stay an opt-in Definition.
//
// It still sits inside the process-level in-flight gate, the request-body cap,
// and the error resolver attachment: those three are assembled ahead of every
// middleware, including this one, in (*Server).Start, and recovering a panic
// in them is deliberately out of scope -- see the required-stage paragraph in
// doc.go's "Contributions" section for the boundary this guarantees and the
// one it does not.
type panicBoundary struct {
	logger log.Logger
	stack  bool
}

func newPanicBoundary(logger log.Logger, stack bool) *panicBoundary {
	if logger == nil {
		logger = log.L()
	}
	return &panicBoundary{logger: logger, stack: stack}
}

func (b *panicBoundary) Handler() Handler { return b.handle }
func (*panicBoundary) Order() Order       { return Order{Phase: PhaseRecover} }

// panicBoundaryIdentity is the producer identity the Server attributes its
// built-in panic boundary to. Like errorBoundaryIdentity and
// authenticationIdentity it is an ordering anchor rather than a selectable
// plugin.
var panicBoundaryIdentity = plugin.Identity{Plugin: PanicBoundaryKey}

func (b *panicBoundary) handle(_ context.Context, c *Ctx) error {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		fields := []any{
			"method", c.Request().Method,
			"path", c.Request().URL.Path,
			"panic_type", reflect.TypeOf(recovered).String(),
			"response_written", c.Writer().Written(),
		}
		if route, ok := CurrentRoute(c); ok {
			fields = append(fields, "route", route.Path)
		}
		if b.stack {
			fields = append(fields, "stack", string(debug.Stack()))
		}

		if brokenConnection(recovered) {
			b.logger.Warn("http request aborted after connection failure", fields...)
			c.Abort()
			return
		}

		// Never log the recovered value itself: panic strings frequently
		// contain credentials or user-controlled data. The type and stack are
		// enough to diagnose the code path without copying request secrets
		// into logs.
		b.logger.Error("http request panic recovered", fields...)
		if c.Writer().Written() {
			// The status/body are already on the wire and cannot safely be
			// replaced.
			c.Abort()
			return
		}
		AbortProblem(c, NewProblem(http.StatusInternalServerError, "internal_server_error"))
	}()

	c.Next()
	return nil
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
