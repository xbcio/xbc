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
	plugin.ConfigSpec[Config]{Defaults: func() Config { return Config{} }},
	newClient,
	plugin.Options[*goredis.Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.redis"),
		Inputs:     plugin.Inputs(),
		Exports:    plugin.Contracts[*goredis.Client](),
		Lifecycle: plugin.Lifecycle[*goredis.Client]{
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

// newClient constructs the one primary value owned by a configured Redis
// instance. A failed factory retains ownership and closes the client locally;
// a successful return transfers ownership to XBC's lifecycle.
func newClient(_ plugin.BuildContext, cfg Config) (_ *goredis.Client, err error) {
	client := goredis.NewClient(cfg.options())
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
			return nil, fmt.Errorf("redis: ping %s: %w", cfg.Addr, pingErr)
		}
	}

	committed = true
	return client, nil
}

// stopClient is the typed lifecycle adapter for the third-party primary value.
// go-redis has no context-aware close operation. Treating ErrClosed as success
// makes the adapter safe and idempotent even when called concurrently or before
// the client has opened a connection.
func stopClient(client *goredis.Client, _ context.Context) error {
	if client == nil {
		return nil
	}
	if err := client.Close(); err != nil && !errors.Is(err, goredis.ErrClosed) {
		return err
	}
	return nil
}
