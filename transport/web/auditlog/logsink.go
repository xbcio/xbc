package auditlog

import (
	"context"

	"github.com/xbcio/xbc/log"
)

// LogSink writes structured fields through XBC's logging facade.
type LogSink struct{ logger log.Logger }

// NewLogSink builds the default metadata-only sink.
func NewLogSink(logger log.Logger) *LogSink {
	if logger == nil {
		logger = log.L()
	}
	return &LogSink{logger: logger}
}

// Write records one structured event. Neither bodies nor idempotency-key values
// are representable by Event, so they cannot leak through this implementation.
func (s *LogSink) Write(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.logger.Info("http audit",
		"timestamp", event.Timestamp,
		"method", event.Method,
		"route_template", event.RouteTemplate,
		"route_name", event.RouteName,
		"status", event.Status,
		"bytes", event.Bytes,
		"latency", event.Latency,
		"client_ip", event.ClientIP,
		"request_id", event.RequestID,
		"subject", event.Subject,
		"auth_method", event.AuthMethod,
		"idempotency_key_present", event.IdempotencyKeyPresent,
		"panicked", event.Panicked,
	)
	return nil
}
