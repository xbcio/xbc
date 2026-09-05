package auditlog

import (
	"context"
	"fmt"
	"time"
)

// Event is deliberately metadata-only. No request/response body field exists,
// making accidental body logging impossible through the Sink contract.
type Event struct {
	Timestamp             time.Time     `json:"timestamp"`
	Method                string        `json:"method"`
	RouteTemplate         string        `json:"route_template,omitempty"`
	RouteName             string        `json:"route_name,omitempty"`
	Status                int           `json:"status"`
	Bytes                 int           `json:"bytes"`
	Latency               time.Duration `json:"latency"`
	ClientIP              string        `json:"client_ip,omitempty"`
	RequestID             string        `json:"request_id,omitempty"`
	Subject               string        `json:"subject,omitempty"`
	AuthMethod            string        `json:"auth_method,omitempty"`
	IdempotencyKeyPresent bool          `json:"idempotency_key_present"`
	Panicked              bool          `json:"panicked,omitempty"`
}

// Sink persists one immutable audit event and must honor ctx cancellation.
type Sink interface {
	Write(ctx context.Context, event Event) error
}

// Flusher is an optional Sink capability used during graceful Stop.
type Flusher interface {
	Flush(ctx context.Context) error
}

func callSink(sink Sink, ctx context.Context, event Event) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("auditlog: sink panic: %v", recovered)
		}
	}()
	return sink.Write(ctx, event)
}
