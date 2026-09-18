// Package ingest is the workloads example's co-resident workload.
//
// A workload is a named group of Definitions a process carries as a unit or not
// at all. This one is deliberately ordinary: it declares no placement
// constraint beyond its replica count, which is what makes it co-resident. A
// process may carry it together with any other non-exclusive workload, and the
// two share the process's resources under their own
// workloads.<key>.max_goroutines budgets.
//
// The distinction from transcode is the whole point of the example. Exclusion
// is for a workload with a process-wide side effect, not for a workload that is
// merely busy: ingest is the busier of the two here, and it still shares its
// process, because nothing it does reaches outside itself.
package ingest

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

// Key is ingest's stable plugin configuration and runtime identity. It owns the
// plugins.ingest section.
const Key plugin.Key = "ingest"

// WorkloadKey is the placement identity of the workload ingest belongs to. It
// owns the workloads.ingest section.
const WorkloadKey plugin.WorkloadKey = "ingest"

// Config is bound from plugins.ingest.
type Config struct {
	// Interval is how often an ingest batch is admitted while this process
	// carries the workload.
	Interval time.Duration `yaml:"interval" default:"500ms" validate:"gt=0"`

	// BatchSize is how many records one admitted batch claims.
	BatchSize int `yaml:"batch_size" default:"100" validate:"gt=0"`
}

// DefaultConfig returns the same defaults XBC's configuration binder applies.
func DefaultConfig() Config {
	return Config{Interval: 500 * time.Millisecond, BatchSize: 100}
}

// Plugin admits one batch per tick and answers a status route.
type Plugin struct {
	cfg    Config
	logger log.Logger

	mu      sync.Mutex
	batches int
	records int
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

// bundle tags this package's Definitions with the ingest workload.
//
// WithReplicas is the slot count: four processes may carry this workload at
// once. It is a cluster-level promise rather than a per-process preference, so
// it is not configurable -- a host that could reinterpret "how many replicas may
// run" would silently invalidate the placement every other process derived from
// the same declaration.
var bundle = plugin.WorkloadOf(
	WorkloadKey,
	plugin.BundleOf(definition),
	plugin.WithReplicas(4),
)

// Definition returns ingest's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this workload's side-effect-free composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// Start submits the ingest loop as one critical managed task, for the same
// reason transcode does: this loop is what a process carrying the workload is
// for.
func (p *Plugin) Start(ctx *plugin.Context) error {
	p.logger = ctx.Log()
	gate := ctx.TrafficGate()
	if !ctx.GoCritical(func(taskCtx context.Context) { p.run(taskCtx, gate) }) {
		return errors.New("ingest: runtime rejected the ingest task")
	}
	return nil
}

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
			return
		case <-ticker.C:
			p.batch()
		}
	}
}

func (p *Plugin) batch() {
	p.mu.Lock()
	p.batches++
	p.records += p.cfg.BatchSize
	batches, records := p.batches, p.records
	p.mu.Unlock()
	p.logger.Info("ingest: admitted a batch", "batches", batches, "records", records)
}

// Stop runs in reverse graph order under the shared shutdown budget, after the
// managed task has already been canceled and joined.
func (p *Plugin) Stop(context.Context) error {
	batches, records := p.totals()
	p.logger.Info("ingest: stopped", "batches", batches, "records", records)
	return nil
}

func (p *Plugin) totals() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batches, p.records
}

// RegisterRoutes implements web.RouteContributor.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/ingest/status", p.status).
		Name("ingest.status").
		Auth(web.Public())
}

func (p *Plugin) status(_ context.Context, c *web.Ctx) error {
	batches, records := p.totals()
	c.JSON(http.StatusOK, map[string]any{
		"workload": WorkloadKey.String(),
		"plugin":   Key.String(),
		"batches":  batches,
		"records":  records,
	})
	return nil
}

// HealthChecks implements health.Contributor. The aggregator prefixes the name
// with this plugin's Identity, so the probe reports as "ingest/ingested".
func (p *Plugin) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Name:    "ingested",
		Kind:    health.Readiness,
		Timeout: 200 * time.Millisecond,
		Checker: health.CheckFunc(func(context.Context) error {
			batches, _ := p.totals()
			if batches == 0 {
				return fmt.Errorf("ingest: no batch has been admitted yet")
			}
			return nil
		}),
	}}
}
