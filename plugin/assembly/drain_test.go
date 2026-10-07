package assembly

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// drainEvents records what happened and when, so an ordering assertion reads
// off one slice instead of several independent probes that each have to be
// reconciled by hand.
type drainEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *drainEvents) record(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *drainEvents) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// ingressValue is a TrafficOpener: its membership in Unwind's ingress closure
// comes from implementing plugin.TrafficOpener directly, exactly as
// web.Server's does in production. stopCost is what its Stop charges phase A.
type ingressValue struct {
	name     string
	events   *drainEvents
	stopCost time.Duration
}

func (value *ingressValue) Name() string                      { return value.name }
func (value *ingressValue) OpenTraffic(*plugin.Context) error { return nil }
func (value *ingressValue) Stop(context.Context) error {
	value.events.record(value.name + ".Stop")
	time.Sleep(value.stopCost)
	return nil
}

func ingressDefinition(key plugin.Key, events *drainEvents, dependencyRef plugin.Ref[storeContract]) plugin.Definition {
	return plugin.Define(key, func(context plugin.BuildContext) (*ingressValue, error) {
		_ = dependencyRef.Get(context)
		return &ingressValue{name: key.String(), events: events}, nil
	}, plugin.Options[*ingressValue]{
		Inputs: plugin.Inputs(dependencyRef),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(value *ingressValue) storeContract { return value }),
		),
	})
}

// slowIngressDefinition is ingressDefinition with a Stop that costs stopCost
// and keeps the rest of its shape identical, so a test can make phase A
// outlast the drain phase's own budget without disturbing its fixture.
func slowIngressDefinition(key plugin.Key, events *drainEvents, dependencyRef plugin.Ref[storeContract], stopCost time.Duration) plugin.Definition {
	return plugin.Define(key, func(context plugin.BuildContext) (*ingressValue, error) {
		_ = dependencyRef.Get(context)
		return &ingressValue{name: key.String(), events: events, stopCost: stopCost}, nil
	}, plugin.Options[*ingressValue]{
		Inputs: plugin.Inputs(dependencyRef),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(value *ingressValue) storeContract { return value }),
		),
	})
}

// plainStopValue is an ordinary dependency-chain member: it stops, exports
// storeContract so something else may depend on it, and declares no Drain
// hook.
type plainStopValue struct {
	name   string
	events *drainEvents
}

func (value *plainStopValue) Name() string { return value.name }
func (value *plainStopValue) Stop(context.Context) error {
	value.events.record(value.name + ".Stop")
	return nil
}

func plainDefinition(key plugin.Key, events *drainEvents) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*plainStopValue, error) {
		return &plainStopValue{name: key.String(), events: events}, nil
	}, plugin.Options[*plainStopValue]{
		Exports: plugin.Contracts(
			plugin.ExportAs(func(value *plainStopValue) storeContract { return value }),
		),
	})
}

// plainDependentDefinition is plainDefinition with one Ref dependency, used to
// build the handler->db and dependent->ingress edges.
func plainDependentDefinition(key plugin.Key, events *drainEvents, dependency plugin.Ref[storeContract]) plugin.Definition {
	return plugin.Define(key, func(context plugin.BuildContext) (*plainStopValue, error) {
		_ = dependency.Get(context)
		return &plainStopValue{name: key.String(), events: events}, nil
	}, plugin.Options[*plainStopValue]{
		Inputs: plugin.Inputs(dependency),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(value *plainStopValue) storeContract { return value }),
		),
	})
}

// drainValue both drains and stops, recording both so ordering tests can see
// Drain happen strictly before this instance's own Stop.
type drainValue struct {
	name       string
	events     *drainEvents
	block      chan struct{}
	drainFn    func(context.Context)
	drainErr   error
	drainPanic bool
}

func (value *drainValue) Name() string { return value.name }

func (value *drainValue) Drain(ctx context.Context) error {
	value.events.record(value.name + ".Drain")
	if value.drainFn != nil {
		value.drainFn(ctx)
	}
	if value.block != nil {
		<-ctx.Done()
	}
	if value.drainPanic {
		panic("drain boom")
	}
	return value.drainErr
}

func (value *drainValue) Stop(context.Context) error {
	value.events.record(value.name + ".Stop")
	return nil
}

func drainDefinition(key plugin.Key, events *drainEvents, block chan struct{}, drainFn func(context.Context)) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*drainValue, error) {
		return &drainValue{name: key.String(), events: events, block: block, drainFn: drainFn}, nil
	}, plugin.Options[*drainValue]{})
}

func drainFailureDefinition(key plugin.Key, events *drainEvents, err error, panics bool) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*drainValue, error) {
		return &drainValue{name: key.String(), events: events, drainErr: err, drainPanic: panics}, nil
	}, plugin.Options[*drainValue]{})
}

// drainOnlyValue declares a Drain hook and nothing else: no Stop, no
// contract. It exists so a test can assert that an instance whose only
// lifecycle action was the drain is reported StopSkipped rather than
// StopCompleted.
type drainOnlyValue struct {
	name    string
	events  *drainEvents
	block   chan struct{}
	drainFn func(context.Context)
}

func (value *drainOnlyValue) Drain(ctx context.Context) error {
	value.events.record(value.name + ".Drain")
	if value.drainFn != nil {
		value.drainFn(ctx)
	}
	if value.block != nil {
		<-value.block
	}
	return nil
}

func drainOnlyDefinition(key plugin.Key, events *drainEvents, block chan struct{}, drainFn func(context.Context)) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*drainOnlyValue, error) {
		return &drainOnlyValue{name: key.String(), events: events, block: block, drainFn: drainFn}, nil
	}, plugin.Options[*drainOnlyValue]{})
}

// drainConstructed builds and constructs a plan from definitions, failing the
// test immediately on any error so every test below can stay to the ordering
// assertion it exists for.
func drainConstructed(t *testing.T, definitions ...plugin.Definition) *Constructed {
	t.Helper()
	plan, err := planFor(t, nil, definitions...)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	return constructed
}

// TestUnwindWithDrainOrdersIngressThenDrainThenRemainingStops pins the whole
// three-phase contract in one assertion. The graph is:
//
//	ingress  (TrafficOpener)      depends on handler
//	handler                       depends on db
//	db
//	dependent                     depends on ingress
//	pool     (independent drainer, no dependency on anything above)
//
// The ingress closure is {ingress, dependent}: dependent transitively depends
// on a TrafficOpener, so it is upward-closed into phase A even though it is
// not itself a TrafficOpener. db, handler and pool are not in the closure, so
// they are stopped (and pool is drained) in the two phases that follow.
func TestUnwindWithDrainOrdersIngressThenDrainThenRemainingStops(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}

	db := plainDefinition("db", events)
	dbRef := plugin.RefTo[storeContract]("db")
	handler := plainDependentDefinition("handler", events, dbRef)
	handlerRef := plugin.RefTo[storeContract]("handler")
	ingress := ingressDefinition("ingress", events, handlerRef)
	ingressRef := plugin.RefTo[storeContract]("ingress")
	dependent := plainDependentDefinition("dependent", events, ingressRef)
	pool := drainOnlyDefinition("pool", events, nil, nil)

	constructed := drainConstructed(t, db, handler, ingress, dependent, pool)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	report, drainReport, err := constructed.UnwindWithDrain(deadline, 5*time.Second, 5*time.Second, nil)
	require.NoError(t, err)

	recorded := events.recorded()
	require.Equal(t, []string{
		"dependent.Stop",
		"ingress.Stop",
		"pool.Drain",
		"handler.Stop",
		"db.Stop",
	}, recorded, "phase A (ingress closure, reverse order) must finish before phase B (drain), which must finish before phase C (remaining stops, reverse order)")

	require.Len(t, drainReport.Records, 1)
	assert.Equal(t, plugin.Identity{Plugin: "pool", Instance: plugin.DefaultInstance}, drainReport.Records[0].Identity)
	assert.Equal(t, DrainCompleted, drainReport.Records[0].Outcome)

	stopped := map[plugin.Key]StopOutcome{}
	for _, record := range report.Records {
		stopped[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, StopCompleted, stopped["dependent"])
	assert.Equal(t, StopCompleted, stopped["ingress"])
	assert.Equal(t, StopCompleted, stopped["handler"])
	assert.Equal(t, StopCompleted, stopped["db"])
	// pool has no Stop hook at all, so it is reported skipped rather than
	// completed -- its only lifecycle action in this unwind was the drain.
	assert.Equal(t, StopSkipped, stopped["pool"])
}

// drainOnlyIngressValue is a TrafficOpener that also declares a Drain hook, so
// the "drained inside the ingress closure" question has a concrete fixture.
type drainOnlyIngressValue struct {
	name   string
	events *drainEvents
}

func (value *drainOnlyIngressValue) OpenTraffic(*plugin.Context) error { return nil }
func (value *drainOnlyIngressValue) Drain(context.Context) error {
	value.events.record(value.name + ".Drain")
	return nil
}
func (value *drainOnlyIngressValue) Stop(context.Context) error {
	value.events.record(value.name + ".Stop")
	return nil
}

// TestDrainerInsideTheIngressClosureIsOnlyStoppedNotDrained pins the
// deliberate choice documented on Constructed.Unwind: an ingress-closure
// member that also implements Drainer is not drained in phase B. Phase A
// already stopped it, and draining it a second time under a different
// deadline would be asking one instance to hand over in-flight work to two
// different phases for one shutdown.
func TestDrainerInsideTheIngressClosureIsOnlyStoppedNotDrained(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}
	definition := plugin.Define("ingress-drainer", func(plugin.BuildContext) (*drainOnlyIngressValue, error) {
		return &drainOnlyIngressValue{name: "ingress-drainer", events: events}, nil
	})
	constructed := drainConstructed(t, definition)

	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, drainReport, err := constructed.UnwindWithDrain(deadline, time.Second, time.Second, nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"ingress-drainer.Stop"}, events.recorded(),
		"an ingress-closure member's Drain hook is never called; it is simply stopped in phase A")
	assert.True(t, drainReport.Empty(), "the drain phase produces no record for an instance phase A already handled")
}

// TestDrainPhaseDeadlineTimesOutAndTheRestOfTheUnwindStillRuns covers the
// drain budget's own contract: a Drain that blocks until its context is done
// must be reported as timed out (abandoned) at drain_timeout, and every Stop
// behind it in the walk must still run rather than being held up by it.
func TestDrainPhaseDeadlineTimesOutAndTheRestOfTheUnwindStillRuns(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}
	block := make(chan struct{})
	defer close(block)
	stuck := drainOnlyDefinition("stuck-drainer", events, block, nil)
	after := plainDefinition("after", events)

	constructed := drainConstructed(t, stuck, after)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const drainBudget = 100 * time.Millisecond

	started := time.Now()
	report, drainReport, err := constructed.UnwindWithDrain(deadline, 5*time.Second, drainBudget, nil)
	elapsed := time.Since(started)

	require.NoError(t, err, "a Drain timeout is recorded in the DrainReport but must not fail the unwind, exactly as an abandoned PreStop does not fail the run")
	require.Len(t, drainReport.Records, 1)
	assert.Equal(t, DrainAbandoned, drainReport.Records[0].Outcome)
	assert.Contains(t, drainReport.Records[0].Err.Error(), "drain budget")
	assert.Less(t, elapsed, 2*time.Second, "the drain phase must not consume more than its own budget")

	stopped := map[plugin.Key]StopOutcome{}
	for _, record := range report.Records {
		stopped[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, StopCompleted, stopped["after"], "the rest of the unwind must still run after an abandoned Drain")
	// "stuck-drainer" declares no Stop hook, so it is skipped rather than
	// completed, but it must still be reached by phase C.
	assert.Equal(t, StopSkipped, stopped["stuck-drainer"])
}

// TestDrainBudgetStartsWhenThePhaseBegins pins that drain_timeout is the
// drain phase's own budget, measured from the moment phase B begins rather
// than from the start of the walk: an ingress Stop that outlasts the whole
// drain budget must not spend it. Before the phase deadline was derived
// inside phase B, the slow ingress Stop consumed the budget and this walk
// reported the drainer not-attempted without ever calling it.
func TestDrainBudgetStartsWhenThePhaseBegins(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}
	const (
		drainBudget = 100 * time.Millisecond
		stopCost    = 3 * drainBudget
	)
	remaining := make(chan time.Duration, 1)
	db := plainDefinition("db", events)
	ingress := slowIngressDefinition("slow-ingress", events, plugin.RefTo[storeContract]("db"), stopCost)
	pool := drainOnlyDefinition("pool", events, nil, func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok {
			remaining <- -1
			return
		}
		remaining <- time.Until(deadline)
	})

	constructed := drainConstructed(t, db, ingress, pool)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, drainReport, err := constructed.UnwindWithDrain(deadline, 5*time.Second, drainBudget, nil)
	require.NoError(t, err)

	require.Len(t, drainReport.Records, 1)
	assert.Equal(t, DrainCompleted, drainReport.Records[0].Outcome,
		"the drain phase must still run after an ingress Stop that outlasts its budget")
	select {
	case left := <-remaining:
		assert.Greater(t, left, drainBudget/2,
			"the phase's budget must start when the phase begins, not when the walk does")
	default:
		t.Fatal("Drain was never called: the slow ingress Stop consumed the drain budget")
	}

	stopped := map[plugin.Key]StopOutcome{}
	for _, record := range report.Records {
		stopped[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, StopCompleted, stopped["db"])
	assert.Equal(t, StopSkipped, stopped["pool"], "the drain-only fixture declares no Stop hook")
}

// TestDrainTimeoutZeroNeverCallsDrainAndDoesNotStopSubsequentStops pins the
// documented off switch: drainBudget of 0 skips phase B entirely, no Drain
// hook runs, and the unwind still finishes every Stop.
func TestDrainTimeoutZeroNeverCallsDrainAndDoesNotStopSubsequentStops(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}
	called := false
	drainer := drainOnlyDefinition("off-drainer", events, nil, func(context.Context) { called = true })
	other := plainDefinition("other", events)

	constructed := drainConstructed(t, drainer, other)

	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	report, drainReport, err := constructed.UnwindWithDrain(deadline, time.Second, 0, nil)
	require.NoError(t, err)

	assert.False(t, called, "drain_timeout 0s must not call the hook")
	assert.True(t, drainReport.Empty(), "a skipped phase produces no record at all")

	stopped := map[plugin.Key]StopOutcome{}
	for _, record := range report.Records {
		stopped[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, StopSkipped, stopped["off-drainer"], "the drain-only fixture declares no Stop hook")
	assert.Equal(t, StopCompleted, stopped["other"])
}

// TestDrainErrorAndPanicAreReportedAndDoNotStopSubsequentStops covers both
// failure shapes the phase has to classify, in one test: a returned error and
// a recovered panic, neither of which may prevent the unwind from reaching the
// instances behind the failing one.
func TestDrainErrorAndPanicAreReportedAndDoNotStopSubsequentStops(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}

	failing := drainFailureDefinition("drain-fails", events, assert.AnError, false)
	angry := drainFailureDefinition("drain-angry", events, nil, true)
	calm := plainDefinition("after-both", events)

	constructed := drainConstructed(t, failing, angry, calm)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, drainReport, err := constructed.UnwindWithDrain(deadline, 5*time.Second, 5*time.Second, nil)
	require.NoError(t, err, "a Drain failure or panic is recorded in the DrainReport but must not fail the unwind, exactly as PreStop's own failures do not fail the run")

	outcomes := map[plugin.Key]DrainOutcome{}
	for _, record := range drainReport.Records {
		outcomes[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, DrainFailed, outcomes["drain-fails"])
	assert.Equal(t, DrainPanicked, outcomes["drain-angry"])

	stopped := map[plugin.Key]StopOutcome{}
	for _, record := range report.Records {
		stopped[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, StopCompleted, stopped["after-both"],
		"a failed or panicking Drain must not stop subsequent Stops")
}

// TestDrainRunsBeforeEveryStopWhenThereIsNoTrafficOpener pins the degenerate
// case the spec calls out explicitly: a composition with no TrafficOpener at
// all has an empty ingress closure, so Unwind collapses to drain-then-stop for
// every instance.
func TestDrainRunsBeforeEveryStopWhenThereIsNoTrafficOpener(t *testing.T) {
	t.Parallel()
	events := &drainEvents{}
	drainer := drainDefinition("solo-drainer", events, nil, nil)

	constructed := drainConstructed(t, drainer)

	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, drainReport, err := constructed.UnwindWithDrain(deadline, time.Second, time.Second, nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"solo-drainer.Drain", "solo-drainer.Stop"}, events.recorded(),
		"with no TrafficOpener the ingress closure is empty, so drain runs before every Stop")
	require.Len(t, drainReport.Records, 1)
	assert.Equal(t, DrainCompleted, drainReport.Records[0].Outcome)
}

// TestDrainOnANilConstructedIsANoOp keeps the drain-aware entry point callable
// from a path that never constructed anything, exactly as Constructed.PreStop
// and Constructed.Unwind already are.
func TestDrainOnANilConstructedIsANoOp(t *testing.T) {
	t.Parallel()
	var constructed *Constructed
	report, drainReport, err := constructed.UnwindWithDrain(context.Background(), time.Second, time.Second, nil)
	require.NoError(t, err)
	assert.Empty(t, report.Records)
	assert.True(t, drainReport.Empty())
}
