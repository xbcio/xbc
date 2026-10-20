package gin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

const (
	// streamDeadlineBudget is the server-wide write timeout, and streamWriteDelay
	// deliberately exceeds it: a response that finished inside the budget would
	// prove nothing about the deadline being lifted.
	streamDeadlineBudget = 200 * time.Millisecond
	streamWriteDelay     = 500 * time.Millisecond
	// streamFirstChunkWait is shorter than streamWriteDelay, so the first chunk
	// is only observed in time if Write flushed it rather than leaving it
	// buffered until the handler returned.
	streamFirstChunkWait = 300 * time.Millisecond
)

// TestBeginStreamLiftsTheWriteDeadlineAndFlushesEveryWrite is the end-to-end
// contract of Ctx.BeginStream over a real gin engine and a real socket, with
// http.Server.WriteTimeout set to a budget the handler outlives.
//
// Three things are asserted together because each one alone is satisfied by a
// broken stack:
//
//   - The first chunk arrives before the handler's sleep ends, which proves
//     StreamWriter.Write flushes instead of leaving the bytes in the server's
//     buffer.
//   - The chunk written after the write-timeout budget elapses still reaches
//     the client, which proves BeginStream lifted the deadline.
//   - A control handler that writes the same way without BeginStream is cut by
//     the deadline, which proves the budget was real and enforced on exactly
//     this path.
func TestBeginStreamLiftsTheWriteDeadlineAndFlushesEveryWrite(t *testing.T) {
	restoreProcessGlobals(t)

	created, err := Factory{}.NewEngine(web.Options{WriteTimeout: streamDeadlineBudget})
	require.NoError(t, err)
	g, ok := created.(*engine)
	require.True(t, ok, "the factory must return this package's engine")

	const (
		first  = "first"
		second = "second"
	)
	handlerErrors := make(chan error, 1)
	g.Handle(http.MethodGet, "/stream", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			stream, beginErr := c.BeginStream(http.StatusOK, "text/event-stream")
			if beginErr != nil {
				handlerErrors <- beginErr
				return beginErr
			}
			if _, writeErr := io.WriteString(stream, first); writeErr != nil {
				handlerErrors <- writeErr
				return writeErr
			}
			time.Sleep(streamWriteDelay)
			if _, writeErr := io.WriteString(stream, second); writeErr != nil {
				handlerErrors <- writeErr
				return writeErr
			}
			return nil
		},
	})
	// The control is the same handler minus BeginStream: same writes, same
	// delay, same content type. Nothing else differs, so the deadline is the
	// only thing that can explain the different outcomes.
	g.Handle(http.MethodGet, "/control", []web.Handler{
		func(_ context.Context, c *web.Ctx) error {
			c.SetHeader("Content-Type", "text/event-stream")
			_, _ = io.WriteString(c.Writer(), first)
			time.Sleep(streamWriteDelay)
			_, _ = io.WriteString(c.Writer(), second)
			return nil
		},
	})
	t.Cleanup(func() {
		select {
		case handlerErr := <-handlerErrors:
			t.Errorf("the streaming handler failed: %v", handlerErr)
		default:
		}
	})

	server := httptest.NewUnstartedServer(g.e)
	server.Config.WriteTimeout = streamDeadlineBudget
	server.Start()
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	response, err := client.Get(server.URL + "/stream")
	require.NoError(t, err, "BeginStream must commit the headers before the handler sleeps")
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusOK, response.StatusCode,
		"the client must see the status BeginStream committed")
	assert.Equal(t, "text/event-stream", response.Header.Get("Content-Type"),
		"the client must see the content type BeginStream committed")

	firstChunk := make(chan string, 1)
	go func() {
		buffer := make([]byte, len(first))
		n, readErr := io.ReadFull(response.Body, buffer)
		if readErr != nil {
			firstChunk <- ""
			return
		}
		firstChunk <- string(buffer[:n])
	}()
	select {
	case chunk := <-firstChunk:
		assert.Equal(t, first, chunk, "the first chunk must arrive before the handler finishes")
	case <-time.After(streamFirstChunkWait):
		t.Fatal("the first chunk did not arrive before the handler finished; StreamWriter.Write must flush")
	}

	rest, err := io.ReadAll(response.Body)
	require.NoError(t, err, "a stream whose deadline BeginStream lifted must not be cut")
	assert.Equal(t, second, string(rest),
		"the chunk written after the write-timeout budget elapsed must still reach the client")

	var controlBody string
	controlResponse, controlErr := client.Get(server.URL + "/control")
	if controlErr == nil {
		body, readErr := io.ReadAll(controlResponse.Body)
		_ = controlResponse.Body.Close()
		controlBody = string(body)
		controlErr = readErr
	}
	assert.True(t, controlErr != nil || controlBody != first+second,
		"without BeginStream the write deadline must still cut the response; got body %q, error %v",
		controlBody, controlErr)
}
