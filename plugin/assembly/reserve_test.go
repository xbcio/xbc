package assembly

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// deadlineIngressValue is a TrafficOpener whose Stop honours its context the
// way an HTTP server does: it waits for in-flight work until the deadline it
// was handed, then returns. It never violates the plugin contract, which is
// what makes it the fixture for "a well-behaved ingress spends the budget".
type deadlineIngressValue struct {
	events   *drainEvents
	deadline atomic.Pointer[time.Time]
}

func (value *deadlineIngressValue) OpenTraffic(*plugin.Context) error { return nil }
func (value *deadlineIngressValue) Stop(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		value.deadline.Store(&deadline)
	}
	value.events.record("ingress.Stop")
	<-ctx.Done()
	// Acting on the expired context takes a moment -- an HTTP server
	// force-closes the connections that refused to drain -- so the Stop
	// returns shortly after its deadline rather than at the same instant.
	time.Sleep(10 * time.Millisecond)
	return nil
}

func deadlineIngressDefinition(events *drainEvents, value *deadlineIngressValue, dependency plugin.Ref[storeContract]) plugin.Definition {
	return plugin.Define("ingress", func(context plugin.BuildContext) (*deadlineIngressValue, error) {
		_ = dependency.Get(context)
		value.events = events
		return value, nil
	}, plugin.Options[*deadlineIngressValue]{Inputs: plugin.Inputs(dependency)})
}

// stuckIngressValue is a TrafficOpener whose Stop ignores its context
// entirely: the plugin contract violation the walk has to survive.
type stuckIngressValue struct {
	entered chan struct{}
	release chan struct{}
}

func (value *stuckIngressValue) OpenTraffic(*plugin.Context) error { return nil }
func (value *stuckIngressValue) Stop(context.Context) error {
	close(value.entered)
	<-value.release
	return nil
}

// TestStopReserveLeavesTheTailStopsTheirShare pins the reason the reserve
// exists: an ingress whose Stop waits for in-flight work until its deadline
// would otherwise spend the whole walk, and the flush that depends on nothing
// but sits last in reverse order would be reported not-attempted. With the
// reserve the ingress is handed the earlier deadline and the flush runs.
func TestStopReserveLeavesTheTailStopsTheirShare(t *testing.T) {
	t.Parallel()
	const (
		budget  = 600 * time.Millisecond
		reserve = 200 * time.Millisecond
	)
	events := &drainEvents{}
	flush := plainDefinition("flush", events)
	ingress := &deadlineIngressValue{}
	constructed := drainConstructed(t, flush, deadlineIngressDefinition(events, ingress, plugin.RefTo[storeContract]("flush")))

	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	walkEnd, _ := deadline.Deadline()
	report, _, err := constructed.UnwindWithDrain(deadline, budget, 0, reserve, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{"ingress.Stop", "flush.Stop"}, events.recorded(),
		"the flush must still run after an ingress that waited until its deadline")
	assert.Empty(t, report.Identities(StopNotAttempted))
	handed := ingress.deadline.Load()
	require.NotNil(t, handed, "the ingress Stop must be handed a deadline")
	assert.WithinDuration(t, walkEnd.Add(-reserve), *handed, time.Millisecond,
		"phase A runs under the walk's deadline less the reserve")
}

// TestStopReserveOfZeroKeepsTheSharedBudget is the off switch: with no
// reserve the same composition behaves exactly as the walk always did, and
// the ingress that waits until its deadline leaves the flush not-attempted.
// It is also what proves the previous test discriminates.
func TestStopReserveOfZeroKeepsTheSharedBudget(t *testing.T) {
	t.Parallel()
	const budget = 300 * time.Millisecond
	events := &drainEvents{}
	flush := plainDefinition("flush", events)
	ingress := &deadlineIngressValue{}
	constructed := drainConstructed(t, flush, deadlineIngressDefinition(events, ingress, plugin.RefTo[storeContract]("flush")))

	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	report, _, err := constructed.UnwindWithDrain(deadline, budget, 0, 0, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugin flush Stop was not attempted")
	assert.Equal(t, []plugin.Identity{{Plugin: "flush", Instance: plugin.DefaultInstance}},
		report.Identities(StopNotAttempted))
	assert.Equal(t, []string{"ingress.Stop"}, events.recorded())
}

// TestStopReserveRunsTheTailBesideAnAbandonedIngressStop pins the one place
// the reserve relaxes strict ordering: an ingress Stop that ignores its
// context is abandoned at the earlier deadline, and phase C then runs under
// the reserve while that Stop is still in flight -- the same allowance an
// abandoned Drain already makes.
func TestStopReserveRunsTheTailBesideAnAbandonedIngressStop(t *testing.T) {
	t.Parallel()
	const (
		budget  = 400 * time.Millisecond
		reserve = 200 * time.Millisecond
	)
	events := &drainEvents{}
	stuck := &stuckIngressValue{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(stuck.release)
	flushRef := plugin.RefTo[storeContract]("flush")
	ingress := plugin.Define("ingress", func(context plugin.BuildContext) (*stuckIngressValue, error) {
		_ = flushRef.Get(context)
		return stuck, nil
	}, plugin.Options[*stuckIngressValue]{Inputs: plugin.Inputs(flushRef)})
	constructed := drainConstructed(t, plainDefinition("flush", events), ingress)

	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	report, _, err := constructed.UnwindWithDrain(deadline, budget, 0, reserve, nil)
	<-stuck.entered

	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc: plugin ingress Stop did not return within shutdown budget 200ms",
		"phase A's abandonment is reported against the budget it was actually given")
	assert.Equal(t, []plugin.Identity{{Plugin: "ingress", Instance: plugin.DefaultInstance}},
		report.Identities(StopAbandoned))
	assert.Empty(t, report.Identities(StopNotAttempted))
	assert.Equal(t, []string{"flush.Stop"}, events.recorded())
}

// TestStopReserveAlsoBoundsTheDrainPhase pins that phase B shares phase A's
// earlier deadline: a drain_timeout longer than what is left before the
// reserve is cut back to it, so a Drain that waits out its context cannot
// spend the tail either.
func TestStopReserveAlsoBoundsTheDrainPhase(t *testing.T) {
	t.Parallel()
	const (
		budget  = 500 * time.Millisecond
		reserve = 200 * time.Millisecond
	)
	events := &drainEvents{}
	drained := make(chan time.Time, 1)
	drainer := drainOnlyDefinition("drainer", events, nil, func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		drained <- deadline
		<-ctx.Done()
	})
	constructed := drainConstructed(t, plainDefinition("flush", events), drainer)

	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	walkEnd, _ := deadline.Deadline()
	report, drainReport, err := constructed.UnwindWithDrain(deadline, budget, time.Hour, reserve, nil)

	require.NoError(t, err)
	assert.Empty(t, report.Identities(StopNotAttempted))
	require.Len(t, drainReport.Records, 1)
	assert.WithinDuration(t, walkEnd.Add(-reserve), <-drained, time.Millisecond,
		"the drain phase is bounded by the walk's deadline less the reserve")
	assert.Contains(t, events.recorded(), "flush.Stop")
}
