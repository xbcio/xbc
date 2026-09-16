package kafka

import (
	"context"
	"errors"
	"fmt"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

var _ health.Contributor = (*Client)(nil)

// HealthChecks contributes broker reachability as a readiness check. Each
// configured instance contributes under its own identity: "kafka" for the default
// instance and "kafka[<instance>]" for a named one. The contribution is inert
// unless the application also selects the health capability Bundle.
//
// Nothing else in this plugin observes the cluster: the writer batches lazily and
// consumer loops log their own failures, so an unreachable or unauthenticated
// cluster stays invisible until a Produce call fails. The check asks a broker for
// cluster metadata instead, which costs one round trip and writes nothing.
func (c *Client) HealthChecks() []health.NamedChecker {
	if c == nil {
		return nil
	}
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(c.pingCluster),
	}}
}

// pingCluster admits the probe exactly as Produce does, so a client closed by
// shutdown reports down rather than racing the writer it no longer owns.
func (c *Client) pingCluster(ctx context.Context) error {
	if ctx == nil {
		return errors.New("kafka: readiness probe requires a non-nil context")
	}
	if c.closing.Load() {
		return ErrClosed
	}
	c.produceMu.RLock()
	defer c.produceMu.RUnlock()
	if c.closing.Load() || c.closed || c.writer == nil {
		return ErrClosed
	}
	if err := c.writer.Ping(ctx); err != nil {
		return fmt.Errorf("kafka: reach cluster: %w", err)
	}
	return nil
}
