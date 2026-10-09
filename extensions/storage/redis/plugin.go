package redis

import (
	"context"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of the Redis plugin.
const Key plugin.Key = "redis"

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: func() Config { return Config{} },
		Prepare:  prepareConfig,
	},
	newClient,
	plugin.Options[*Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.redis"),
		Inputs:     plugin.Inputs(),
		Exports: plugin.Contracts(
			plugin.ExportAs[goredis.UniversalClient](func(value *Client) goredis.UniversalClient { return value }),
		),
		Lifecycle: plugin.Lifecycle[*Client]{
			Stop: stopClient,
		},
	},
)

var bundle = plugin.BundleOf(definition, healthDefinition, leaseDefinition)

// Definition returns this integration's canonical Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this integration's side-effect-free composition bundle: the
// client Definition, the readiness probe that reports on its instances, and the
// lease Definition that exports a lease.Locker over one of them.
func Bundle() plugin.Bundle { return bundle }

// Client is the primary value of one configured Redis instance. Which
// topology it addresses -- one server, a sentinel-managed master, or a cluster
// -- is fixed by the instance's configuration, and the wrapper is what lets
// that choice be invisible to consumers: a Definition's primary must be a
// concrete type, while consumers depend on the topology-neutral
// goredis.UniversalClient contract this type satisfies.
//
// The wrapper carries no state of its own. Every method is the embedded
// client's, so a consumer that resolved goredis.UniversalClient and one that
// holds a *Client address the same connection pool.
type Client struct {
	goredis.UniversalClient
}

// clientFactory builds the concrete client for one prepared configuration.
// The constructors are fields rather than direct calls because the sentinel
// path needs a live sentinel topology to exercise end to end: a test swaps a
// field to observe the options the path would have been handed.
//
// failover and failoverCluster are separate because go-redis panics when
// RouteByLatency or RouteRandomly reach NewFailoverClient -- replica routing
// exists only on the sentinel-backed cluster client -- so the choice between
// them follows the route flags rather than a retry.
type clientFactory struct {
	standalone      func(*goredis.Options) goredis.UniversalClient
	failover        func(*goredis.FailoverOptions) goredis.UniversalClient
	failoverCluster func(*goredis.FailoverOptions) goredis.UniversalClient
	cluster         func(*goredis.ClusterOptions) goredis.UniversalClient
}

func defaultClientFactory() clientFactory {
	return clientFactory{
		standalone: func(options *goredis.Options) goredis.UniversalClient {
			return goredis.NewClient(options)
		},
		failover: func(options *goredis.FailoverOptions) goredis.UniversalClient {
			return goredis.NewFailoverClient(options)
		},
		failoverCluster: func(options *goredis.FailoverOptions) goredis.UniversalClient {
			return goredis.NewFailoverClusterClient(options)
		},
		cluster: func(options *goredis.ClusterOptions) goredis.UniversalClient {
			return goredis.NewClusterClient(options)
		},
	}
}

// build selects the constructor the prepared configuration names. The explicit
// per-mode constructors are deliberate: goredis.NewUniversalClient picks a
// client type from which option fields happen to be set, so a section that
// named both a master and several seeds would resolve by heuristic instead of
// by its declared mode.
func (f clientFactory) build(cfg Config) goredis.UniversalClient {
	switch cfg.Mode {
	case ModeSentinel:
		options := cfg.failoverOptions()
		if cfg.RouteByLatency || cfg.RouteRandomly {
			return f.failoverCluster(options)
		}
		return f.failover(options)
	case ModeCluster:
		return f.cluster(cfg.clusterOptions())
	default:
		// prepareConfig admits only the three modes, so the remaining case is
		// standalone.
		return f.standalone(cfg.options())
	}
}

// newClient constructs the one primary value owned by a configured Redis
// instance. A failed factory retains ownership and closes the client locally;
// a successful return transfers ownership to XBC's lifecycle.
func newClient(_ plugin.BuildContext, cfg Config) (*Client, error) {
	return buildClient(defaultClientFactory(), cfg)
}

// buildClient is the construction path with its factory supplied, so a test can
// drive it without a live topology of the mode under test.
func buildClient(factory clientFactory, cfg Config) (_ *Client, err error) {
	client := &Client{UniversalClient: factory.build(cfg)}
	committed := false
	defer func() {
		if committed {
			return
		}
		if closeErr := stopClient(client, context.Background()); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("redis: close client after construction failure: %w", closeErr))
		}
	}()

	if cfg.Ping {
		if pingErr := client.Ping(context.Background()).Err(); pingErr != nil {
			return nil, fmt.Errorf("redis: ping %s: %w", cfg.topology(), pingErr)
		}
	}

	committed = true
	return client, nil
}

// stopClient is the typed lifecycle adapter for the primary value. go-redis has
// no context-aware close operation. Treating ErrClosed as success makes the
// adapter safe and idempotent even when called concurrently or before the
// client has opened a connection.
func stopClient(client *Client, _ context.Context) error {
	if client == nil || client.UniversalClient == nil {
		return nil
	}
	if err := client.UniversalClient.Close(); err != nil && !errors.Is(err, goredis.ErrClosed) {
		return err
	}
	return nil
}
