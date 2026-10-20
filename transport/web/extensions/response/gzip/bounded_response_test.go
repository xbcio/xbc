package gzip

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func TestMaxBufferBytesConfigDefaultsAndValidation(t *testing.T) {
	env, err := config.NewEnvironment(nil, "XBC_GZIP_BUFFER_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := env.Bind("plugins.gzip", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(&cfg, "plugins.gzip"); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBufferBytes != defaultMaxBufferBytes {
		t.Fatalf("bound default = %d, want %d", cfg.MaxBufferBytes, defaultMaxBufferBytes)
	}
	if cfg.MaxBufferBytes != 1<<20 {
		t.Fatalf("default bound = %d, want 1 MiB", cfg.MaxBufferBytes)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	for _, value := range []int{0, -1} {
		invalid := DefaultConfig()
		invalid.MaxBufferBytes = value
		err := invalid.Validate()
		if err == nil || !strings.Contains(err.Error(), "max_buffer_bytes") {
			t.Fatalf("Validate(MaxBufferBytes=%d) = %v, want an error naming max_buffer_bytes", value, err)
		}
	}
}

// TestOverflowStreamsIdentity drives a real connection. A body past the bound
// must arrive complete and identity-encoded: no Content-Encoding, Vary intact,
// the handler's status and Content-Length preserved, and Content-Type detected
// from the buffered prefix.
func TestOverflowStreamsIdentity(t *testing.T) {
	const limit = 64
	cfg := DefaultConfig()
	cfg.MinLength = 0
	cfg.MaxBufferBytes = limit
	p := pluginForConfig(t, cfg)

	prefix := []byte("<!DOCTYPE html><html><body>")
	rest := bytes.Repeat([]byte("x"), 200)
	wantBody := append(append([]byte(nil), prefix...), rest...)

	type observation struct {
		status  int
		size    int
		written bool
		count   int
		err     error
	}
	observed := make(chan observation, 1)

	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/overflow", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusCreated)
		c.SetHeader("Content-Length", strconv.Itoa(len(wantBody)))
		writer := c.Writer()
		if _, err := writer.Write(prefix); err != nil {
			return err
		}
		count, err := writer.Write(rest)
		observed <- observation{status: writer.Status(), size: writer.Size(), written: writer.Written(), count: count, err: err}
		return nil
	})
	server := httptest.NewServer(router)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/overflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want the handler's %d", response.StatusCode, http.StatusCreated)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity after overflow", got)
	}
	if !headerContainsToken(response.Header.Values("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %v, want Accept-Encoding preserved", response.Header.Values("Vary"))
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want detection from the buffered prefix", got)
	}
	if response.ContentLength != int64(len(wantBody)) {
		t.Fatalf("Content-Length = %d, want the handler's %d", response.ContentLength, len(wantBody))
	}
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("body = %d bytes, want the complete %d bytes", len(body), len(wantBody))
	}
	got := <-observed
	if got.err != nil || got.count != len(rest) {
		t.Fatalf("overflow write = (%d, %v), want the underlying write's (%d, nil)", got.count, got.err, len(rest))
	}
	if !got.written {
		t.Fatal("Written() must delegate to the committed underlying writer after overflow")
	}
	if got.status != http.StatusCreated {
		t.Fatalf("Status() = %d after overflow, want the committed %d", got.status, http.StatusCreated)
	}
	if got.size != len(wantBody) {
		t.Fatalf("Size() = %d after overflow, want the committed %d", got.size, len(wantBody))
	}
}

// TestBufferLimitBoundary pins the exact limit: exactly max_buffer_bytes is
// still fully buffered and compressed, while one byte more has already been
// committed identity-encoded.
func TestBufferLimitBoundary(t *testing.T) {
	const limit = 64
	cfg := DefaultConfig()
	cfg.MinLength = 0
	cfg.MaxBufferBytes = limit
	p := pluginForConfig(t, cfg)

	exact := bytes.Repeat([]byte("e"), limit)
	over := bytes.Repeat([]byte("o"), limit+1)

	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/exact", func(_ context.Context, c *web.Ctx) error {
		c.Data(http.StatusOK, "text/plain", exact)
		return nil
	})
	router.GET("/over", func(_ context.Context, c *web.Ctx) error {
		c.Data(http.StatusOK, "text/plain", over)
		return nil
	})
	server := httptest.NewServer(router)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	rawRequest := func(path string) *http.Response {
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept-Encoding", "gzip")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	response := rawRequest("/exact")
	encoded, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("exactly-limit response encoding = %q, want gzip", got)
	}
	if got := ungzip(t, encoded); got != string(exact) {
		t.Fatalf("exactly-limit decoded body = %d bytes, want %d", len(got), len(exact))
	}

	response = rawRequest("/over")
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("limit+1 response encoding = %q, want identity", got)
	}
	if !bytes.Equal(body, over) {
		t.Fatalf("limit+1 body = %d bytes, want the complete %d bytes", len(body), len(over))
	}
}

// TestPanicAfterOverflowDoesNotProduceASecondResponse commits a partial
// response, then panics. The panic must propagate with the original writer
// restored and no replacement written: the client sees the overflowed response
// exactly once. The outer recover mirrors the framework panic boundary's rule
// for an already committed response.
func TestPanicAfterOverflowDoesNotProduceASecondResponse(t *testing.T) {
	const limit = 64
	cfg := DefaultConfig()
	cfg.MinLength = 0
	cfg.MaxBufferBytes = limit
	p := pluginForConfig(t, cfg)
	prefix := bytes.Repeat([]byte("a"), 20)
	chunk := bytes.Repeat([]byte("b"), 100)
	wantBody := append(append([]byte(nil), prefix...), chunk...)

	router := enginetest.New()
	router.Use(func(_ context.Context, c *web.Ctx) error {
		defer func() {
			if recovered := recover(); recovered != nil && !c.Writer().Written() {
				web.AbortProblem(c, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
			}
		}()
		return p.handle(context.Background(), c)
	})
	router.GET("/panic", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusCreated)
		writer := c.Writer()
		if _, err := writer.Write(prefix); err != nil {
			return err
		}
		if _, err := writer.Write(chunk); err != nil {
			return err
		}
		panic("boom")
	})
	server := httptest.NewServer(router)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/panic", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want the committed %d", response.StatusCode, http.StatusCreated)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("body = %q, want exactly the overflowed response", body)
	}
}

func pluginForConfig(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	state, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	p.state.Store(&state)
	return p
}

// TestOverflowDetectsContentTypeFromBufferedPrefix pins the Content-Type
// detection in overflow without relying on net/http's server-side sniffing.
// The response recorder beneath the wrapper never sniffs, so the header can
// only come from the middleware reading the buffered prefix. The exact-limit
// prefix is buffered and the next byte overflows. The buffer reset is pinned
// too: finish returns immediately in passthrough, and a non-empty buffer would
// mean the prefix could be replayed a second time.
func TestOverflowDetectsContentTypeFromBufferedPrefix(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx := enginetest.NewCtx(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	writer := newBufferingWriter(ctx.Writer(), 8)

	prefix := []byte("<html><b") // exactly the bound: buffered, not overflowed
	if n, err := writer.Write(prefix); err != nil || n != len(prefix) {
		t.Fatalf("buffered write = (%d, %v), want (%d, nil)", n, err, len(prefix))
	}
	if writer.body.Len() != len(prefix) {
		t.Fatalf("buffered body = %d bytes, want %d", writer.body.Len(), len(prefix))
	}
	if n, err := writer.Write([]byte("x")); err != nil || n != 1 {
		t.Fatalf("overflow write = (%d, %v), want (1, nil)", n, err)
	}
	if writer.body.Len() != 0 {
		t.Fatalf("buffer = %d bytes after overflow, want it reset", writer.body.Len())
	}
	if err := writer.finish(ctx.Request(), normalizedConfig{}); err != nil {
		t.Fatal(err)
	}

	result := recorder.Result()
	if got := result.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the buffered prefix detected without net/http sniffing", got)
	}
	if got := result.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	if got := recorder.Body.String(); got != string(prefix)+"x" {
		t.Fatalf("body = %q, want the complete overflowed body", got)
	}
}
