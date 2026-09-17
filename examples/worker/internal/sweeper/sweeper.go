// Package sweeper is the worker application's business Plugin. It owns the
// recurring work that justifies the process existing, and it is the plugin that
// keeps a transportless application alive.
package sweeper

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is sweeper's stable configuration and runtime identity.
const Key plugin.Key = "sweeper"

// Config is bound from plugins.sweeper.
type Config struct {
	Interval time.Duration `yaml:"interval" default:"5s" validate:"gt=0"`
}

// DefaultConfig returns the same defaults XBC's configuration binder applies.
func DefaultConfig() Config {
	return Config{Interval: 5 * time.Second}
}

// Plugin performs one unit of recurring background work per tick.
type Plugin struct {
	cfg    Config
	logger log.Logger

	mu     sync.Mutex
	sweeps int
}

var (
	_ health.Contributor = (*Plugin)(nil)
	_ plugin.Runner      = (*Plugin)(nil)
	_ plugin.Closer      = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{Defaults: DefaultConfig},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return &Plugin{cfg: cfg}, nil
	},
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[health.Contributor](func(value *Plugin) health.Contributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns sweeper's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns sweeper's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// Start submits the recurring work as one critical managed task.
//
// GoCritical, not Go, for two reasons. It is what tells the runtime this
// application has a long-lived capability, and it means an unprompted return or
// a panic from the loop shuts the whole process down instead of leaving a live
// process with nothing running inside it.
//
// Submission is legal only while Start is executing, so the task is admitted
// here and never from Init or a goroutine the plugin spawned itself.
func (p *Plugin) Start(ctx *plugin.Context) error {
	p.logger = ctx.Log()
	gate := ctx.TrafficGate()
	if !ctx.GoCritical(func(taskCtx context.Context) { p.run(taskCtx, gate) }) {
		return errors.New("sweeper: runtime rejected the sweep task")
	}
	return nil
}

// run waits for the traffic gate before doing anything, exactly as a serving
// transport would. The gate closes only after every plugin's traffic
// preparation has succeeded, so no background work observes a half-built
// application.
func (p *Plugin) run(taskCtx context.Context, gate <-chan struct{}) {
	select {
	case <-gate:
	case <-taskCtx.Done():
		return
	}

	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-taskCtx.Done():
			// A prompted return is not a failure: the runtime canceled this
			// task, so returning is how the plugin cooperates with shutdown.
			return
		case <-ticker.C:
			p.sweep()
		}
	}
}

func (p *Plugin) sweep() {
	p.mu.Lock()
	p.sweeps++
	completed := p.sweeps
	p.mu.Unlock()
	p.logger.Info("sweeper: completed a sweep", "sweeps", completed)
}

// Stop runs in reverse graph order under the shared shutdown budget, after the
// managed task has already been canceled and joined. Reporting the final count
// here is safe precisely because of that ordering: no sweep can still be in
// flight by the time Stop is called.
func (p *Plugin) Stop(context.Context) error {
	p.mu.Lock()
	completed := p.sweeps
	p.mu.Unlock()
	p.logger.Info("sweeper: stopped", "sweeps", completed)
	return nil
}

// HealthChecks implements health.Contributor. The aggregator prefixes the name
// with this plugin's Identity, so the check reports as "sweeper/swept".
//
// A background service has no /readyz to serve, but readiness is still worth
// answering: this instance is not doing its job until the first sweep lands.
func (p *Plugin) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Name:    "swept",
		Kind:    health.Readiness,
		Timeout: 200 * time.Millisecond,
		Checker: health.CheckFunc(func(context.Context) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.sweeps == 0 {
				return errors.New("sweeper: no sweep has completed yet")
			}
			return nil
		}),
	}}
}
