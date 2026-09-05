package kafka

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Client is the concrete Kafka runtime value exported by Definition. It is a
// concurrently safe Producer, owns its configured consumers, and is stopped by
// XBC through the Definition's typed lifecycle adapter.
type Client struct {
	produceMu sync.RWMutex
	writer    messageWriter
	closed    bool
	closing   atomic.Bool

	lifecycleMu sync.Mutex

	handlerMu sync.Mutex
	handlers  map[string]Handler
	frozen    bool

	stateMu    sync.Mutex
	normalized normalizedConfig
	factory    backendFactory
	consumers  []*runningConsumer
	started    bool
	stopping   bool
	stopDone   chan struct{}
	stopErr    error
	runCancel  context.CancelFunc
	loops      sync.WaitGroup
}

var _ Producer = (*Client)(nil)

func newClient(writer messageWriter) *Client {
	return &Client{
		writer:   writer,
		handlers: make(map[string]Handler),
		stopDone: make(chan struct{}),
	}
}

// Produce writes a batch atomically with respect to shutdown. Multiple callers
// may execute concurrently; shutdown waits for admitted calls before closing
// the underlying writer.
func (c *Client) Produce(ctx context.Context, messages ...Message) error {
	if ctx == nil {
		return errors.New("kafka: Produce requires a non-nil context")
	}
	if len(messages) == 0 {
		return nil
	}
	if c == nil || c.closing.Load() {
		return ErrClosed
	}
	c.produceMu.RLock()
	defer c.produceMu.RUnlock()
	if c.closing.Load() || c.closed || c.writer == nil {
		return ErrClosed
	}
	if err := c.writer.Write(ctx, messages); err != nil {
		return errors.Join(errors.New("kafka: produce messages"), err)
	}
	return nil
}

func (c *Client) close() error {
	if c == nil {
		return nil
	}
	c.closing.Store(true)
	c.produceMu.Lock()
	defer c.produceMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.writer == nil {
		return nil
	}
	writer := c.writer
	c.writer = nil
	return writer.Close()
}
