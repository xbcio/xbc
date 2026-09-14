package auditlog

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) observe(_ context.Context, c *web.Ctx) error {
	state := p.state.Load()
	if state == nil {
		c.Next()
		return nil
	}
	gc := c.Gin()
	route, routeFound := web.CurrentRoute(gc)
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
		principal, _ := web.CurrentPrincipal(gc)
		event := Event{
			Timestamp:             start.UTC(),
			Method:                method,
			Status:                status,
			Bytes:                 bytesWritten,
			Latency:               time.Since(start),
			ClientIP:              state.clientIP(gc),
			RequestID:             state.requestID(gc),
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
