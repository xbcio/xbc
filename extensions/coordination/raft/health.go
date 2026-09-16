package raft

import (
	"context"
	"errors"
	"fmt"

	hashiraft "github.com/hashicorp/raft"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

var _ health.Contributor = (*node)(nil)

// HealthChecks contributes cluster progress as a readiness check named "raft".
// This plugin has no Init or Start stage, so nothing else ever verifies that the
// cluster can make progress: a node can be fully alive -- process running,
// transport listening, stores open -- while no leader exists because a partition
// lost the majority or bootstrap never completed. Every Apply and Barrier then
// fails, but only once something calls one.
//
// The check asks whether a leader exists, not whether this node is the leader. A
// follower is a legitimate member that can serve reads and forward writes;
// requiring leadership would report every follower as permanently not ready and
// withdraw the whole cluster except one node from a load balancer.
//
// The check is a process-local read of Raft state and issues no network call, so
// it cannot block: Configuration is deliberately not used because it waits on a
// future, and LeaderCh is deliberately not used because it is edge-triggered and
// reading it would consume an event another caller is waiting for.
func (n *node) HealthChecks() []health.NamedChecker {
	if n == nil {
		return nil
	}
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(n.checkClusterProgress),
	}}
}

func (n *node) checkClusterProgress(ctx context.Context) error {
	if err := n.ready(ctx); err != nil {
		return err
	}
	if state := n.State(); state == hashiraft.Shutdown {
		return errors.New("raft: node is shut down")
	}
	address, id := n.Leader()
	if address == "" || id == "" {
		return fmt.Errorf("raft: no leader elected, node %q is %s", n.ID(), n.State())
	}
	return nil
}
