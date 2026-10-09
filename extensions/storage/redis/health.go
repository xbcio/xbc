package redis

import (
	"context"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

// HealthKey is the stable identity of the readiness probe for configured Redis
// clients. It stays a Definition of its own because it aggregates: it collects
// every client instance and reports them under one name, "redis-health". A
// contributor carried by the client Definition would answer only for the single
// instance its primary represents.
const HealthKey plugin.Key = "redis-health"

// healthProbe reports one readiness check per configured Redis instance. It
// collects every client this plugin produced, so a new configured instance is
// probed without touching the composition root.
type healthProbe struct {
	clients []plugin.Entry[*Client]
}

var _ health.Contributor = (*healthProbe)(nil)

var clientsInput = plugin.Collect[*Client]()

var healthDefinition = plugin.Define(
	HealthKey,
	func(ctx plugin.BuildContext) (*healthProbe, error) {
		return newHealthProbe(clientsInput.Get(ctx))
	},
	plugin.Options[*healthProbe]{
		Activation: plugin.WhenConfigured("plugins.redis"),
		Inputs:     plugin.Inputs(clientsInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[health.Contributor](func(value *healthProbe) health.Contributor { return value }),
		),
	},
)

func newHealthProbe(clients []plugin.Entry[*Client]) (*healthProbe, error) {
	for _, entry := range clients {
		if entry.Value == nil {
			return nil, fmt.Errorf("redis: instance %s produced a nil client", entry.Identity)
		}
	}
	return &healthProbe{clients: append([]plugin.Entry[*Client](nil), clients...)}, nil
}

// HealthChecks returns one readiness check per configured instance. The default
// instance contributes an unqualified check so the aggregate name stays
// "redis-health"; a named instance contributes "redis-health/<instance>". Each
// check inherits the timeout configured in plugins.health.
func (p *healthProbe) HealthChecks() []health.NamedChecker {
	checks := make([]health.NamedChecker, 0, len(p.clients))
	for _, entry := range p.clients {
		client := entry.Value
		name := entry.Identity.Normalized().Instance
		if name == plugin.DefaultInstance {
			name = ""
		}
		checks = append(checks, health.NamedChecker{
			Name:    name,
			Kind:    health.Readiness,
			Checker: health.CheckFunc(func(ctx context.Context) error { return pingClient(ctx, client) }),
		})
	}
	return checks
}

// pingClient treats a closed client as down rather than as a probe defect: a
// client closed by shutdown must not be reported as ready. The ping reaches
// whatever topology the instance addresses, so a sentinel-managed or clustered
// instance answers through its own client.
func pingClient(ctx context.Context, client *Client) error {
	if err := client.Ping(ctx).Err(); err != nil {
		if errors.Is(err, goredis.ErrClosed) {
			return fmt.Errorf("redis: client is closed: %w", err)
		}
		return fmt.Errorf("redis: ping: %w", err)
	}
	return nil
}
