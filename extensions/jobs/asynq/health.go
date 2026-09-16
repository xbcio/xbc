package asynq

import (
	"context"
	"errors"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

var _ health.Contributor = (*Plugin)(nil)

// HealthChecks contributes the reachability of the Redis instance this plugin
// owns as a readiness check. The connection is created and pinged once by Init;
// without a readiness check, losing it afterwards stays invisible until an
// Enqueue call fails or the worker silently stops consuming.
//
// The primary value carries the contract, so the aggregate report names the
// check after this plugin's identity, "asynq". The check inherits the timeout
// configured in plugins.health, and the contribution is inert unless the
// application also selects the health capability Bundle.
func (p *Plugin) HealthChecks() []health.NamedChecker {
	if p == nil {
		return nil
	}
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(p.pingRedis),
	}}
}

// pingRedis reports a plugin that has not initialized, or has already stopped,
// as down rather than as a probe defect: neither state can accept or consume a
// task, so readiness must not claim otherwise.
func (p *Plugin) pingRedis(ctx context.Context) error {
	p.mu.Lock()
	client := p.redis
	address := p.cfg.Redis.Addr
	stopped := p.stopping || p.stopped
	p.mu.Unlock()

	switch {
	case client == nil && stopped:
		return errors.New("asynq: plugin has stopped")
	case client == nil:
		return errors.New("asynq: plugin is not initialized")
	}
	if err := client.Ping(ctx).Err(); err != nil {
		if errors.Is(err, goredis.ErrClosed) {
			return fmt.Errorf("asynq: Redis connection is closed: %w", err)
		}
		return fmt.Errorf("asynq: ping Redis at %s: %w", address, err)
	}
	return nil
}
