package tracing

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Handle is the application-owned tracing contract exposed through
// plugin.Provide. It intentionally hides SDK and OTLP exporter implementations.
type Handle struct {
	provider    trace.TracerProvider
	propagator  propagation.TextMapPropagator
	forceFlush  func(context.Context) error
	shutdown    func(context.Context) error
	stopTimeout time.Duration

	stopMu   sync.Mutex
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

func newHandle(
	provider trace.TracerProvider,
	propagator propagation.TextMapPropagator,
	forceFlush func(context.Context) error,
	shutdown func(context.Context) error,
	stopTimeout time.Duration,
) *Handle {
	return &Handle{
		provider: provider, propagator: propagator,
		forceFlush: forceFlush, shutdown: shutdown, stopTimeout: stopTimeout,
		stopDone: make(chan struct{}),
	}
}

// Provider returns this plugin's private API-level TracerProvider. It is never
// installed as OpenTelemetry's global provider.
func (h *Handle) Provider() trace.TracerProvider {
	if h == nil {
		return nil
	}
	return h.provider
}

// Tracer creates an instrumentation tracer from the private provider.
func (h *Handle) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	if h == nil || h.provider == nil {
		return nil
	}
	return h.provider.Tracer(name, options...)
}

// Extract reads configured W3C propagation fields without consulting the
// process-wide propagator.
func (h *Handle) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.propagator == nil || carrier == nil {
		return ctx
	}
	return h.propagator.Extract(ctx, carrier)
}

// Inject writes configured W3C propagation fields without consulting the
// process-wide propagator.
func (h *Handle) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.propagator == nil || carrier == nil {
		return
	}
	h.propagator.Inject(ctx, carrier)
}

// ForceFlush exports all completed spans currently buffered by this handle.
func (h *Handle) ForceFlush(ctx context.Context) error {
	if h == nil || h.forceFlush == nil {
		return fmt.Errorf("tracing: handle is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.stopMu.Lock()
	if h.stopping {
		done := h.stopDone
		h.stopMu.Unlock()
		select {
		case <-done:
			h.stopMu.Lock()
			err := h.stopErr
			h.stopMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.stopMu.Unlock()

	return h.forceFlush(ctx)
}

// Shutdown starts exactly one bounded background provider shutdown. The SDK's
// BatchSpanProcessor shutdown flushes its queue before releasing the exporter,
// so calling ForceFlush first would only risk exhausting the one-shot shutdown
// deadline. Caller contexts limit waiting but are never passed to the provider;
// later callers can wait for and receive the shared cleanup result.
func (h *Handle) Shutdown(ctx context.Context) error {
	if h == nil || h.shutdown == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.stopMu.Lock()
	if !h.stopping {
		h.stopping = true
		go h.finishShutdown(h.stopDone)
	}
	done := h.stopDone
	h.stopMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		h.stopMu.Lock()
		err := h.stopErr
		h.stopMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handle) finishShutdown(done chan struct{}) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), h.stopTimeout)
	err := h.shutdown(cleanupCtx)
	cancel()

	h.stopMu.Lock()
	h.stopErr = err
	close(done)
	h.stopMu.Unlock()
}
