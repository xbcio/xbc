package gorm

import (
	"context"
	"fmt"

	gormlib "gorm.io/gorm"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

// HealthKey is the stable identity of the readiness probe for configured GORM
// databases. It is a second Definition rather than a contract on the primary
// value because this plugin's primary is *gormlib.DB: a third-party type cannot
// be given a method, so it cannot implement health.Contributor itself.
const HealthKey plugin.Key = "gorm-health"

// healthProbe reports one readiness check per configured database. It collects
// every database this plugin produced, so a new configured instance is probed
// without touching the composition root.
type healthProbe struct {
	databases []plugin.Entry[*gormlib.DB]
}

var _ health.Contributor = (*healthProbe)(nil)

var databasesInput = plugin.Collect[*gormlib.DB]()

var healthDefinition = plugin.Define(
	HealthKey,
	func(ctx plugin.BuildContext) (*healthProbe, error) {
		return newHealthProbe(databasesInput.Get(ctx))
	},
	plugin.Options[*healthProbe]{
		Activation: plugin.WhenConfigured("plugins.gorm"),
		Inputs:     plugin.Inputs(databasesInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[health.Contributor](func(value *healthProbe) health.Contributor { return value }),
		),
	},
)

func newHealthProbe(databases []plugin.Entry[*gormlib.DB]) (*healthProbe, error) {
	for _, entry := range databases {
		if entry.Value == nil {
			return nil, fmt.Errorf("xbc gorm: instance %s produced a nil database", entry.Identity)
		}
	}
	return &healthProbe{databases: append([]plugin.Entry[*gormlib.DB](nil), databases...)}, nil
}

// HealthChecks returns one readiness check per configured instance. The default
// instance contributes an unqualified check so the aggregate name stays
// "gorm-health"; a named instance contributes "gorm-health/<instance>". Each
// check inherits the timeout configured in plugins.health.
func (p *healthProbe) HealthChecks() []health.NamedChecker {
	checks := make([]health.NamedChecker, 0, len(p.databases))
	for _, entry := range p.databases {
		db := entry.Value
		name := entry.Identity.Normalized().Instance
		if name == plugin.DefaultInstance {
			name = ""
		}
		checks = append(checks, health.NamedChecker{
			Name:    name,
			Kind:    health.Readiness,
			Checker: health.CheckFunc(func(ctx context.Context) error { return pingDatabase(ctx, db) }),
		})
	}
	return checks
}

// pingDatabase probes the pool rather than the GORM session. A pool closed by
// shutdown reports down instead of ready, and a pool that cannot be obtained is
// reported with the same driver label used by the construction errors.
func pingDatabase(ctx context.Context, db *gormlib.DB) error {
	driver := databaseDriver(db)
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("xbc gorm: obtain %s connection pool: %w", driver, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("xbc gorm: ping %s database: %w", driver, err)
	}
	return nil
}
