package gzip

import (
	"bufio"
	"bytes"
	compressgzip "compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	cfg := p.state.Load()
	if cfg == nil || cfg.excludedPath(c.Request().URL.Path) || isUpgrade(c.Request()) ||
		acceptsEventStream(c.Request().Header.Values("Accept")) ||
		c.Request().Header.Get("Range") != "" {
		c.Next()
		return nil
	}

	// A cache must distinguish compressed and identity representations even
	// when this particular request selected identity (including HEAD).
	addVary(c.Writer().Header(), "Accept-Encoding")
	if c.Request().Method == http.MethodHead || !gzipAccepted(c.Request().Header.Get("Accept-Encoding")) {
		c.Next()
		return nil
	}

	original := c.Writer()
	writer := newBufferingWriter(original)
	c.SetWriter(writer)
	defer func() {
		recovered := recover()
		c.SetWriter(original)
		if recovered != nil {
			// Both body and handler-owned headers are isolated until finish, so
			// the outer recovery boundary can still emit a clean response.
			panic(recovered)
		}
		if err := writer.finish(c.Request(), *cfg); err != nil {
			panic(err)
		}
	}()
	c.Next()
	return nil
}

type bufferingWriter struct {
	web.ResponseWriter
	header      http.Header
	body        bytes.Buffer
	status      int
	written     bool
	passthrough bool
}

func newBufferingWriter(writer web.ResponseWriter) *bufferingWriter {
	return &bufferingWriter{
		ResponseWriter: writer,
		header:         cloneHeader(writer.Header()),
		status:         writer.Status(),
	}
}

func (w *bufferingWriter) Header() http.Header {
	if w.passthrough {
		return w.ResponseWriter.Header()
	}
	return w.header
}

// WriteHeader records the status into the buffer. It deliberately does not
// mark the buffered response written: recording a status is not committing a
// response, and the "has this response been written yet" guards across the
// framework must give the same answer whether or not this middleware happens
// to be installed. Written flips on the first body write, which is exactly
// when this wrapper starts holding bytes the connection has not seen -- the
// case web.ResponseWriter documents Written for.
func (w *bufferingWriter) WriteHeader(code int) {
	if w.passthrough {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if code > 0 && !w.written {
		w.status = code
	}
}

func (w *bufferingWriter) Write(data []byte) (int, error) {
	if w.passthrough {
		return w.ResponseWriter.Write(data)
	}
	w.written = true
	return w.body.Write(data)
}

func (w *bufferingWriter) Status() int {
	if w.passthrough {
		return w.ResponseWriter.Status()
	}
	return w.status
}

func (w *bufferingWriter) Size() int {
	if w.passthrough {
		return w.ResponseWriter.Size()
	}
	if !w.written {
		return -1
	}
	return w.body.Len()
}

func (w *bufferingWriter) Written() bool {
	if w.passthrough {
		return w.ResponseWriter.Written()
	}
	return w.written
}

// Flush dynamically falls back to identity streaming. Normal SSE and upgrade
// requests are bypassed before wrapping; this path safely handles a handler
// that starts streaming without announcing it in the request.
func (w *bufferingWriter) Flush() {
	if !w.passthrough {
		w.header.Del("Content-Length")
		if err := w.commit(w.body.Bytes()); err != nil {
			panic(err)
		}
		w.body.Reset()
		w.passthrough = true
	}
	w.ResponseWriter.Flush()
}

func (w *bufferingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.passthrough && (w.written || w.body.Len() != 0) {
		return nil, nil, errors.New("gzip: cannot hijack after a buffered response write")
	}
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.passthrough = true
	}
	return conn, rw, err
}

func (w *bufferingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *bufferingWriter) finish(request *http.Request, cfg normalizedConfig) error {
	if w.passthrough {
		return nil
	}
	body := w.body.Bytes()
	contentType := w.header.Get("Content-Type")
	if contentType == "" && len(body) > 0 {
		contentType = http.DetectContentType(body)
		w.header.Set("Content-Type", contentType)
	}
	encoded := w.header.Get("Content-Encoding") != ""
	compress := !encoded && len(body) >= cfg.minLength && responseHasBody(request.Method, w.status) &&
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") &&
		!hasDirective(w.header.Values("Cache-Control"), "no-transform") &&
		w.header.Get("Content-Range") == "" && cfg.matchesContentType(contentType)

	if !compress {
		return w.commit(body)
	}

	var encodedBody bytes.Buffer
	compressor, err := compressgzip.NewWriterLevel(&encodedBody, cfg.level)
	if err != nil {
		return err
	}
	if _, err = compressor.Write(body); err != nil {
		return err
	}
	if err = compressor.Close(); err != nil {
		return err
	}
	w.header.Set("Content-Encoding", "gzip")
	w.header.Del("Content-Length")
	if etag := w.header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		w.header.Set("ETag", "W/"+etag)
	}
	return w.commit(encodedBody.Bytes())
}

func (w *bufferingWriter) commit(body []byte) error {
	replaceHeader(w.ResponseWriter.Header(), w.header)
	w.ResponseWriter.WriteHeader(w.status)
	if len(body) == 0 {
		return nil
	}
	_, err := w.ResponseWriter.Write(body)
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

func gzipAccepted(value string) bool {
	gzipQ := -1.0
	wildcardQ := -1.0
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		q := 1.0
		for _, parameter := range parts[1:] {
			keyValue := strings.SplitN(strings.TrimSpace(parameter), "=", 2)
			if len(keyValue) == 2 && strings.EqualFold(keyValue[0], "q") {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(keyValue[1]), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					q = 0
				} else {
					q = parsed
				}
			}
		}
		switch name {
		case "gzip", "x-gzip":
			gzipQ = q
		case "*":
			wildcardQ = q
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return wildcardQ > 0
}

func addVary(header http.Header, value string) {
	for _, line := range header.Values("Vary") {
		for _, existing := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(existing), value) || strings.TrimSpace(existing) == "*" {
				return
			}
		}
	}
	header.Add("Vary", value)
}

func isUpgrade(request *http.Request) bool {
	return headerContainsToken(request.Header.Values("Connection"), "upgrade") || request.Header.Get("Upgrade") != ""
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

func hasDirective(values []string, directive string) bool {
	return headerContainsToken(values, directive)
}

func responseHasBody(method string, status int) bool {
	return method != http.MethodHead && status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

var _ io.Writer = (*bufferingWriter)(nil)
var _ web.ResponseWriter = (*bufferingWriter)(nil)
