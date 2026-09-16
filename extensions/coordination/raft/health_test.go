package raft

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

func TestHealthChecksContributeOneReadinessCheck(t *testing.T) {
	n := newTestNode(t, fastConfig("health-leader", true), nil)
	waitForLeader(t, n)

	checks := n.HealthChecks()
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(checks))
	}
	if checks[0].Name != "" {
		t.Fatalf("the check must be named by the contributing instance, got %q", checks[0].Name)
	}
	if checks[0].Kind != health.Readiness {
		t.Fatalf("got kind %q, want readiness", checks[0].Kind)
	}
	if checks[0].Timeout != 0 {
		t.Fatalf("the check must inherit the plugins.health timeout, got %s", checks[0].Timeout)
	}

	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	if !report.Healthy() {
		t.Fatalf("a node in a cluster with a leader must report up, got %+v", report)
	}
}

// A node that requested no bootstrap and has no peers can never elect a leader,
// so it can never make progress. That is the failure this check exists for: the
// node is otherwise fully alive and nothing else reports the problem.
func TestHealthCheckReportsDownWithoutALeader(t *testing.T) {
	n := newTestNode(t, fastConfig("health-no-leader", false), nil)

	report := health.Check(context.Background(), health.Readiness, n.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a node without a leader must report down, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Error == nil {
		t.Fatalf("expected one failed check, got %+v", report.Checks)
	}
	if message := report.Checks[0].Error.Error(); !strings.Contains(message, "no leader elected") {
		t.Fatalf("got error %q, want it to name the missing leader", message)
	}
}

func TestHealthCheckReportsDownAfterStop(t *testing.T) {
	n := newTestNode(t, fastConfig("health-stopped", true), nil)
	waitForLeader(t, n)
	if err := n.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	report := health.Check(context.Background(), health.Readiness, n.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a stopped node must report down, got %+v", report)
	}
	if err := report.Checks[0].Error; !errors.Is(err, ErrStopped) {
		t.Fatalf("got error %v, want ErrStopped", err)
	}
}

// A follower is a legitimate cluster member: readiness asks whether a leader
// exists, not whether this node is it, so a follower must not be withdrawn from
// service.
func TestHealthCheckReportsUpForAFollower(t *testing.T) {
	leaderAddress := reserveTCPAddress(t)
	leaderConfig := fastConfig("health-cluster-leader", true)
	leaderConfig.BindAddr = leaderAddress
	leader := newTestNode(t, leaderConfig, nil)
	waitForLeader(t, leader)

	followerAddress := reserveTCPAddress(t)
	followerConfig := fastConfig("health-cluster-follower", false)
	followerConfig.BindAddr = followerAddress
	follower := newTestNode(t, followerConfig, nil)

	if err := leader.Join(context.Background(), follower.ID(), follower.Address()); err != nil {
		t.Fatalf("join follower: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		report := health.Check(context.Background(), health.Readiness, follower.HealthChecks(), time.Second)
		if report.Status == health.Up {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the follower never reported up: %+v", report)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
