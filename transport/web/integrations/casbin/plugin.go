package casbin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	casbinlib "github.com/casbin/casbin/v2"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable Definition, configuration, and middleware identity.
const Key plugin.Key = "casbin"

// ErrStopped reports an operation attempted after lifecycle shutdown began.
var ErrStopped = errors.New("casbin: plugin is stopped")

// Option customizes a directly constructed Plugin.
type Option func(*options)

type options struct {
	resolver SubjectResolver
}

// WithSubjectResolver replaces the default web.CurrentPrincipal resolver.
// The supplied resolver is responsible for accepting only verified identity
// facts. A nil resolver falls back to the default Principal contract.
func WithSubjectResolver(resolver SubjectResolver) Option {
	return func(o *options) { o.resolver = resolver }
}

type runtimeState struct {
	cfg      normalizedConfig
	enforcer *casbinlib.SyncedEnforcer
	resolver SubjectResolver

	// logger is populated once by start, before the periodic reload task (if
	// any) is submitted and before traffic opens. It is never retained as a
	// *plugin.Context: only this derived, safe-to-share value is kept.
	logger corelog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

// Plugin owns a synchronized Casbin enforcer and contributes route-aware Web
// authorization. Its enforcer is built once, from validated configuration,
// when the Plugin is constructed; the configured policy source remains
// available for atomic reload throughout the plugin lifecycle.
type Plugin struct {
	state *runtimeState

	mu      sync.Mutex
	started bool
	stopped atomic.Bool
}

var (
	_ web.Middleware = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg, nil)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
			plugin.ExportAs(func(value *Plugin) *casbinlib.SyncedEnforcer { return value.state.enforcer }),
		),
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Start: (*Plugin).start,
			Stop:  (*Plugin).Stop,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable Plugin from cfg. The enforcer's model and
// policy are loaded immediately; a configuration or source error is returned
// rather than deferred.
func New(cfg Config, opts ...Option) (*Plugin, error) {
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	return newPlugin(cfg, built.resolver)
}

func newPlugin(cfg Config, resolver SubjectResolver) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	enforcer, err := buildEnforcer(normalized)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = principalSubjectResolver{}
	}
	return &Plugin{
		state: &runtimeState{
			cfg:      normalized,
			enforcer: enforcer,
			resolver: resolver,
			logger:   corelog.Nop(),
		},
	}, nil
}

// Definition returns Casbin's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns Casbin's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// start begins managed periodic policy reload when configured. It stores only
// the derived log.Logger obtained from ctx, never ctx itself, so no later
// call can retain a *plugin.Context past this method.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return fmt.Errorf("casbin: Start requires a non-nil plugin context")
	}
	if p.stopped.Load() {
		return ErrStopped
	}

	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("casbin: Start called more than once")
	}
	p.started = true
	p.mu.Unlock()

	p.state.logger = ctx.Log()
	if p.state.cfg.reloadInterval <= 0 {
		return nil
	}

	reloadCtx, cancel := context.WithCancel(context.Background())
	state := p.state
	state.cancel = cancel
	state.done = make(chan struct{})
	accepted := ctx.Go(func(taskCtx context.Context) {
		p.reloadLoop(taskCtx, reloadCtx, state)
	})
	if !accepted {
		cancel()
		state.cancel = nil
		state.done = nil
		p.mu.Lock()
		p.started = false
		p.mu.Unlock()
		return errors.New("casbin: runtime rejected periodic reload task during Start")
	}
	return nil
}

// Enforcer returns the live synchronized enforcer while the plugin is active.
func (p *Plugin) Enforcer() (*casbinlib.SyncedEnforcer, bool) {
	if p.stopped.Load() {
		return nil, false
	}
	return p.state.enforcer, true
}

// LoadPolicy atomically reloads the configured inline text or policy file.
// Concurrent Enforce calls continue against either the complete old policy or
// the complete new policy; they never observe a partially loaded model.
func (p *Plugin) LoadPolicy() error {
	if p.stopped.Load() {
		return ErrStopped
	}
	if err := p.state.enforcer.LoadPolicy(); err != nil {
		return fmt.Errorf("casbin: reload policy: %w", err)
	}
	return nil
}

func (p *Plugin) reloadLoop(taskCtx, reloadCtx context.Context, state *runtimeState) {
	defer close(state.done)
	ticker := time.NewTicker(state.cfg.reloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-taskCtx.Done():
			return
		case <-reloadCtx.Done():
			return
		case <-ticker.C:
			if err := state.enforcer.LoadPolicy(); err != nil {
				state.logger.Error("casbin: periodic policy reload failed", "error", err)
			}
		}
	}
}

// Stop terminates periodic reload and waits for its managed task. It is safe
// to call repeatedly and respects the caller's shutdown deadline.
func (p *Plugin) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.stopped.Store(true)
	state := p.state
	if state.cancel == nil || state.done == nil {
		return nil
	}
	state.cancel()
	select {
	case <-state.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("casbin: stop policy reload: %w", ctx.Err())
	}
}
