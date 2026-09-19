// Package transcode is the workloads example's exclusive workload.
//
// A workload is a named group of Definitions a process carries as a unit or not
// at all, declared once at the composition root and selected by placement. This
// one is declared with plugin.WithExclusiveProcess, so a process carrying it
// carries nothing else.
//
// What earns that declaration is a process-wide side effect rather than size:
// a transcode-only deployment pairs the declaration with the framework's own
// process-level knobs -- a lower xbc.runtime.gc_percent, a different
// xbc.runtime.memory_limit -- and a knob of that kind lands on every
// process-mate whether or not it wants it. Nothing sharing this process could
// opt out, so nothing shares it. A workload that were merely heavy would express
// that as a replica count and a task budget instead, neither of which forces it
// into a process of its own.
package transcode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is transcode's stable plugin configuration and runtime identity. It owns
// the plugins.transcode section.
const Key plugin.Key = "transcode"

// WorkloadKey is the placement identity of the workload transcode belongs to.
// It owns the workloads.transcode section.
//
// It shares the plugin's spelling, as the specification's sast example does,
// but the two name different namespaces: plugins.transcode configures one
// plugin, while workloads.transcode decides whether this process carries the
// whole group.
const WorkloadKey plugin.WorkloadKey = "transcode"

// Config is bound from plugins.transcode.
type Config struct {
	// Interval is how often a transcode pass runs while this process carries
	// the workload.
	Interval time.Duration `yaml:"interval" default:"2s" validate:"gt=0"`
}

// DefaultConfig returns the same defaults XBC's configuration binder applies.
func DefaultConfig() Config {
	return Config{Interval: 2 * time.Second}
}

// Plugin performs one transcode pass per tick and answers a status route.
type Plugin struct {
	cfg    Config
	logger log.Logger

	mu     sync.Mutex
	passes int
}

var (
	_ web.RouteContributor = (*Plugin)(nil)
	_ health.Contributor   = (*Plugin)(nil)
	_ plugin.Runner        = (*Plugin)(nil)
	_ plugin.Closer        = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{Defaults: DefaultConfig},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		// The logger is defaulted here as well as in Start, because the migrate
		// subcommand unwinds the graph without ever running a Start hook, and a Stop
		// hook that logged through a nil logger would panic on that path.
		return &Plugin{cfg: cfg, logger: log.L()}, nil
	},
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
			plugin.ExportAs[health.Contributor](func(value *Plugin) health.Contributor { return value }),
		),
	},
)

// bundle is the whole point of this package: plugin.WorkloadOf tags every entry
// with a workload key and attaches the placement constraints, and that tag is
// what lets assembly leave these Definitions out of a process that does not
// carry the workload.
//
// WithReplicas is a declaration the placement source reads rather than one this
// package enforces. A source that hands out slots makes it this workload's slot
// count -- two processes may carry it at once, and a third waits for one of them
// to go away -- which is what the lease source under
// extensions/coordination/placement does. The static placement these examples
// use assigns no slots and ignores the count.
var bundle = plugin.WorkloadOf(
	WorkloadKey,
	plugin.BundleOf(definition),
	plugin.WithExclusiveProcess(),
	plugin.WithReplicas(2),
)

// Definition returns transcode's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this workload's side-effect-free composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// Start submits the transcode loop as one critical managed task.
//
// GoCritical rather than Go, because this loop is the reason a process carrying
// the workload exists: an unprompted return or a panic from it must take the
// process down rather than leave a live process with nothing running inside it.
//
// Submission is legal only while Start is executing, so the task is admitted
// here and never from Init or a goroutine this plugin spawned itself.
func (p *Plugin) Start(ctx *plugin.Context) error {
	p.logger = ctx.Log()
	gate := ctx.TrafficGate()
	if !ctx.GoCritical(func(taskCtx context.Context) { p.run(taskCtx, gate) }) {
		return errors.New("transcode: runtime rejected the transcode task")
	}
	return nil
}

// run waits for the traffic gate before doing anything, exactly as a serving
// transport does: the gate closes only after every plugin's traffic preparation
// has succeeded, so background work never observes a half-built application.
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
			p.pass()
		}
	}
}

func (p *Plugin) pass() {
	p.mu.Lock()
	p.passes++
	completed := p.passes
	p.mu.Unlock()
	p.logger.Info("transcode: completed a pass", "passes", completed)
}

// Stop runs in reverse graph order under the shared shutdown budget, after the
// managed task has already been canceled and joined.
func (p *Plugin) Stop(context.Context) error {
	p.logger.Info("transcode: stopped", "passes", p.passCount())
	return nil
}

func (p *Plugin) passCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.passes
}

// RegisterRoutes implements web.RouteContributor. The route exists only in a
// process that carries this workload: an unhosted workload contributes no
// Definition, so its routes are simply not in this process's table and a
// request for one is answered 404 rather than forwarded anywhere.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/transcode/status", p.status).
		Name("transcode.status").
		Auth(web.Public())
}

func (p *Plugin) status(_ context.Context, c *web.Ctx) error {
	c.JSON(http.StatusOK, map[string]any{
		"workload": WorkloadKey.String(),
		"plugin":   Key.String(),
		"passes":   p.passCount(),
	})
	return nil
}

// HealthChecks implements health.Contributor. The aggregator prefixes the name
// with this plugin's Identity, so the probe reports as "transcode/transcoded".
func (p *Plugin) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Name:    "transcoded",
		Kind:    health.Readiness,
		Timeout: 200 * time.Millisecond,
		Checker: health.CheckFunc(func(context.Context) error {
			if p.passCount() == 0 {
				return fmt.Errorf("transcode: no pass has completed yet")
			}
			return nil
		}),
	}}
}
