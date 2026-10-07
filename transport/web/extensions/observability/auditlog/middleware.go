package auditlog

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/transport/web"
)

// observe is auditlog's single middleware handler. It deliberately does not
// require, or even check for, a published authentication.Principal: unlike
// tenant and idempotency -- which each gate access or a stored side effect on
// caller identity and so declare authentication.RequiresPrincipal -- auditlog
// is an observability sidecar that must keep recording every request Web
// actually serves, authenticated or not. A missing Principal here is recorded
// as an empty Subject/AuthMethod rather than skipped or refused; see
// TestNoPrincipalRequestRecordsEmptySubject. Its Order (PhaseObserve) is also
// deliberately placed before PhaseAuth rather than pinned after it: the hard
// phase boundary wraps authentication inside this handler's own deferred
// closure, so a rejected credential, an exempt route, or a panic are all still
// observed with the request's final outcome.
func (p *Plugin) observe(_ context.Context, c *web.Ctx) error {
	state := p.state.Load()
	if state == nil {
		c.Next()
		return nil
	}
	route, routeFound := web.CurrentRoute(c)
	routePath := route.Path
	rawPath := ""
	method := ""
	if c.Request() != nil {
		method = c.Request().Method
		if c.Request().URL != nil {
			rawPath = c.Request().URL.Path
		}
	}
	if state.skipped(routePath, rawPath) {
		c.Next()
		return nil
	}
	start := time.Now()

	defer func() {
		panicValue := recover()
		status := c.Writer().Status()
		if status <= 0 {
			status = http.StatusOK
		}
		if panicValue != nil {
			status = http.StatusInternalServerError
		}
		bytesWritten := c.Writer().Size()
		if bytesWritten < 0 {
			bytesWritten = 0
		}
		principal, _ := web.CurrentPrincipal(c)
		event := Event{
			Timestamp:             start.UTC(),
			Method:                method,
			Status:                status,
			Bytes:                 bytesWritten,
			Latency:               time.Since(start),
			ClientIP:              state.clientIP(c),
			RequestID:             state.requestID(c),
			Subject:               principal.Subject,
			AuthMethod:            principal.AuthMethod,
			IdempotencyKeyPresent: strings.TrimSpace(c.GetHeader(state.config.idempotencyHeader)) != "",
			Panicked:              panicValue != nil,
		}
		if routeFound {
			event.RouteTemplate = route.Path
			event.RouteName = route.Name
		}
		p.record(c.Request().Context(), state, event)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	c.Next()
	return nil
}

func (p *Plugin) record(requestContext context.Context, state *runtimeState, event Event) {
	if state.dispatch != nil {
		if err := state.dispatch.submit(requestContext, event); err != nil {
			state.logger.Warn("audit event was not queued", "error", err, "dropped_total", state.dispatch.droppedCount())
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), state.config.sinkTimeout)
	defer cancel()
	if err := callSink(state.sink, ctx, event); err != nil {
		state.logger.Error("audit sink write failed", "error", err)
	}
}
