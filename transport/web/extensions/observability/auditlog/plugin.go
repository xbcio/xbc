package auditlog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the plugin's stable configuration and middleware identity.
const Key plugin.Key = "auditlog"

// RequestIDExtractor decouples audit logging from any request-ID plugin.
type RequestIDExtractor func(*gin.Context) string

// ClientIPExtractor allows applications with a trusted-proxy policy to inject
// their own logic. The default reads RemoteAddr and ignores spoofable headers.
type ClientIPExtractor func(*gin.Context) string

type runtimeState struct {
	config    normalizedConfig
	sink      Sink
	logger    log.Logger
	dispatch  *asyncDispatcher
	requestID RequestIDExtractor
	clientIP  ClientIPExtractor
}

// Plugin contributes outer observation middleware and owns optional queueing.
type Plugin struct {
	state atomic.Pointer[runtimeState]

	lifecycleMu    sync.Mutex
	startAttempted bool
	workerAccepted bool
	stopDone       chan struct{}
	stopErr        error
}

type options struct {
	sink      Sink
	requestID RequestIDExtractor
	clientIP  ClientIPExtractor
}

// Option customizes a directly constructed Plugin.
type Option func(*options)

// WithSink injects an audit destination.
func WithSink(sink Sink) Option { return func(options *options) { options.sink = sink } }

// WithRequestIDExtractor supplies integration with an existing request-ID
// middleware without importing it.
func WithRequestIDExtractor(extractor RequestIDExtractor) Option {
	return func(options *options) { options.requestID = extractor }
}

// WithClientIPExtractor supplies a deployment's trusted-proxy policy.
func WithClientIPExtractor(extractor ClientIPExtractor) Option {
	return func(options *options) { options.clientIP = extractor }
}

var (
	_ plugin.Runner  = (*Plugin)(nil)
	_ plugin.Closer  = (*Plugin)(nil)
	_ web.Middleware = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg, ctx.Log())
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs directly usable audit middleware. Async dispatch is prepared
// during construction but starts only when Start admits its managed task.
func New(cfg Config, opts ...Option) (*Plugin, error) {
	return newPlugin(cfg, log.L(), opts...)
}

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func newPlugin(cfg Config, logger log.Logger, opts ...Option) (*Plugin, error) {
	config, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.L()
	}

	settings := options{}
	for _, option := range opts {
		if option != nil {
			option(&settings)
		}
	}

	sink := settings.sink
	if sink == nil {
		sink = NewLogSink(logger)
	}
	requestID := settings.requestID
	if requestID == nil {
		requestID = headerRequestID(config.requestIDHeader)
	}
	clientIP := settings.clientIP
	if clientIP == nil {
		clientIP = remoteClientIP
	}

	state := &runtimeState{
		config:    config,
		sink:      sink,
		logger:    logger,
		requestID: requestID,
		clientIP:  clientIP,
	}
	if config.async {
		state.dispatch = newAsyncDispatcher(sink, logger, config.queueSize, config.overflow, config.sinkTimeout)
	}
	value := &Plugin{}
	value.state.Store(state)
	return value, nil
}

// Handler returns the Gin middleware function.
func (p *Plugin) Handler() gin.HandlerFunc { return web.Handle(p.observe) }

// Order places audit logging in the outer observation phase.
func (*Plugin) Order() web.Order { return web.Order{Phase: web.PhaseObserve} }

// Definition returns auditlog's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns auditlog's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// Start admits the prepared async dispatcher as one critical managed task. A
// synchronous configuration has no task to submit.
func (p *Plugin) Start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("auditlog: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auditlog: Start canceled: %w", err)
	}

	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopDone != nil {
		return errors.New("auditlog: cannot Start after Stop")
	}
	if p.startAttempted {
		return errors.New("auditlog: Start called more than once")
	}
	p.startAttempted = true

	state := p.state.Load()
	if state == nil {
		return errors.New("auditlog: Start called on an unconstructed plugin")
	}
	if state.dispatch == nil {
		return nil
	}
	if !ctx.GoCritical(state.dispatch.run) {
		return errors.New("auditlog: runtime rejected dispatcher task during Start")
	}
	p.workerAccepted = true
	return nil
}

// Stop stops queue admission, drains accepted events within ctx's deadline,
// then invokes the optional Sink Flusher. It is concurrent and idempotent, and
// also drains a dispatcher that was constructed but never started.
func (p *Plugin) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.lifecycleMu.Lock()
	if p.stopDone != nil {
		done := p.stopDone
		p.lifecycleMu.Unlock()
		select {
		case <-done:
			p.lifecycleMu.Lock()
			err := p.stopErr
			p.lifecycleMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	p.stopDone = done
	workerAccepted := p.workerAccepted
	p.lifecycleMu.Unlock()

	err := p.stop(ctx, workerAccepted)
	p.lifecycleMu.Lock()
	p.stopErr = err
	close(done)
	p.lifecycleMu.Unlock()
	return err
}

func (p *Plugin) stop(ctx context.Context, workerAccepted bool) error {
	state := p.state.Load()
	if state == nil {
		return nil
	}
	// Runtime supplies a shared shutdown deadline. The additional timeout
	// keeps programmatic Stop(context.Background()) bounded as well.
	stopCtx, cancel := context.WithTimeout(ctx, state.config.sinkTimeout)
	defer cancel()
	var result error
	if state.dispatch != nil {
		result = state.dispatch.stopAndWait(stopCtx, workerAccepted)
	}
	if flusher, ok := state.sink.(Flusher); ok && stopCtx.Err() == nil {
		if err := callFlush(flusher, stopCtx); err != nil {
			result = errors.Join(result, fmt.Errorf("auditlog: flush sink: %w", err))
		}
	}
	return result
}

func callFlush(flusher Flusher, ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("sink panic: %v", recovered)
		}
	}()
	return flusher.Flush(ctx)
}

func (state *runtimeState) skipped(route, rawPath string) bool {
	for _, pattern := range state.config.skipPaths {
		if matched, _ := path.Match(pattern, route); matched {
			return true
		}
		if matched, _ := path.Match(pattern, rawPath); matched {
			return true
		}
	}
	return false
}

func headerRequestID(header string) RequestIDExtractor {
	return func(c *gin.Context) string {
		value := strings.TrimSpace(c.GetHeader(header))
		if len(value) > 256 || containsControl(value) {
			return ""
		}
		return value
	}
}

func remoteClientIP(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(c.Request.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(c.Request.RemoteAddr)
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
