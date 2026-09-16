package elasticsearch

import (
	"github.com/xbcio/xbc/extensions/reliability/health"
)

var _ health.Contributor = (*Client)(nil)

// HealthChecks contributes this cluster's reachability as a readiness check.
// The primary value carries the contract itself, so each configured instance
// contributes its own check under its own identity: "elasticsearch" for the
// default instance and "elasticsearch[<instance>]" for a named one.
//
// An instance configured with health_probe: false contributes nothing. That
// flag is the operator's statement that this cluster's reachability must not
// decide whether the process serves traffic, and Init already honours it; one
// flag with two meanings would be worse than no contribution.
//
// The check inherits the timeout configured in plugins.health, and Do bounds it
// again by this instance's own timeout, so the earlier of the two applies.
func (client *Client) HealthChecks() []health.NamedChecker {
	if client == nil || !client.healthProbeEnabled() {
		return nil
	}
	// A closed client makes Health return ErrClientClosed, which is reported as
	// down rather than as a probe defect: a client closed by shutdown must not
	// be reported as ready.
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(client.Health),
	}}
}

func (client *Client) healthProbeEnabled() bool {
	client.lifecycleMu.Lock()
	defer client.lifecycleMu.Unlock()
	return client.healthProbe
}
