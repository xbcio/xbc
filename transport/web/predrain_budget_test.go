package web_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// TestPreflightRejectsAPreDrainWindowAtOrPastTheIngressBudget pins the check
// P2 added: web.shutdown.pre_drain_delay is held open before the graceful stop
// runs, and both draw on the one ingress share of the shutdown budget
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
			cfg := web.DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.Shutdown.PreDrainDelay = tc.delay
			server, ctx, host := newPingServer(t, cfg, serverInputs{})
			host.ingressBudget = tc.budget

			err := server.Preflight(ctx)
			require.Error(t, err,
				"a pre-drain window that leaves the graceful stop no time must fail validation")
			assert.ErrorContains(t, err, "web.shutdown.pre_drain_delay")
			assert.ErrorContains(t, err, "ingress stop budget")
			assert.ErrorContains(t, err, "xbc.shutdown_timeout")
			assert.ErrorContains(t, err, "xbc.stop_reserve",
				"the error names the three keys that decide the budget")
			assert.ErrorContains(t, err, "("+tc.delay.String()+")",
				"the error states the configured window")
			assert.ErrorContains(t, err, "("+tc.budget.String()+")",
				"the error states the budget it was compared against")
		})
	}
}

// TestStartRefusesAPreDrainWindowAtOrPastTheIngressBudget pins the boot half
// of the same check: Start assembles through the same path Preflight
// validates, so a window the validate pass refuses must fail the boot -- and
// must fail it before the listener binds, not after.
func TestStartRefusesAPreDrainWindowAtOrPastTheIngressBudget(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = 5 * time.Second
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	host.ingressBudget = 5 * time.Second

	err := server.Start(ctx)
	require.Error(t, err,
		"a boot whose pre-drain window consumes the whole ingress share must refuse to start")
	assert.ErrorContains(t, err, "web.shutdown.pre_drain_delay")
	assert.Empty(t, server.Addr(),
		"the refusal must come before the listener binds")
}

// TestPreflightAcceptsAPreDrainWindowInsideTheIngressBudget pins the accepted
// side of the strict comparison: a window one nanosecond inside the budget is
// exactly the boundary that must still boot, so the check can never drift to
// refusing anything that fits.
func TestPreflightAcceptsAPreDrainWindowInsideTheIngressBudget(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = 5*time.Second - time.Nanosecond
	server, ctx, host := newPingServer(t, cfg, serverInputs{})
	host.ingressBudget = 5 * time.Second

	require.NoError(t, server.Preflight(ctx),
		"a pre-drain window strictly inside the budget must validate")
}

// TestPreflightSkipsThePreDrainCheckWhenTheBudgetIsUnknown pins the documented
// reading of a zero budget: "cannot judge", not "no time". A host that reports
// none -- a Context with no runtime behind it -- must leave the window alone,
// or every harness and embedded caller would be refused for carrying no
// budgets rather than for configuring a bad one.
func TestPreflightSkipsThePreDrainCheckWhenTheBudgetIsUnknown(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Shutdown.PreDrainDelay = time.Hour
	server, ctx, _ := newPingServer(t, cfg, serverInputs{})
	// host.ingressBudget stays at its zero value.

	require.NoError(t, server.Preflight(ctx),
		"a zero budget means unknown, so the hour-long window must not be judged against it")
}
