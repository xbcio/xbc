package grpc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPreflightRejectsAPreDrainWindowAtOrPastTheIngressBudget pins the check
// P2 added: grpc.shutdown.pre_drain_delay is held open before the graceful
// stop runs, and both draw on the one ingress share of the shutdown budget
// (xbc.shutdown_timeout less xbc.stop_reserve), so a window that consumes the
// whole share degrades the graceful stop to a forced one. Preflight is the
// path `validate` runs, so reporting the mistake there is what makes it
// visible before a deployment rather than during one.
func TestPreflightRejectsAPreDrainWindowAtOrPastTheIngressBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		delay  time.Duration
	}{
		{name: "equal to the budget", budget: 5 * time.Second, delay: 5 * time.Second},
		{name: "past the budget", budget: 5 * time.Second, delay: 6 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.Shutdown.PreDrainDelay = tc.delay
			s := newServer(cfg, contributions{})
			host := newFakeHost()
			host.ingressBudget = tc.budget

			err := s.Preflight(contextFromHost(host))
			if err == nil {
				t.Fatal("Preflight accepted a pre-drain window that leaves the graceful stop no time")
			}
			for _, want := range []string{
				"grpc.shutdown.pre_drain_delay",
				"ingress stop budget",
				"xbc.shutdown_timeout",
				"xbc.stop_reserve",
				"(" + tc.delay.String() + ")",
				"(" + tc.budget.String() + ")",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestStartRefusesAPreDrainWindowAtOrPastTheIngressBudget pins the boot half
// of the same check: Start assembles through the same path Preflight
// validates, so a window the validate pass refuses must fail the boot -- and
// must fail it before the listener binds, not after.
func TestStartRefusesAPreDrainWindowAtOrPastTheIngressBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = 5 * time.Second
	s := newServer(cfg, contributions{})
	host := newFakeHost()
	host.ingressBudget = 5 * time.Second
	ctx := contextFromHost(host)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(stopCtx)
		host.shutdown()
	})

	err := s.Start(ctx)
	if err == nil {
		t.Fatal("a boot whose pre-drain window consumes the whole ingress share must refuse to start")
	}
	if !strings.Contains(err.Error(), "grpc.shutdown.pre_drain_delay") {
		t.Errorf("error %q does not name the configured key", err)
	}
	if addr := s.Addr(); addr != "" {
		t.Errorf("the refusal must come before the listener binds, but Addr() reports %q", addr)
	}
}

// TestPreflightAcceptsAPreDrainWindowInsideTheIngressBudget pins the accepted
// side of the strict comparison: a window one nanosecond inside the budget is
// exactly the boundary that must still boot, so the check can never drift to
// refusing anything that fits.
func TestPreflightAcceptsAPreDrainWindowInsideTheIngressBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = 5*time.Second - time.Nanosecond
	s := newServer(cfg, contributions{})
	host := newFakeHost()
	host.ingressBudget = 5 * time.Second

	if err := s.Preflight(contextFromHost(host)); err != nil {
		t.Fatalf("Preflight rejected a pre-drain window strictly inside the budget: %v", err)
	}
}

// TestPreflightSkipsThePreDrainCheckWhenTheBudgetIsUnknown pins the documented
// reading of a zero budget: "cannot judge", not "no time". A host that reports
// none -- a Context with no runtime behind it -- must leave the window alone,
// or every harness and embedded caller would be refused for carrying no
// budgets rather than for configuring a bad one.
func TestPreflightSkipsThePreDrainCheckWhenTheBudgetIsUnknown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = time.Hour
	s := newServer(cfg, contributions{})
	host := newFakeHost()
	// host.ingressBudget stays at its zero value.

	if err := s.Preflight(contextFromHost(host)); err != nil {
		t.Fatalf("a zero budget means unknown, so the hour-long window must not be judged against it: %v", err)
	}
}
