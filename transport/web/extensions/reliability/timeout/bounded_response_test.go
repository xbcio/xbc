package timeout

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func TestMaxBufferBytesConfigDefaultsAndValidation(t *testing.T) {
	env, err := config.NewEnvironment(nil, "XBC_TIMEOUT_BUFFER_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := env.Bind("plugins.timeout", &cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(&cfg, "plugins.timeout"); err != nil {
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

// TestWritePastTheBufferLimitStreamsWithoutA504 drives a real connection. The
// handler writes past the bound, then blocks until the deadline. The buffered
// prefix and the overflowing chunk must reach the client as one committed
// response with the handler's status; the deadline must not append a 504, and
// the handler must still observe context cancellation.
func TestWritePastTheBufferLimitStreamsWithoutA504(t *testing.T) {
	const limit = 64
	p := pluginFor(t, Config{Duration: 40 * time.Millisecond, MaxBufferBytes: limit})
	prefix := bytes.Repeat([]byte("a"), 20)
	chunk := bytes.Repeat([]byte("b"), 100)
	tail := bytes.Repeat([]byte("c"), 10)
	wantBody := append(append(append([]byte(nil), prefix...), chunk...), tail...)

	type observation struct {
		status  int
		size    int
		written bool
		count   int
		err     error
	}
	observed := make(chan observation, 1)
	canceled := make(chan struct{})

	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/overflow", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Handler", "yes")
		c.Status(http.StatusCreated)
		writer := c.Writer()
		if _, err := writer.Write(prefix); err != nil {
			return err
		}
		count, err := writer.Write(chunk)
		observed <- observation{status: writer.Status(), size: writer.Size(), written: writer.Written(), count: count, err: err}
		if _, err := writer.Write(tail); err != nil {
			return err
		}
		<-c.Request().Context().Done()
		close(canceled)
		return nil
	})
	server := httptest.NewServer(router)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(server.URL + "/overflow")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body %q)", response.StatusCode, http.StatusCreated, body)
	}
	if got := response.Header.Get("X-Handler"); got != "yes" {
		t.Fatalf("X-Handler = %q, want the handler header committed with the prefix", got)
	}
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("body = %d bytes, want the complete %d bytes", len(body), len(wantBody))
	}
	got := <-observed
	if got.err != nil || got.count != len(chunk) {
		t.Fatalf("overflow write = (%d, %v), want the underlying write's (%d, nil)", got.count, got.err, len(chunk))
	}
	if !got.written {
		t.Fatal("Written() must delegate to the committed underlying writer after overflow")
	}
	if got.status != http.StatusCreated {
		t.Fatalf("Status() = %d after overflow, want the committed %d", got.status, http.StatusCreated)
	}
	if got.size != len(prefix)+len(chunk) {
		t.Fatalf("Size() = %d after overflow, want the committed %d", got.size, len(prefix)+len(chunk))
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled after the response overflowed")
	}
}

// TestBufferLimitBoundary pins the exact limit: an exactly-full buffer is
// still replaceable by the deadline 504, while one byte more has already
// committed the response and must stream.
func TestBufferLimitBoundary(t *testing.T) {
	const limit = 64
	p := pluginFor(t, Config{Duration: 40 * time.Millisecond, MaxBufferBytes: limit})
	exact := bytes.Repeat([]byte("e"), limit)
	over := bytes.Repeat([]byte("o"), limit+1)

	router := enginetest.New()
	router.Use(p.handle)
	router.GET("/exact", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		if _, err := c.Writer().Write(exact); err != nil {
			return err
		}
		<-c.Request().Context().Done()
		return nil
	})
	router.GET("/over", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		if _, err := c.Writer().Write(over); err != nil {
			return err
		}
		<-c.Request().Context().Done()
		return nil
	})
	server := httptest.NewServer(router)
	defer server.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(server.URL + "/exact")
	if err != nil {
		t.Fatal(err)
	}
	exactBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("exactly-limit response = %d %q, want the replaceable 504", response.StatusCode, exactBody)
	}

	response, err = client.Get(server.URL + "/over")
	if err != nil {
		t.Fatal(err)
	}
	overBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("limit+1 response = %d %q, want the committed response", response.StatusCode, overBody)
	}
	if !bytes.Equal(overBody, over) {
		t.Fatalf("limit+1 body = %d bytes, want the complete %d bytes", len(overBody), len(over))
	}
}

// TestPanicAfterOverflowDoesNotProduceASecondResponse commits a partial
// response, then panics. The panic must propagate with the original writer
// restored and no replacement written: the client sees the overflowed response
// exactly once. The outer recover mirrors the framework panic boundary's rule
// for an already committed response.
func TestPanicAfterOverflowDoesNotProduceASecondResponse(t *testing.T) {
	const limit = 64
	p := pluginFor(t, Config{Duration: time.Second, MaxBufferBytes: limit})
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

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(server.URL + "/panic")
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
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("body = %q, want exactly the overflowed response", body)
	}
}

// writeHeaderCounter counts how many times the middleware beneath it commits a
// status. Every other method is promoted from the wrapped writer.
type writeHeaderCounter struct {
	web.ResponseWriter
	calls int
}

func (w *writeHeaderCounter) WriteHeader(code int) {
	w.calls++
	w.ResponseWriter.WriteHeader(code)
}

// TestOverflowThenFastReturnCommitsExactlyOnce pins the passthrough early
// return for the case the deadline test cannot observe: the handler overflows
// and returns before the deadline. Without the early return the deferred
// commit replaces the header and calls WriteHeader a second time. The counting
// writer is the middleware's underlying writer, so it sees exactly the calls
// the deferred commit would add.
func TestOverflowThenFastReturnCommitsExactlyOnce(t *testing.T) {
	const limit = 64
	p := pluginFor(t, Config{Duration: time.Second, MaxBufferBytes: limit})
	prefix := bytes.Repeat([]byte("a"), 20)
	chunk := bytes.Repeat([]byte("b"), 100)
	wantBody := append(append([]byte(nil), prefix...), chunk...)

	var counter *writeHeaderCounter
	router := enginetest.New()
	router.Use(func(_ context.Context, c *web.Ctx) error {
		counter = &writeHeaderCounter{ResponseWriter: c.Writer()}
		c.SetWriter(counter)
		c.Next()
		return nil
	})
	router.Use(p.handle)
	router.GET("/fast-overflow", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusCreated)
		if _, err := c.Writer().Write(prefix); err != nil {
			return err
		}
		if _, err := c.Writer().Write(chunk); err != nil {
			return err
		}
		return nil
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fast-overflow", nil))

	if counter == nil {
		t.Fatal("counting writer was not installed")
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want the committed %d", response.Code, http.StatusCreated)
	}
	if !bytes.Equal(response.Body.Bytes(), wantBody) {
		t.Fatalf("body = %d bytes, want the complete %d bytes", response.Body.Len(), len(wantBody))
	}
	if counter.calls != 1 {
		t.Fatalf("underlying WriteHeader calls = %d, want exactly 1 after a passthrough response", counter.calls)
	}
}
