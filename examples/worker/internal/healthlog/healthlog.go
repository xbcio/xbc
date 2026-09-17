// Package healthlog answers the question a background service cannot answer
// over HTTP: is this instance ready? It periodically runs the neutral health
// aggregator's readiness probe and writes the result to the log.
//
// It exists to show two things. The health capability is protocol-neutral, so it
// works with no transport adapter selected at all; and a supporting loop belongs
// on Context.Go rather than Context.GoCritical, because reporting is not the
// reason this process exists.
package healthlog

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is healthlog's stable configuration and runtime identity.
const Key plugin.Key = "healthlog"

// Config is bound from plugins.healthlog.
type Config struct {
	Interval time.Duration `yaml:"interval" default:"10s" validate:"gt=0"`
}

// DefaultConfig returns the same defaults XBC's configuration binder applies.
func DefaultConfig() Config {
	return Config{Interval: 10 * time.Second}
}

// Plugin reports readiness on an interval.
type Plugin struct {
	cfg    Config
	probe  *health.Plugin
	logger log.Logger
}

// The aggregator is an ordinary declared input. healthlog must not contribute
// checks of its own: reading the aggregator while also feeding it would be a
// dependency cycle, and assembly rejects those by name.
var probe = plugin.RefTo[*health.Plugin](health.Key)

var _ plugin.Runner = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{Defaults: DefaultConfig},
	func(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
		return &Plugin{cfg: cfg, probe: probe.Get(ctx).Value}, nil
	},
	plugin.Options[*Plugin]{
		Inputs: plugin.Inputs(probe),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns healthlog's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns healthlog's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// Start submits the reporting loop as a non-critical managed task. Context.Go
// deliberately does not satisfy the runtime's long-lived capability
// requirement, so this plugin alone could not keep the process alive -- sweeper
// does that. Losing the reporter should not take the worker down with it.
func (p *Plugin) Start(ctx *plugin.Context) error {
	p.logger = ctx.Log()
	if !ctx.Go(p.run) {
		return errors.New("healthlog: runtime rejected the reporting task")
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
			p.report(taskCtx)
		}
	}
}

// report runs the same probe an HTTP adapter would render. Check applies each
// contributor's own timeout, so this call is already bounded.
func (p *Plugin) report(taskCtx context.Context) {
	result := p.probe.Check(taskCtx, health.Readiness)
	names := make([]string, 0, len(result.Checks))
	for _, check := range result.Checks {
		names = append(names, check.Name+"="+string(check.Status))
	}
	p.logger.Info("healthlog: readiness probe",
		"status", string(result.Status),
		"checks", strings.Join(names, ","),
	)
}
