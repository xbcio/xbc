package tracing

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/accesslog"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.handleRequest }

// Order implements web.Middleware. It runs in the observation phase, ahead
// of metrics and access logging, so those middlewares observe a request that
// already carries an active span.
func (*Plugin) Order() web.Order {
	return web.Order{
		Phase:  web.PhaseObserve,
		Before: []web.OrderRef{web.Prefer(metricsKey), web.Prefer(accesslog.Key)},
	}
}

func (p *Plugin) handleRequest(c *gin.Context) {
	if c.Request == nil || p.handle == nil {
		c.Next()
		return
	}

	method := boundedMethod(c.Request.Method)
	route := "unmatched"
	if info, ok := web.CurrentRoute(c); ok && info.Path != "" {
		route = info.Path
	}
	parent := p.handle.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
	tracer := p.handle.Tracer(instrumentationName)
	if tracer == nil {
		c.Next()
		return
	}
	requestContext, span := tracer.Start(
		parent,
		method+" "+route,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.HTTPRequestMethodKey.String(method),
			semconv.HTTPRoute(route),
		),
	)
	c.Request = c.Request.WithContext(requestContext)
	if headers := p.config.responseHeaders; headers.Enabled {
		spanContext := span.SpanContext()
		if spanContext.IsValid() {
			c.Header(headers.TraceIDHeader, spanContext.TraceID().String())
			c.Header(headers.SpanIDHeader, spanContext.SpanID().String())
		}
	}

	defer func() {
		panicValue := recover()
		status := c.Writer.Status()
		if panicValue != nil {
			status = http.StatusInternalServerError
		} else if status <= 0 {
			status = http.StatusOK
		}
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if panicValue != nil {
			span.RecordError(panicAsError(panicValue), trace.WithStackTrace(true))
			span.SetStatus(codes.Error, http.StatusText(http.StatusInternalServerError))
		} else if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		span.End()
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	c.Next()
}

func panicAsError(value any) error {
	if err, ok := value.(error); ok {
		return err
	}
	return fmt.Errorf("panic: %v", value)
}

func boundedMethod(method string) string {
	switch method = strings.ToUpper(strings.TrimSpace(method)); method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}
