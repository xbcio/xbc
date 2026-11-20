package redisx

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenConstructsAConfiguredClient covers the direct-construction contract:
// the exported constructor pings the server, returns the topology-neutral
// client, and applies the configured timeout and pool bounds.
func TestOpenConstructsAConfiguredClient(t *testing.T) {
	server := miniredis.RunT(t)
	cfg := preparedConfig(t, map[string]any{
		"addr":               server.Addr(),
		"dial_timeout":       "250ms",
		"read_timeout":       "300ms",
		"write_timeout":      "350ms",
		"pool_timeout":       "400ms",
		"pool_size":          4,
		"min_idle_conns":     1,
		"max_idle_conns":     2,
		"max_active_conns":   6,
		"conn_max_idle_time": "2m",
		"conn_max_lifetime":  "5m",
		"max_retries":        1,
	})

	client, err := Open(cfg)
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	if server.CommandCount() == 0 {
		t.Fatal("Open sent no Redis command; the default ping did not validate the connection")
	}

	standalone, ok := client.(*goredis.Client)
	require.True(t, ok, "standalone Open returned %T, want *goredis.Client", client)
	options := standalone.Options()
	assert.Equal(t, cfg.Addr, options.Addr)
	assert.Equal(t, cfg.DialTimeout, options.DialTimeout)
	assert.Equal(t, cfg.ReadTimeout, options.ReadTimeout)
	assert.Equal(t, cfg.WriteTimeout, options.WriteTimeout)
	assert.Equal(t, cfg.PoolTimeout, options.PoolTimeout)
	assert.Equal(t, cfg.PoolSize, options.PoolSize)
	assert.Equal(t, cfg.MinIdleConns, options.MinIdleConns)
	assert.Equal(t, cfg.MaxIdleConns, options.MaxIdleConns)
	assert.Equal(t, cfg.MaxActiveConns, options.MaxActiveConns)
	assert.Equal(t, cfg.ConnMaxIdleTime, options.ConnMaxIdleTime)
	assert.Equal(t, cfg.ConnMaxLifetime, options.ConnMaxLifetime)
	assert.Equal(t, cfg.MaxRetries, options.MaxRetries)

	require.NoError(t, client.Set(context.Background(), "opened", "direct", 0).Err())
	value, err := server.Get("opened")
	require.NoError(t, err)
	assert.Equal(t, "direct", value)
}

// TestOpenReturnsAnErrorForAnUnreachableAddress proves the startup ping fails
// construction instead of handing back a client that cannot connect.
func TestOpenReturnsAnErrorForAnUnreachableAddress(t *testing.T) {
	server := miniredis.RunT(t)
	address := server.Addr()
	server.Close()

	cfg := preparedConfig(t, map[string]any{
		"addr":          address,
		"dial_timeout":  "100ms",
		"read_timeout":  "100ms",
		"write_timeout": "100ms",
		"pool_timeout":  "100ms",
		"max_retries":   -1,
	})

	client, err := Open(cfg)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.True(t, strings.Contains(err.Error(), "redis: ping "+address), "error = %v, want the unreachable address named", err)
}

// TestOpenRejectsAnInvalidConfig proves the exported path keeps the mode
// validation the plugin performs during planning rather than silently falling
// back to standalone.
func TestOpenRejectsAnInvalidConfig(t *testing.T) {
	client, err := Open(Config{})
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "unknown mode")
}
