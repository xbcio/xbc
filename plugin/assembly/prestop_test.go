package assembly

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// preStopProbe is what one PreStop hook observed and did. Every field is
// written by the hook and read by the test only after the phase has returned,
// so the mutex is what makes the reading race-free even when the phase
// abandoned the hook and therefore established no ordering with it.
type preStopProbe struct {
	mu       sync.Mutex
	calls    []string
	contexts []context.Context
	block    chan struct{}
	release  chan struct{}
	panics   bool
	err      error
}

func (probe *preStopProbe) enter(key string, ctx context.Context) {
	probe.mu.Lock()
	probe.calls = append(probe.calls, key)
	probe.contexts = append(probe.contexts, ctx)
	probe.mu.Unlock()
}

func (probe *preStopProbe) recorded() []string {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return append([]string(nil), probe.calls...)
}

// preStopDefinition builds one Definition whose only lifecycle work is
// PreStop, so a phase test never has to account for a Stop it did not ask for.
func preStopDefinition(key plugin.Key, probe *preStopProbe) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*store, error) {
		return &store{name: key.String()}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		PreStop: func(_ *store, ctx context.Context) error {
			probe.enter(key.String(), ctx)
			if probe.release != nil {
				<-probe.release
			}
			if probe.panics {
				panic("prestop boom")
			}
			return probe.err
		},
	}})
}

func preStopConstructed(t *testing.T, definitions ...plugin.Definition) *Constructed {
	t.Helper()
	plan, err := planFor(t, nil, definitions...)
	require.NoError(t, err)
	constructed, err := Construct(plan, ConstructOptions{})
	require.NoError(t, err)
	return constructed
}

// TestPreStopPhaseWaitsForEveryHookAndRecordsEachOutcome pins the ordinary
// case first: a hook that returns inside the budget is reported as completed
// with the time it cost, and an instance that declares no hook produces no
// record at all rather than a zero-length one.
func TestPreStopPhaseWaitsForEveryHookAndRecordsEachOutcome(t *testing.T) {
	t.Parallel()
	probe := &preStopProbe{}
	silent := plugin.Define("b-silent", func(plugin.BuildContext) (*store, error) {
		return &store{name: "b-silent"}, nil
	})
	constructed := preStopConstructed(t, preStopDefinition("a-hooked", probe), silent)

	deadline, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := constructed.PreStop(deadline, time.Minute)
	require.NoError(t, err)

	assert.Equal(t, []string{"a-hooked"}, probe.recorded())
	require.Len(t, report.Records, 1, "an instance without a hook is not a phase result")
	assert.Equal(t, plugin.Identity{Plugin: "a-hooked", Instance: plugin.DefaultInstance}, report.Records[0].Identity)
	assert.Equal(t, PreStopCompleted, report.Records[0].Outcome)
	assert.Nil(t, report.Records[0].Err)
	assert.False(t, report.Empty())
	assert.Equal(t, []string{"a-hooked " + report.Records[0].Duration.String()}, report.Waited())
}

// TestPreStopPhaseIsEmptyWhenNothingDeclaresAHook is the property that makes
// the phase safe to add to an existing application: a composition with no
// PreStop anywhere starts nothing, waits for nothing, and reports nothing.
func TestPreStopPhaseIsEmptyWhenNothingDeclaresAHook(t *testing.T) {
	t.Parallel()
	plain := plugin.Define("plain", func(plugin.BuildContext) (*store, error) {
		return &store{name: "plain"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		Stop: func(*store, context.Context) error { return nil },
	}})
	constructed := preStopConstructed(t, plain)

	report, err := constructed.PreStop(context.Background(), time.Second)
	require.NoError(t, err)
	assert.True(t, report.Empty())
	assert.Empty(t, report.Records)
	assert.Empty(t, report.Waited())
}

// TestPreStopInitiateEveryHookBeforeWaitingOnAnyOfThem is the phase's own
// ordering contract, and the reason it is not a sequential walk: hook A
// blocking must not cost hook B its chance to release. The probe blocks A
// until B has been entered, so a sequential walk deadlocks on the budget and
// then reports B as never having run.
func TestPreStopInitiateEveryHookBeforeWaitingOnAnyOfThem(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	blocked := &preStopProbe{release: release}
	// The second hook closes release as soon as it is entered. If the phase
	// started it only after the first had returned, nothing would ever close
	// release and the phase would spend its whole budget.
	entered := make(chan struct{})
	second := plugin.Define("b-second", func(plugin.BuildContext) (*store, error) {
		return &store{name: "b-second"}, nil
	}, plugin.Options[*store]{Lifecycle: plugin.Lifecycle[*store]{
		PreStop: func(*store, context.Context) error {
			close(entered)
			close(release)
			return nil
		},
	}})
	constructed := preStopConstructed(t, preStopDefinition("a-blocked", blocked), second)

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, err := constructed.PreStop(deadline, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, []PreStopOutcome{PreStopCompleted, PreStopCompleted},
		[]PreStopOutcome{report.Records[0].Outcome, report.Records[1].Outcome},
		"both hooks ran to completion; a sequential walk would have abandoned the second")
	assert.Equal(t, []string{"a-blocked"}, blocked.recorded())
}

// TestPreStopBudgetIsWholePhase is the budget's contract in one assertion: N
// hooks that each block for longer than the budget cost one budget, not N of
// them. Three hooks that never return must leave the phase after roughly the
// budget it was given, and every one of them must be reported abandoned.
func TestPreStopBudgetIsWholePhase(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	probes := make([]*preStopProbe, 3)
	definitions := make([]plugin.Definition, 3)
	for index := range probes {
		probes[index] = &preStopProbe{release: release}
		definitions[index] = preStopDefinition(plugin.Key(string(rune('a'+index))+"-stuck"), probes[index])
	}
	constructed := preStopConstructed(t, definitions...)

	const budget = 120 * time.Millisecond
	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	started := time.Now()
	report, err := constructed.PreStop(deadline, budget)
	elapsed := time.Since(started)

	require.Error(t, err, "an abandoned hook is reported, not swallowed")
	require.Len(t, report.Records, 3)
	for _, record := range report.Records {
		assert.Equal(t, PreStopAbandoned, record.Outcome)
		assert.Contains(t, record.Err.Error(), "pre-stop budget")
	}
	for _, probe := range probes {
		assert.Len(t, probe.recorded(), 1, "every hook was initiated, including the ones later abandoned")
	}
	assert.Less(t, elapsed, 3*budget,
		"three blocked hooks must cost one budget, not one budget each")
	assert.Empty(t, report.Waited(), "an abandoned hook was not waited for, so it is not a wait")
}

// TestPreStopPanicIsRecoveredAndDoesNotTakeTheShutdownDown covers the boundary
// the phase shares with Stop: foreign code panicking must be classified, not
// propagated, because a shutdown that dies on one plugin's panic cannot clean
// up anything else.
func TestPreStopPanicIsRecoveredAndDoesNotTakeTheShutdownDown(t *testing.T) {
	t.Parallel()
	calm := &preStopProbe{}
	angry := &preStopProbe{panics: true}
	constructed := preStopConstructed(t,
		preStopDefinition("a-angry", angry),
		preStopDefinition("b-calm", calm),
	)

	deadline, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := constructed.PreStop(deadline, time.Minute)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin a-angry PreStop panic: prestop boom",
		"the recovered panic names the stage it happened in, so it is never read as a failed Stop")
	assert.Equal(t, []string{"b-calm"}, calm.recorded(),
		"a panicking hook does not prevent another hook from running")
	outcomes := map[plugin.Key]PreStopOutcome{}
	for _, record := range report.Records {
		outcomes[record.Identity.Plugin] = record.Outcome
	}
	assert.Equal(t, PreStopPanicked, outcomes["a-angry"])
	assert.Equal(t, PreStopCompleted, outcomes["b-calm"])
}

// TestPreStopFailureIsReportedAndShutdownProceeds pins that an error is
// information and not a veto: the phase reports it, and every other hook still
// ran. A release that failed has no better outcome left to offer during a
// shutdown that is already under way.
func TestPreStopFailureIsReportedAndShutdownProceeds(t *testing.T) {
	t.Parallel()
	failing := &preStopProbe{err: errors.New("lease release refused")}
	constructed := preStopConstructed(t, preStopDefinition("a-failing", failing))

	deadline, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := constructed.PreStop(deadline, time.Minute)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin a-failing PreStop failed: lease release refused")
	require.Len(t, report.Records, 1)
	assert.Equal(t, PreStopFailed, report.Records[0].Outcome)
	assert.Equal(t, []string{"a-failing " + report.Records[0].Duration.String()}, report.Waited(),
		"a failed hook was waited for, so its cost is reported")
}

// TestPreStopOnANilConstructedIsANoOp keeps the phase callable from a shutdown
// path that never constructed anything. abort reaches unwind before owned is
// set, and the phase must not have to be guarded at that call site.
func TestPreStopOnANilConstructedIsANoOp(t *testing.T) {
	t.Parallel()
	var constructed *Constructed
	report, err := constructed.PreStop(context.Background(), time.Second)
	require.NoError(t, err)
	assert.True(t, report.Empty())
}

// TestInvokePreStopOnAnInstanceWithoutAHookCompletesInstantly pins the
// per-instance entry point's treatment of a declaration it does not have. It
// is not an abandoned attempt and not a failure: the instance declined, and
// the phase never calls it for such an instance in the first place.
func TestInvokePreStopOnAnInstanceWithoutAHookCompletesInstantly(t *testing.T) {
	t.Parallel()
	plain := plugin.Define("plain-invoke", func(plugin.BuildContext) (*store, error) {
		return &store{name: "plain-invoke"}, nil
	})
	constructed := preStopConstructed(t, plain)
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "plain-invoke"})
	require.True(t, ok)
	require.False(t, instance.HasPreStop())

	outcome, elapsed, err := instance.InvokePreStop(context.Background(), time.Second)
	require.NoError(t, err)
	assert.Equal(t, PreStopCompleted, outcome)
	assert.Zero(t, elapsed)
}

// TestInvokePreStopAbandonsAHookThatIgnoresItsDeadline covers the per-instance
// entry point's own abandon path, which the phase exercises only through its
// shared deadline.
func TestInvokePreStopAbandonsAHookThatIgnoresItsDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	probe := &preStopProbe{release: release}
	constructed := preStopConstructed(t, preStopDefinition("stuck", probe))
	instance, ok := constructed.Instance(plugin.Identity{Plugin: "stuck"})
	require.True(t, ok)

	const budget = 50 * time.Millisecond
	deadline, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	outcome, elapsed, err := instance.InvokePreStop(deadline, budget)

	require.Error(t, err)
	assert.Equal(t, PreStopAbandoned, outcome)
	assert.GreaterOrEqual(t, elapsed, budget,
		"an abandoned attempt is measured by how long it was waited for, not by a zero")
}
