package casbin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/persist"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/security/rbac"
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
	cfg            normalizedConfig
	enforcer       *casbinlib.SyncedEnforcer
	resolver       SubjectResolver
	watcherFactory WatcherFactory

	// logger is populated once by start, before the periodic reload task (if
	// any) is submitted and before traffic opens. It is never retained as a
	// *plugin.Context: only this derived, safe-to-share value is kept.
	logger corelog.Logger

	// The following resources are published together under Plugin.mu only after
	// Start succeeds. Stop snapshots them under the same lock and transfers
	// ownership to its single cleanup worker.
	watcher persist.Watcher
	cancel  context.CancelFunc
	done    chan struct{}
}

// Plugin owns a synchronized Casbin enforcer and contributes route-aware Web
// authorization. Inline and file policies are loaded during construction.
// External adapters are attached during construction but first loaded in
// Start, after XBC's migration stage.
type Plugin struct {
	state *runtimeState

	mu         sync.Mutex
	mutationMu sync.Mutex
	started    bool
	stopped    atomic.Bool

	stopOnce sync.Once
	stopDone chan struct{}
}

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
			plugin.ExportAs[EnforcerProvider](func(value *Plugin) EnforcerProvider { return value }),
			plugin.ExportAs[rbac.Backend](func(value *Plugin) rbac.Backend { return value }),
		),
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Start: (*Plugin).start,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// plan declares only the exact configured provider instances. It is pure:
// provider values are read and the enforcer is constructed later by the
// returned factory, after graph wiring has completed.
func plan(cfg Config) (plugin.Plan[*Plugin], error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return plugin.Plan[*Plugin]{}, err
	}
	if !normalized.adapter.enabled() {
		return plugin.PlanOf(plugin.Inputs(), func(plugin.BuildContext) (*Plugin, error) {
			return newSourcePlugin(normalized, nil)
		}), nil
	}

	adapterRef := plugin.RefToInstance[AdapterProvider](normalized.adapter.key, normalized.adapter.instance)
	if !normalized.watcher.enabled() {
		return plugin.PlanOf(plugin.Inputs(adapterRef), func(ctx plugin.BuildContext) (*Plugin, error) {
			provider := adapterRef.Get(ctx).Value
			return newProviderPlugin(normalized, provider, nil, nil)
		}), nil
	}

	watcherRef := plugin.RefToInstance[WatcherFactory](normalized.watcher.key, normalized.watcher.instance)
	return plugin.PlanOf(plugin.Inputs(adapterRef, watcherRef), func(ctx plugin.BuildContext) (*Plugin, error) {
		adapterProvider := adapterRef.Get(ctx).Value
		watcherFactory := watcherRef.Get(ctx).Value
		return newProviderPlugin(normalized, adapterProvider, watcherFactory, nil)
	}), nil
}

// New constructs a directly usable Plugin from cfg. Inline/file models and
// policies are loaded immediately. Configured providers require graph-backed
// Definition/Bundle composition and are intentionally unavailable here.
func New(cfg Config, opts ...Option) (*Plugin, error) {
	var built options
	for _, opt := range opts {
		if opt != nil {
			opt(&built)
		}
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if normalized.adapter.enabled() {
		return nil, fmt.Errorf("casbin: adapter provider %s requires Definition/Bundle composition; New cannot resolve configured providers", providerIdentity(normalized.adapter))
	}
	return newSourcePlugin(normalized, built.resolver)
}

func newSourcePlugin(cfg normalizedConfig, resolver SubjectResolver) (*Plugin, error) {
	enforcer, err := buildSourceEnforcer(cfg)
	if err != nil {
		return nil, err
	}
	return newPlugin(cfg, enforcer, nil, resolver), nil
}

func newProviderPlugin(cfg normalizedConfig, provider AdapterProvider, watcherFactory WatcherFactory, resolver SubjectResolver) (*Plugin, error) {
	if isNil(provider) {
		return nil, fmt.Errorf("casbin: adapter provider %s returned a nil capability", providerIdentity(cfg.adapter))
	}
	adapter := provider.Adapter()
	if isNil(adapter) {
		return nil, fmt.Errorf("casbin: adapter provider %s returned a nil adapter", providerIdentity(cfg.adapter))
	}
	if cfg.watcher.enabled() && isNil(watcherFactory) {
		return nil, fmt.Errorf("casbin: watcher provider %s returned a nil factory", providerIdentity(cfg.watcher))
	}
	enforcer, err := buildProviderEnforcer(cfg, adapter)
	if err != nil {
		return nil, err
	}
	return newPlugin(cfg, enforcer, watcherFactory, resolver), nil
}

func newPlugin(cfg normalizedConfig, enforcer *casbinlib.SyncedEnforcer, watcherFactory WatcherFactory, resolver SubjectResolver) *Plugin {
	if resolver == nil {
		resolver = principalSubjectResolver{}
	}
	return &Plugin{
		state: &runtimeState{
			cfg:            cfg,
			enforcer:       enforcer,
			resolver:       resolver,
			watcherFactory: watcherFactory,
			logger:         corelog.Nop(),
		},
		stopDone: make(chan struct{}),
	}
}

func providerIdentity(ref normalizedProviderRef) string {
	return plugin.Identity{Plugin: ref.key, Instance: ref.instance}.String()
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Definition returns Casbin's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns Casbin's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// start attaches the optional watcher, performs the first external-adapter
// load, and begins managed periodic reload when configured. It stores only
// values derived from ctx and never retains the *plugin.Context itself.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return fmt.Errorf("casbin: Start requires a non-nil plugin context")
	}
	if p.stopped.Load() {
		return ErrStopped
	}

	// Holding mu for the complete Start transaction makes a concurrent Stop
	// wait until resources are either committed or locally rolled back. Stop
	// sets stopped before waiting, so each fallible step can observe shutdown.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped.Load() {
		return ErrStopped
	}
	if p.started {
		return fmt.Errorf("casbin: Start called more than once")
	}
	p.started = true
	rollback := func() { p.started = false }

	state := p.state
	state.logger = ctx.Log()

	var watcher persist.Watcher
	if state.cfg.adapter.enabled() && state.cfg.watcher.enabled() {
		opened, err := state.watcherFactory.Open(state.enforcer)
		if err != nil {
			rollback()
			return fmt.Errorf("casbin: open watcher %s: %w", providerIdentity(state.cfg.watcher), err)
		}
		if isNil(opened) {
			rollback()
			return fmt.Errorf("casbin: watcher provider %s returned nil without an error", providerIdentity(state.cfg.watcher))
		}
		watcher = opened
		if p.stopped.Load() {
			watcher.Close()
			rollback()
			return ErrStopped
		}
		if err := state.enforcer.SetWatcher(watcher); err != nil {
			watcher.Close()
			rollback()
			return fmt.Errorf("casbin: attach watcher %s: %w", providerIdentity(state.cfg.watcher), err)
		}
	}

	if state.cfg.adapter.enabled() {
		if p.stopped.Load() {
			if watcher != nil {
				watcher.Close()
			}
			rollback()
			return ErrStopped
		}
		if err := state.enforcer.LoadPolicy(); err != nil {
			if watcher != nil {
				watcher.Close()
			}
			rollback()
			return fmt.Errorf("casbin: initial adapter policy load: %w", err)
		}
	}

	var cancel context.CancelFunc
	var done chan struct{}
	if state.cfg.reloadInterval > 0 {
		reloadCtx, stopReload := context.WithCancel(context.Background())
		cancel = stopReload
		done = make(chan struct{})
		accepted := ctx.Go(func(taskCtx context.Context) {
			p.reloadLoop(taskCtx, reloadCtx, done, state)
		})
		if !accepted {
			cancel()
			if watcher != nil {
				watcher.Close()
			}
			rollback()
			return errors.New("casbin: runtime rejected periodic reload task during Start")
		}
	}

	if p.stopped.Load() {
		if cancel != nil {
			cancel()
			<-done
		}
		if watcher != nil {
			watcher.Close()
		}
		rollback()
		return ErrStopped
	}

	state.watcher = watcher
	state.cancel = cancel
	state.done = done
	return nil
}

// Enforcer returns the live synchronized enforcer while the plugin is active.
func (p *Plugin) Enforcer() (*casbinlib.SyncedEnforcer, bool) {
	if p.stopped.Load() {
		return nil, false
	}
	return p.state.enforcer, true
}

// LoadPolicy atomically reloads the configured policy source or external
// adapter. Concurrent Enforce calls see either the complete old policy or the
// complete new policy, never a partially loaded model.
func (p *Plugin) LoadPolicy() error {
	if p.stopped.Load() {
		return ErrStopped
	}
	if err := p.state.enforcer.LoadPolicy(); err != nil {
		return fmt.Errorf("casbin: reload policy: %w", err)
	}
	return nil
}

func (p *Plugin) reloadLoop(taskCtx, reloadCtx context.Context, done chan struct{}, state *runtimeState) {
	defer close(done)
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

// Stop cancels periodic reload, closes the owned watcher exactly once, and
// waits for cleanup. Repeated and concurrent calls share one cleanup operation
// and independently observe their deadlines. Cleanup runs separately because
// persist.Watcher.Close has no context-aware form and may block.
func (p *Plugin) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.stopped.Store(true)
	p.stopOnce.Do(func() { go p.finishStop() })

	// Prefer an already-complete shutdown over an already-cancelled caller.
	select {
	case <-p.stopDone:
		return nil
	default:
	}
	select {
	case <-p.stopDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("casbin: stop: %w", ctx.Err())
	}
}

func (p *Plugin) finishStop() {
	p.mu.Lock()
	state := p.state
	cancel, done, watcher := state.cancel, state.done, state.watcher
	state.cancel, state.done, state.watcher = nil, nil, nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if watcher != nil {
		watcher.Close()
	}
	if done != nil {
		<-done
	}
	close(p.stopDone)
}
