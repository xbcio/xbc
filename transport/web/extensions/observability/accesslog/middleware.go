package accesslog

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	state := p.state.Load()
	if state == nil {
		c.Next()
		return nil
	}
	if state.config.skip(c.Request().URL.Path) {
		c.Next()
		return nil
	}

	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			p.write(state, c, started, true)
			panic(recovered)
		}
		p.write(state, c, started, false)
	}()
	c.Next()
	return nil
}

func (p *Plugin) write(state *runtimeState, c *web.Ctx, started time.Time, panicked bool) {
	latency := time.Since(started)
	status := c.Writer().Status()
	if panicked && !c.Writer().Written() {
		status = http.StatusInternalServerError
	}
	bytes := c.Writer().Size()
	if bytes < 0 {
		bytes = 0
	}
	// "" is the fallback initial value for an unmatched request: web.CurrentRoute
	// overwrites it below whenever the request matched a frozen route, and gin
	// itself reports "" from FullPath for a request that matched no route.
	route := ""
	routeName := ""
	if info, ok := web.CurrentRoute(c); ok {
		route = info.Path
		routeName = info.Name
	}
	// Only consume the response header produced by the requestid middleware.
	// Falling back to the raw inbound header would let an unvalidated,
	// attacker-controlled value enter structured logs when requestid is absent.
	requestID := safeRequestID(c.Writer().Header(), state.config.requestIDHeader)
	fields := []any{
		"method", c.Request().Method,
		"path", c.Request().URL.Path,
		"route", route,
		"route_name", routeName,
		"status", status,
		"bytes", bytes,
		"latency", latency,
		"request_id", requestID,
		"client_ip", state.config.clientIP(c),
		"panicked", panicked,
	}

	// Deliberately do not include RawQuery, headers, cookies, request body,
	// errors, or panic values: all are common credential/PII leak paths.
	switch {
	case status >= http.StatusInternalServerError || panicked:
		state.logger.Error("http request completed", fields...)
	case state.config.slowRequest > 0 && latency >= state.config.slowRequest:
		state.logger.Warn("http request completed slowly", fields...)
	default:
		state.logger.Info("http request completed", fields...)
	}
}

func safeRequestID(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 1024 {
		return ""
	}
	value := values[0]
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == ':' {
			continue
		}
		return ""
	}
	return value
}

func (c normalizedConfig) skip(path string) bool {
	for _, rule := range c.skipPaths {
		if (!rule.prefix && path == rule.value) || (rule.prefix && strings.HasPrefix(path, rule.value)) {
			return true
		}
	}
	return false
}

func (c normalizedConfig) clientIP(ctx *web.Ctx) string {
	if c.trustProxyHeaders {
		if ip := net.ParseIP(strings.TrimSpace(ctx.ClientIP())); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(ctx.Request().RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(ctx.Request().RemoteAddr)
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "unknown"
}
