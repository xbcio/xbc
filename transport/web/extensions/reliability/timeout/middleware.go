package timeout

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	state := p.state.Load()
	gc := c.Gin()
	if state == nil || c.Writer().Written() || state.config.bypass(gc) {
		c.Next()
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), state.config.duration)
	defer cancel()
	c.SetContext(ctx)

	// Phase 4 debt: timeoutWriter embeds gin.ResponseWriter, so the writer
	// swap stays on gc until Ctx gains a neutral SetWriter.
	original := gc.Writer
	writer := newTimeoutWriter(original)
	gc.Writer = writer
	defer func() {
		recovered := recover()
		gc.Writer = original
		if recovered != nil {
			// Nothing buffered by this middleware reached the connection.
			panic(recovered)
		}
		if writer.passthrough {
			// A handler unexpectedly began streaming. Its response is already
			// committed and cannot be replaced safely.
			return
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.Abort()
			writeTimeoutResponse(gc, original)
			return
		}
		if err := writer.commit(); err != nil {
			panic(err)
		}
	}()
	c.Next()
	return nil
}

func writeTimeoutResponse(c *gin.Context, writer gin.ResponseWriter) {
	timeoutContext := c.Copy()
	timeoutContext.Writer = writer
	web.AbortProblem(timeoutContext, web.NewProblem(http.StatusGatewayTimeout, "gateway_timeout"))
}

func (cfg normalizedConfig) bypass(c *gin.Context) bool {
	if cfg.excludedPath(c.Request.URL.Path) || requestStreams(c.Request) {
		return true
	}
	if route, ok := web.CurrentRoute(c); ok {
		_, excluded := cfg.excludeRoutes[route.Name]
		return excluded
	}
	return false
}

func requestStreams(request *http.Request) bool {
	if request == nil {
		return false
	}
	if acceptsEventStream(request.Header.Values("Accept")) {
		return true
	}
	return request.Header.Get("Upgrade") != "" || headerContainsToken(request.Header.Values("Connection"), "upgrade")
}

func acceptsEventStream(values []string) bool {
	for _, line := range values {
		for _, item := range strings.Split(line, ",") {
			parts := strings.Split(item, ";")
			if !strings.EqualFold(strings.TrimSpace(parts[0]), "text/event-stream") {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				keyValue := strings.SplitN(strings.TrimSpace(parameter), "=", 2)
				if len(keyValue) != 2 || !strings.EqualFold(strings.TrimSpace(keyValue[0]), "q") {
					continue
				}
				parsed, err := strconv.ParseFloat(strings.TrimSpace(keyValue[1]), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					quality = 0
				} else {
					quality = parsed
				}
			}
			if quality > 0 {
				return true
			}
		}
	}
	return false
}

func headerContainsToken(values []string, token string) bool {
	for _, line := range values {
		for _, value := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(value), token) {
				return true
			}
		}
	}
	return false
}

type timeoutWriter struct {
	gin.ResponseWriter
	header      http.Header
	body        bytes.Buffer
	status      int
	written     bool
	passthrough bool
}

func newTimeoutWriter(writer gin.ResponseWriter) *timeoutWriter {
	return &timeoutWriter{
		ResponseWriter: writer,
		header:         cloneHeader(writer.Header()),
		status:         writer.Status(),
	}
}

func (w *timeoutWriter) Header() http.Header {
	if w.passthrough {
		return w.ResponseWriter.Header()
	}
	return w.header
}

func (w *timeoutWriter) WriteHeader(code int) {
	if w.passthrough {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if code > 0 && !w.written {
		w.status = code
	}
}

func (w *timeoutWriter) WriteHeaderNow() {
	if w.passthrough {
		w.ResponseWriter.WriteHeaderNow()
		return
	}
	w.written = true
}

func (w *timeoutWriter) Write(data []byte) (int, error) {
	if w.passthrough {
		return w.ResponseWriter.Write(data)
	}
	w.written = true
	return w.body.Write(data)
}

func (w *timeoutWriter) WriteString(value string) (int, error) {
	if w.passthrough {
		return w.ResponseWriter.WriteString(value)
	}
	w.written = true
	return w.body.WriteString(value)
}

func (w *timeoutWriter) Status() int {
	if w.passthrough {
		return w.ResponseWriter.Status()
	}
	return w.status
}

func (w *timeoutWriter) Size() int {
	if w.passthrough {
		return w.ResponseWriter.Size()
	}
	if !w.written {
		return -1
	}
	return w.body.Len()
}

func (w *timeoutWriter) Written() bool {
	if w.passthrough {
		return w.ResponseWriter.Written()
	}
	return w.written
}

// Flush switches to passthrough rather than allowing a timeout goroutine to
// race a streaming writer. Streaming routes should normally be bypassed by
// request headers or configuration; this is a safe dynamic fallback.
func (w *timeoutWriter) Flush() {
	if !w.passthrough {
		w.header.Del("Content-Length")
		if err := w.commit(); err != nil {
			panic(err)
		}
		w.body.Reset()
		w.passthrough = true
	}
	w.ResponseWriter.Flush()
}

func (w *timeoutWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.passthrough && (w.written || w.body.Len() != 0) {
		return nil, nil, errors.New("timeout: cannot hijack after a buffered response write")
	}
	conn, rw, err := w.ResponseWriter.Hijack()
	if err == nil {
		w.passthrough = true
	}
	return conn, rw, err
}

func (w *timeoutWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *timeoutWriter) commit() error {
	replaceHeader(w.ResponseWriter.Header(), w.header)
	w.ResponseWriter.WriteHeader(w.status)
	if w.body.Len() == 0 {
		return nil
	}
	_, err := w.ResponseWriter.Write(w.body.Bytes())
	return err
}

func cloneHeader(source http.Header) http.Header {
	cloned := make(http.Header, len(source))
	for key, values := range source {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func replaceHeader(destination, source http.Header) {
	for key := range destination {
		delete(destination, key)
	}
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

var _ gin.ResponseWriter = (*timeoutWriter)(nil)
