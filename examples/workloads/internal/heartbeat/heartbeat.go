// Package heartbeat is the workloads example's unowned plugin: it belongs to no
// workload, so every process shape carries it.
//
// It is here to make the difference visible rather than merely stated. A
// workload's Definitions exist only in a process that carries it, but a
// transport, a health capability and an application plugin like this one are
// outside the placement question entirely. Run the example as the exclusive
// role, as the co-resident role, or as a standby hosting neither, and this
// route answers in all three while the workload routes answer in one.
package heartbeat

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

// Key is heartbeat's stable plugin configuration and runtime identity.
const Key plugin.Key = "heartbeat"

// Config is bound from plugins.heartbeat.
type Config struct {
	// Interval is how often the heartbeat is recorded.
	Interval time.Duration `yaml:"interval" default:"3s" validate:"gt=0"`
}

// DefaultConfig returns the same defaults XBC's configuration binder applies.
func DefaultConfig() Config {
	return Config{Interval: 3 * time.Second}
}

// Plugin records a heartbeat on an interval and answers a status route.
type Plugin struct {
	cfg    Config
	logger log.Logger

	mu        sync.Mutex
	beats     int
	lastBeat  time.Time
	startedAt time.Time
}

var (
	_ web.RouteContributor = (*Plugin)(nil)
	_ health.Contributor   = (*Plugin)(nil)
	_ plugin.Runner        = (*Plugin)(nil)
	_ plugin.Closer        = (*Plugin)(nil)
)

// definition declares no workload membership. It is a plain BundleOf member, so
// it enters every process's plan whatever the placement decided: an unowned
// plugin answers for itself rather than for a role.
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

var bundle = plugin.BundleOf(definition)

// Definition returns heartbeat's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns heartbeat's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// Start submits the heartbeat loop on Context.Go rather than GoCritical.
//
// Reporting is not the reason this process exists, and only a critical task
// counts as the long-lived capability the runtime requires. Go is the right
// choice for a supporting loop that may legitimately return: losing the
// reporter should not take the process down with it. In this example the
// process is kept alive by whichever workload it carries -- and, in the standby
// role, by the Web transport's serving task.
func (p *Plugin) Start(ctx *plugin.Context) error {
	p.logger = ctx.Log()
	p.startedAt = time.Now()
	if !ctx.Go(p.run) {
		return errors.New("heartbeat: runtime rejected the heartbeat task")
	}
	return nil
}

func (p *Plugin) run(taskCtx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-taskCtx.Done():
			return
		case <-ticker.C:
			p.beat()
		}
	}
}

func (p *Plugin) beat() {
	p.mu.Lock()
	p.beats++
	p.lastBeat = time.Now()
	beats := p.beats
	p.mu.Unlock()
	p.logger.Info("heartbeat: alive", "beats", beats)
}

// Stop runs in reverse graph order under the shared shutdown budget, after the
// managed task has already been canceled and joined.
func (p *Plugin) Stop(context.Context) error {
	p.logger.Info("heartbeat: stopped", "beats", p.snapshot().beats)
	return nil
}

type beatSnapshot struct {
	beats     int
	lastBeat  time.Time
	startedAt time.Time
}

func (p *Plugin) snapshot() beatSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return beatSnapshot{beats: p.beats, lastBeat: p.lastBeat, startedAt: p.startedAt}
}

// RegisterRoutes implements web.RouteContributor.
//
// This route exists in every role, which is exactly what makes it the control in
// the experiment: a 404 from /api/v1/transcode/status beside a 200 from
// /api/v1/heartbeat is the hosted set doing its job.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/heartbeat", p.status).
		Name("heartbeat.status").
		Auth(web.Public())
}

func (p *Plugin) status(_ context.Context, c *web.Ctx) error {
	snapshot := p.snapshot()
	payload := map[string]any{
		"plugin": Key.String(),
		"beats":  snapshot.beats,
	}
	if !snapshot.lastBeat.IsZero() {
		payload["last_beat"] = snapshot.lastBeat.Format(time.RFC3339Nano)
	}
	if !snapshot.startedAt.IsZero() {
		payload["uptime"] = time.Since(snapshot.startedAt).Round(time.Second).String()
	}
	c.JSON(http.StatusOK, payload)
	return nil
}

// HealthChecks implements health.Contributor. The aggregator prefixes the name
// with this plugin's Identity, so the probe reports as "heartbeat/beating".
//
// A heartbeat is a dumb dependency-free liveness signal, so this is the one
// probe that is meaningful in every role -- including the standby, which carries
// no workload and would otherwise contribute no readiness check at all. It is
// "has this plugin started", not "is the process doing its job": the workload
// probes answer that second question, and answering it here would make the
// standby permanently not-ready when being idle is exactly its job.
func (p *Plugin) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Name:    "beating",
		Kind:    health.Readiness,
		Timeout: 200 * time.Millisecond,
		Checker: health.CheckFunc(func(context.Context) error {
			if p.snapshot().startedAt.IsZero() {
				return fmt.Errorf("heartbeat: not started")
			}
			return nil
		}),
	}}
}
