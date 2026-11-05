package asynq

import (
	"context"
	"log/slog"

	xbclog "github.com/xbcio/xbc/log"
)

// newSlogLogger adapts the framework's log facade to the *slog.Logger that
// kit/distributed accepts, so the semaphore's diagnostics reach the same sink
// as the rest of the plugin.
func newSlogLogger(logger xbclog.Logger) *slog.Logger {
	if logger == nil {
		logger = xbclog.Nop()
	}
	return slog.New(facadeSlogHandler{logger: logger})
}

// facadeSlogHandler forwards records to the facade, turning each attribute
// into one key/value pair. The facade writes the level through the method it is
// called on, so the record's level chooses the method rather than an argument.
type facadeSlogHandler struct {
	logger xbclog.Logger
	attrs  []slog.Attr
}

func (facadeSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h facadeSlogHandler) Handle(_ context.Context, record slog.Record) error {
	args := make([]any, 0, 2*(len(h.attrs)+record.NumAttrs()))
	for _, attr := range h.attrs {
		args = append(args, attr.Key, attr.Value.Any())
	}
	record.Attrs(func(attr slog.Attr) bool {
		args = append(args, attr.Key, attr.Value.Any())
		return true
	})
	switch {
	case record.Level >= slog.LevelError:
		h.logger.Error(record.Message, args...)
	case record.Level >= slog.LevelWarn:
		h.logger.Warn(record.Message, args...)
	case record.Level >= slog.LevelInfo:
		h.logger.Info(record.Message, args...)
	default:
		h.logger.Debug(record.Message, args...)
	}
	return nil
}

func (h facadeSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return facadeSlogHandler{logger: h.logger, attrs: merged}
}

// WithGroup returns the handler unchanged: the semaphore logs flat records and
// never opens a group, so there is no group prefix to apply.
func (h facadeSlogHandler) WithGroup(string) slog.Handler { return h }
