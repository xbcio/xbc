package kafka

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrClosed reports use after the producer has begun closing.
	ErrClosed = errors.New("kafka: producer is closed")
	// ErrHandlersFrozen reports registration at or after Start.
	ErrHandlersFrozen = errors.New("kafka: consumer handlers are frozen")
)

// Header is a Kafka record header without a dependency on kafka-go types.
type Header struct {
	Key   string
	Value []byte
}

// Message is the transport-neutral record used by both producers and
// consumers. Partition and Offset are populated for consumed messages.
type Message struct {
	Topic     string
	Key       []byte
	Value     []byte
	Headers   []Header
	Time      time.Time
	Partition int
	Offset    int64
}

// Producer is safe for concurrent use. Produce returns only after kafka-go's
// configured acknowledgement policy has been met.
type Producer interface {
	Produce(context.Context, ...Message) error
}

// Handler processes one record. Returning nil allows its offset to be
// committed; returning an error invokes the configured retry/error policy.
type Handler interface {
	Handle(context.Context, Message) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, Message) error

func (f HandlerFunc) Handle(ctx context.Context, message Message) error {
	return f(ctx, message)
}
