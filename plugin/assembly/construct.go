package assembly

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

const defaultShutdownTimeout = 15 * time.Second

// ContextFactory creates the lifecycle Context for one planned identity.
type ContextFactory func(plugin.Identity, log.Logger) *plugin.Context

// Stage names one step the framework invokes on a single owned value. Every
// constant's value is the exact name this package already prints in its
// diagnostics, so a timing key and an error message can never disagree about
// what ran -- which is why StageFactory is spelled lower case while the
// Lifecycle hooks carry their Go field names.
type Stage string

const (
	// StageFactory is the Definition's factory call. It is not a Lifecycle
	// hook: it receives already-decoded configuration and resolved inputs,
	// and the convention is that it performs no I/O. It is timed anyway,
	// because a factory that breaks that convention would otherwise be the
	// one slow step with nowhere to look.
	StageFactory Stage = "factory"
	// StageInit runs inside Construct, after ownership has transferred.
	StageInit Stage = "Init"
	// StageMigrate runs only when the run was asked to migrate.
	StageMigrate Stage = "Migrate"
	// StageStart runs before the traffic gate is released.
	StageStart Stage = "Start"
	// StageOpenTraffic is the last startup stage before the gate opens.
	StageOpenTraffic Stage = "OpenTraffic"
	// StagePreStop runs during shutdown but before the reverse unwind, on its
	// own goroutine and under the pre-stop budget. Its duration is reported
	// per attempt in PreStopRecord rather than in Instance.Timings, for the
	// same reason StageStop's is: an abandoned PreStop outlives the phase that
	// started it, so a slice owned by the instance would be written from
	// outside the walk that reads it.
	StagePreStop Stage = "PreStop"
	// StageDrain runs during shutdown after the ingress closure has been
	// stopped and before any remaining instance's Stop, on its own goroutine
	// and under the drain budget. Its duration is reported per attempt in
	// DrainRecord rather than in Instance.Timings, for the same reason
	// StageStop's and StagePreStop's are.
	StageDrain Stage = "Drain"
	// StageStop runs during the reverse unwind, on its own goroutine and
	// under a shared deadline. Its duration is reported per attempt in
	// StopRecord rather than in Instance.Timings.
	StageStop Stage = "Stop"
)

// StageTiming is how long one stage took for one instance. A stage the
// instance does not declare produces no StageTiming at all, so a reader can
// tell "never ran" apart from "ran instantly".
type StageTiming struct {
	Stage    Stage
	Duration time.Duration
}

// ConstructOptions configures the sole resource-owning transaction.
type ConstructOptions struct {
	ContextFactory  ContextFactory
	ShutdownTimeout time.Duration

	// OnStageBegin, when set, is called immediately before this package
	// invokes one startup stage on one instance: the factory and Init inside
	// Construct, and Migrate, Start and OpenTraffic through the Invoke*
	// methods. A hook the Definition never declared announces nothing,
	// because nothing runs. StageStop, StagePreStop and StageDrain announce
	// nothing either: all three are already bounded by the budget they run
	// under and reported per attempt in StopRecord, PreStopRecord and
	// DrainRecord, and all three run on their own goroutine, so they are the
	// stages a caller can already see while they are still in flight.
	//
	// Only the beginning is reported, and that is enough to name the stage
	// currently in flight: these stages are strictly serial, so the last one
	// announced is the one that has not returned yet. StageTiming cannot
	// answer that question at all, because an entry is appended only after
	// the hook returns -- which is precisely why the stage an operator most
	// needs named, the one that never returns, has no timing.
	//
	// It runs on the goroutine driving the stage, outside the panic boundary,
	// so an implementation must neither block nor panic.
	OnStageBegin func(plugin.Identity, Stage)
}

// Instance is one successfully factory-returned, framework-owned primary
// value with lifecycle metadata compiled before construction began.
type Instance struct {
	identity  plugin.Identity
	value     any
	context   *plugin.Context
	lifecycle lifecycleDescriptor

	// workload is the workload this instance's Definition belongs to, or ""
	// when it belongs to none. It is carried here rather than looked up from
	// the planned graph because materializeSlots publishes it into the
	// ResolvedEntry a consumer reads, and that must describe the instance that
	// was actually constructed.
	workload pluginmodel.WorkloadKey

	// dependsOn lists the identities this instance declared an Input on,
	// deduplicated, in no particular order. It is copied out of the plan's own
	// bindings at construction time rather than read from the plan afterward,
	// because Unwind's ingress closure (see Constructed.unwindPhases) needs
	// exactly the edges that were actually wired for the instances that were
	// actually constructed, and an Instance is the one record both methods
	// already share.
	dependsOn []plugin.Identity

	// timings is appended to only by the single goroutine that drives this
	// instance's lifecycle: Construct, and then the Invoke* methods the
	// runtime calls in order. Stop is excluded by construction because it
	// runs on its own goroutine and an abandoned Stop outlives the walk.
	timings []StageTiming

	// onStageBegin is ConstructOptions.OnStageBegin, or nil. A nil callback
	// is never called, so an unobserved construction costs one comparison
	// per stage.
	onStageBegin func(plugin.Identity, Stage)

	stopMu  sync.Mutex
	stopped bool
}

func (instance *Instance) Identity() plugin.Identity { return instance.identity }
func (instance *Instance) Primary() any              { return instance.value }
func (instance *Instance) Context() *plugin.Context  { return instance.context }
func (instance *Instance) HasMigration() bool        { return instance.lifecycle.migrate != nil }
func (instance *Instance) HasStart() bool            { return instance.lifecycle.start != nil }
func (instance *Instance) HasTrafficPreparation() bool {
	return instance.lifecycle.openTraffic != nil
}
func (instance *Instance) HasStop() bool { return instance.lifecycle.stop != nil }

// HasPreStop reports whether this instance declares a PreStop hook.
func (instance *Instance) HasPreStop() bool { return instance.lifecycle.preStop != nil }

// HasDrain reports whether this instance declares a Drain hook.
func (instance *Instance) HasDrain() bool { return instance.lifecycle.drain != nil }

// IsIngress reports whether this instance opens traffic: either its primary
// value implements plugin.TrafficOpener directly, or an Options[P].Lifecycle
// adapter supplies OpenTraffic. Both forms are equally real ingress -- the
// compiled lifecycleDescriptor is what already erases that distinction for
// every other stage, and the ingress closure must not see fewer ingress
// instances than Construct's own OpenTraffic phase does. It names only the
// direct capability, not the transitive ingress closure Unwind actually stops
// first; see Constructed.ingressClosure for that set.
func (instance *Instance) IsIngress() bool {
	return instance.lifecycle.openTraffic != nil
}

// Timings returns how long each stage took for this instance, in the order the
// stages ran, including a stage that ended in an error or a recovered panic:
// "Start failed after 30s" is the reading an operator needs most.
//
// It is safe to call once the lifecycle driver has returned from the stage in
// question. StageStop never appears here; see StopRecord.Duration.
func (instance *Instance) Timings() []StageTiming {
	if instance == nil {
		return nil
	}
	return append([]StageTiming(nil), instance.timings...)
}

// Constructed is the complete owned value set returned only after every
// factory and Init stage succeeds.
type Constructed struct {
	instances  []*Instance
	byIdentity map[plugin.Identity]*Instance
}

func (constructed *Constructed) Instances() []*Instance {
	if constructed == nil {
		return nil
	}
	return append([]*Instance(nil), constructed.instances...)
}

func (constructed *Constructed) Instance(identity plugin.Identity) (*Instance, bool) {
	if constructed == nil {
		return nil, false
	}
	instance, ok := constructed.byIdentity[identity.Normalized()]
	return instance, ok
}

// Construct invokes every factory exactly once in frozen graph order. A
// successful non-nil factory return transfers ownership before Init. Any later
// failure fully unwinds all owned values before this function returns.
func Construct(plan *Plan, options ConstructOptions) (*Constructed, error) {
	if plan == nil {
		return nil, fmt.Errorf("xbc: cannot construct a nil assembly plan")
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = defaultShutdownTimeout
	}
	constructed := &Constructed{
		instances:  make([]*Instance, 0, len(plan.order)),
		byIdentity: make(map[plugin.Identity]*Instance, len(plan.order)),
	}
	for _, identity := range plan.order {
		planned := plan.instances[identity]
		slots, err := materializeSlots(planned, constructed.byIdentity)
		if err != nil {
			return nil, joinConstructionFailure(err, constructed, options.ShutdownTimeout)
		}
		buildContext := pluginmodel.NewBuildContext(
			pluginmodel.Identity{Plugin: pluginmodel.Key(identity.Plugin), Instance: identity.Instance},
			planned.logger,
			slots,
		)
		if options.OnStageBegin != nil {
			options.OnStageBegin(identity, StageFactory)
		}
		factoryStarted := time.Now()
		value, err := invokeFactory(planned, buildContext)
		factoryElapsed := time.Since(factoryStarted)
		pluginmodel.InvalidateBuildContext(buildContext)
		if err != nil {
			return nil, joinConstructionFailure(err, constructed, options.ShutdownTimeout)
		}
		if isNil(value) {
			err = fmt.Errorf("xbc: plugin %s factory returned nil primary value", identity)
			return nil, joinConstructionFailure(err, constructed, options.ShutdownTimeout)
		}
		if got := reflect.TypeOf(value); got != planned.definition.Primary {
			err = fmt.Errorf("xbc: internal invariant: plugin %s factory returned %s, frozen primary type is %s", identity, got, planned.definition.Primary)
			return nil, joinConstructionFailure(err, constructed, options.ShutdownTimeout)
		}

		lifecycleContext := (*plugin.Context)(nil)
		if options.ContextFactory != nil {
			lifecycleContext = options.ContextFactory(identity, planned.logger)
		}
		instance := &Instance{
			identity:     identity,
			value:        value,
			context:      lifecycleContext,
			lifecycle:    planned.lifecycle,
			workload:     planned.workload,
			dependsOn:    directDependencies(planned),
			timings:      []StageTiming{{Stage: StageFactory, Duration: factoryElapsed}},
			onStageBegin: options.OnStageBegin,
		}
		// Ownership transfers before Init. From here onward this instance is in
		// every rollback set, including an Init failure or panic.
		constructed.instances = append(constructed.instances, instance)
		constructed.byIdentity[identity] = instance

		if err := instance.invoke(StageInit, instance.lifecycle.init); err != nil {
			return nil, joinConstructionFailure(err, constructed, options.ShutdownTimeout)
		}
	}
	return constructed, nil
}

// directDependencies flattens one planned instance's token bindings into the
// deduplicated set of identities it declared an Input on. Order does not
// matter to any caller: the ingress closure (see Constructed.ingressClosure)
// only tests set membership, never walks this slice in a particular order.
func directDependencies(planned *plannedInstance) []plugin.Identity {
	if len(planned.bindings) == 0 {
		return nil
	}
	seen := make(map[plugin.Identity]bool)
	var dependencies []plugin.Identity
	for _, producers := range planned.bindings {
		for _, producer := range producers {
			producer = producer.Normalized()
			if seen[producer] {
				continue
			}
			seen[producer] = true
			dependencies = append(dependencies, producer)
		}
	}
	return dependencies
}

func materializeSlots(planned *plannedInstance, owned map[plugin.Identity]*Instance) (map[uint64][]pluginmodel.ResolvedEntry, error) {
	slots := make(map[uint64][]pluginmodel.ResolvedEntry, len(planned.bindings))
	for tokenID, producers := range planned.bindings {
		entries := make([]pluginmodel.ResolvedEntry, len(producers))
		for index, producer := range producers {
			instance, exists := owned[producer]
			if !exists {
				return nil, fmt.Errorf("xbc: internal invariant: plugin %s input token %d producer %s is not constructed", planned.identity, tokenID, producer)
			}
			entries[index] = pluginmodel.ResolvedEntry{
				Identity: pluginmodel.Identity{Plugin: pluginmodel.Key(producer.Plugin), Instance: producer.Instance},
				Value:    instance.value,
				// The producer's own reconciled membership, taken from the
				// instance that was actually constructed rather than from the
				// consumer's view of the graph, so an application accounting
				// through Entry[T].Workload attributes the value to the workload
				// that really owns it.
				Workload: instance.workload,
			}
		}
		slots[tokenID] = entries
	}
	return slots, nil
}

func invokeFactory(planned *plannedInstance, context pluginmodel.BuildContext) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("xbc: plugin %s factory panic: %v\n%s", planned.identity, recovered, debug.Stack())
		}
	}()
	value, err = planned.plan.Factory(context)
	if err != nil {
		return nil, fmt.Errorf("xbc: plugin %s factory failed: %w", planned.identity, err)
	}
	return value, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}

func joinConstructionFailure(cause error, constructed *Constructed, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, unwind := constructed.Unwind(deadline, timeout, nil)
	return errors.Join(cause, unwind)
}

// InvokeMigration runs one frozen migration descriptor under a panic boundary.
func (instance *Instance) InvokeMigration() error {
	return instance.invoke(StageMigrate, instance.lifecycle.migrate)
}

// InvokeStart runs one frozen start descriptor under a panic boundary.
func (instance *Instance) InvokeStart() error {
	return instance.invoke(StageStart, instance.lifecycle.start)
}

// InvokeTrafficPreparation runs one frozen traffic-preparation descriptor.
func (instance *Instance) InvokeTrafficPreparation() error {
	return instance.invoke(StageOpenTraffic, instance.lifecycle.openTraffic)
}

// invoke runs one declared startup hook and records what it cost. A hook the
// Definition never declared is not a zero-length stage, so it is neither run,
// nor timed, nor announced.
func (instance *Instance) invoke(stage Stage, hook func(any, *plugin.Context) error) error {
	if hook == nil {
		return nil
	}
	if instance.onStageBegin != nil {
		instance.onStageBegin(instance.identity, stage)
	}
	started := time.Now()
	_, err := instance.invokeClassified(stage, func() error {
		return hook(instance.value, instance.context)
	})
	instance.timings = append(instance.timings, StageTiming{Stage: stage, Duration: time.Since(started)})
	return err
}

// invokeClassified runs one lifecycle stage under a panic boundary and tells
// a recovered panic apart from a returned error without parsing diagnostics.
func (instance *Instance) invokeClassified(stage Stage, fn func() error) (panicked bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			err = fmt.Errorf("xbc: plugin %s %s panic: %v\n%s", instance.identity, stage, recovered, debug.Stack())
		}
	}()
	if err := fn(); err != nil {
		return false, fmt.Errorf("xbc: plugin %s %s failed: %w", instance.identity, stage, err)
	}
	return false, nil
}

// StopOutcome classifies how one owned value's Stop attempt ended.
type StopOutcome string

const (
	// StopSkipped means the instance declares no Stop hook, or a previous
	// unwind already stopped it.
	StopSkipped StopOutcome = "skipped"
	// StopCompleted means Stop returned nil inside the shared budget.
	StopCompleted StopOutcome = "completed"
	// StopFailed means Stop returned an error inside the shared budget.
	StopFailed StopOutcome = "failed"
	// StopPanicked means Stop panicked; the panic was recovered and reported.
	StopPanicked StopOutcome = "panicked"
	// StopAbandoned means Stop ignored the shared deadline and was left
	// running. That is a plugin contract violation, not a runtime choice.
	StopAbandoned StopOutcome = "abandoned"
	// StopNotAttempted means the shared budget was already spent when the
	// reverse walk reached this instance, so no Stop was started for it.
	StopNotAttempted StopOutcome = "not-attempted"
)

// StopRecord is one instance's entry in a ShutdownReport. TaskErr carries the
// afterStop (task-scope cancel and join) diagnostic for the same identity.
type StopRecord struct {
	Identity plugin.Identity
	Outcome  StopOutcome
	Err      error
	TaskErr  error
	// Duration is how long the walk waited on this instance: until Stop
	// returned, or until the shared deadline expired and the Stop was
	// abandoned. It is zero for a skipped or not-attempted instance. Because
	// exactly one Stop is in flight at a time, these durations are what make
	// an expired budget attributable to the instances that consumed it and
	// not only to the ones it cut off.
	Duration time.Duration
}

// ShutdownReport is the observable result of one reverse unwind.
//
// Attempted lists the identities the walk reached while the shared budget was
// still live, in reverse graph order; Completed lists the identities whose
// Stop returned — successfully, with an error, or through a recovered panic —
// before that budget was spent, in the order those calls returned. An
// instance with no Stop hook appears in both: there was nothing to run and
// nothing to wait for. Because Unwind starts no Stop until the previous one
// has returned or been abandoned, the two lists are identical on every path
// where nothing is abandoned; an implementation that launched Stop hooks
// concurrently would reorder Completed against Attempted.
type ShutdownReport struct {
	Attempted []plugin.Identity
	Completed []plugin.Identity
	Records   []StopRecord
}

// Identities returns the recorded identities with the given outcome, in
// reverse graph order.
func (report ShutdownReport) Identities(outcome StopOutcome) []plugin.Identity {
	var found []plugin.Identity
	for _, record := range report.Records {
		if record.Outcome == outcome {
			found = append(found, record.Identity)
		}
	}
	return found
}

// StopBounded stops one owned value at most once and abandons a stuck Stop
// when the shared deadline expires.
func (instance *Instance) StopBounded(deadline context.Context, budget time.Duration) error {
	_, _, err := instance.stopBounded(deadline, budget)
	return err
}

type stopResult struct {
	outcome StopOutcome
	err     error
}

func (instance *Instance) stopBounded(deadline context.Context, budget time.Duration) (StopOutcome, time.Duration, error) {
	instance.stopMu.Lock()
	if instance.stopped {
		instance.stopMu.Unlock()
		return StopSkipped, 0, nil
	}
	instance.stopped = true
	instance.stopMu.Unlock()
	if instance.lifecycle.stop == nil {
		return StopSkipped, 0, nil
	}
	started := time.Now()
	done := make(chan stopResult, 1)
	go func() {
		panicked, err := instance.invokeClassified(StageStop, func() error {
			return instance.lifecycle.stop(instance.value, deadline)
		})
		switch {
		case panicked:
			done <- stopResult{outcome: StopPanicked, err: err}
		case err != nil:
			done <- stopResult{outcome: StopFailed, err: err}
		default:
			done <- stopResult{outcome: StopCompleted}
		}
	}()
	select {
	case result := <-done:
		return result.outcome, time.Since(started), result.err
	case <-deadline.Done():
	}
	// The budget is spent. Read done once more without blocking before
	// declaring the Stop abandoned: a select whose cases are both ready picks
	// one at random, so without this second read a Stop that finished in the
	// same instant as the deadline would be classified by the scheduler
	// rather than by what it actually did.
	select {
	case result := <-done:
		return result.outcome, time.Since(started), result.err
	default:
		return StopAbandoned, time.Since(started), fmt.Errorf("xbc: plugin %s Stop did not return within shutdown budget %s; abandoning it", instance.identity, budget)
	}
}

// PreStopOutcome classifies how one owned value's PreStop attempt ended.
//
// The vocabulary deliberately stops short of StopOutcome's: there is no
// "skipped" and no "not-attempted" because the phase has no per-instance
// omissions to report. An instance that declares no hook produces no record
// at all -- the phase is entered for the instances that have one -- and a
// wasted budget cannot deny a later instance its turn, because every hook is
// already running by the time the budget can expire. See Constructed.PreStop.
type PreStopOutcome string

const (
	// PreStopCompleted means PreStop returned nil inside the phase budget.
	PreStopCompleted PreStopOutcome = "completed"
	// PreStopFailed means PreStop returned an error inside the phase budget.
	// The error is reported, and the shutdown proceeds: a release that failed
	// has no better outcome left to offer.
	PreStopFailed PreStopOutcome = "failed"
	// PreStopPanicked means PreStop panicked; the panic was recovered and
	// reported, and the shutdown proceeds.
	PreStopPanicked PreStopOutcome = "panicked"
	// PreStopAbandoned means PreStop ignored the phase budget and was left
	// running. That is a plugin contract violation, not a runtime choice. The
	// abandoned hook keeps running and is therefore concurrent with every Stop
	// that follows, which is exactly why Stop must be safe without a
	// successful PreStop.
	//
	// An abandoned hook is recorded once, with this outcome, and never again:
	// if it later returns or fails, that answer is dropped, because the phase
	// has already stopped waiting and its buffered result channel has no
	// reader. So an operator sees that the hook overran, and not what it would
	// eventually have said -- which is the honest report, since the phase is
	// over by then.
	PreStopAbandoned PreStopOutcome = "abandoned"
)

// PreStopRecord is one instance's entry in a PreStopReport.
type PreStopRecord struct {
	Identity plugin.Identity
	Outcome  PreStopOutcome
	Err      error
	// Duration is how long the phase waited on this instance: until PreStop
	// returned, or until the phase budget expired and it was abandoned.
	Duration time.Duration
}

// PreStopReport is the observable result of one pre-stop phase. It records
// only the instances that declared a hook, in graph order.
type PreStopReport struct {
	Records []PreStopRecord
}

// Identities returns the recorded identities with the given outcome, in graph
// order.
func (report PreStopReport) Identities(outcome PreStopOutcome) []plugin.Identity {
	var found []plugin.Identity
	for _, record := range report.Records {
		if record.Outcome == outcome {
			found = append(found, record.Identity)
		}
	}
	return found
}

// Empty reports whether this phase ran no hook at all. A phase that started
// nothing and a phase that started nothing successfully are the same thing to
// every caller, so they share one answer.
func (report PreStopReport) Empty() bool { return len(report.Records) == 0 }

// Waited renders how long the phase waited on each instance it actually waited
// for, in graph order. Abandoned instances are omitted: nothing was waited for
// there, and their recorded duration is the phase budget rather than a
// measurement of anything the plugin did.
func (report PreStopReport) Waited() []string {
	var labels []string
	for _, record := range report.Records {
		if record.Outcome == PreStopAbandoned {
			continue
		}
		labels = append(labels, record.Identity.String()+" "+record.Duration.String())
	}
	return labels
}

// preStopResult is what one instance's PreStop goroutine reports back. The
// channel it travels on is buffered, so an abandoned hook still returns
// without blocking on a reader that has already moved on.
type preStopResult struct {
	outcome PreStopOutcome
	err     error
	elapsed time.Duration
}

// InvokePreStop starts one instance's PreStop hook on its own goroutine under
// the phase budget and returns how the attempt ended.
//
// It is the PreStop counterpart of stopBounded, and it reuses that method's
// abandon semantics on purpose: the hook runs detached, so a plugin that
// ignores its deadline cannot hold the shutdown open. What it does not reuse
// is the panic boundary's stage argument -- invokeClassified is called with
// StagePreStop, so a recovered panic reads "plugin X PreStop panic", not
// "plugin X Stop panic", and the two are never confused in a log.
//
// deadline is the phase's shared context, not a per-instance one. Every hook
// started by one phase therefore observes the same instant, which is what
// makes the budget whole-phase: N hooks that each block cost one budget, not
// N. A nil hook is not an error and not an abandoned attempt; it is the
// instance politely declining, which is why it reports PreStopCompleted in
// zero time.
//
// It exists for a caller driving one instance on its own. The phase itself
// does not go through it: Constructed.PreStop has to initiate every hook
// before waiting on any of them, so it launches and awaits separately.
func (instance *Instance) InvokePreStop(deadline context.Context, budget time.Duration) (PreStopOutcome, time.Duration, error) {
	hook := instance.lifecycle.preStop
	if hook == nil {
		return PreStopCompleted, 0, nil
	}
	var waiting sync.WaitGroup
	waiting.Add(1)
	done, started := instance.startPreStop(hook, deadline, &waiting)
	return instance.awaitPreStop(done, started, deadline, budget)
}

// startPreStop launches the hook and returns the channel its outcome will
// arrive on, together with the instant it was launched. Splitting the launch
// from the wait is what lets Constructed.PreStop initiate every hook before it
// waits on any of them.
//
// The channel is buffered and is the only channel the goroutine ever writes,
// so an abandoned hook still returns rather than blocking on a reader that has
// already moved on. waiting is decremented by the same goroutine, which is how
// a caller learns that every channel now holds its value without consuming any
// of them.
func (instance *Instance) startPreStop(hook func(any, context.Context) error, deadline context.Context, waiting *sync.WaitGroup) (chan preStopResult, time.Time) {
	done := make(chan preStopResult, 1)
	started := time.Now()
	go func() {
		defer waiting.Done()
		panicked, err := instance.invokeClassified(StagePreStop, func() error {
			return hook(instance.value, deadline)
		})
		result := preStopResult{err: err, elapsed: time.Since(started)}
		switch {
		case panicked:
			result.outcome = PreStopPanicked
		case err != nil:
			result.outcome = PreStopFailed
		default:
			result.outcome = PreStopCompleted
		}
		done <- result
	}()
	return done, started
}

// awaitPreStop reads one launched hook's outcome under the shared deadline.
//
// The two-step read mirrors stopBounded's for the same reason: a select whose
// cases are both ready picks one at random, so a hook that returned in the
// same instant the budget expired must be classified by what it did rather
// than by the scheduler. The second read is what makes an already-completed
// hook report its real duration instead of the budget.
func (instance *Instance) awaitPreStop(done chan preStopResult, started time.Time, deadline context.Context, budget time.Duration) (PreStopOutcome, time.Duration, error) {
	if result, ok := readPreStopResult(done); ok {
		return result.outcome, result.elapsed, result.err
	}
	select {
	case result := <-done:
		return result.outcome, result.elapsed, result.err
	case <-deadline.Done():
	}
	if result, ok := readPreStopResult(done); ok {
		return result.outcome, result.elapsed, result.err
	}
	return PreStopAbandoned, time.Since(started), fmt.Errorf(
		"xbc: plugin %s PreStop did not return within pre-stop budget %s; abandoning it",
		instance.identity, budget)
}

// readPreStopResult takes an outcome that is already waiting, without blocking.
func readPreStopResult(done chan preStopResult) (preStopResult, bool) {
	select {
	case result := <-done:
		return result, true
	default:
		return preStopResult{}, false
	}
}

// PreStop runs one pre-stop phase: it initiates every declared PreStop hook
// and then waits for all of them under a single shared budget.
//
// # Why the hooks run concurrently
//
// Unwind walks Stop hooks one at a time and treats reverse order as a
// contract, because each Stop releases something the next one still needs.
// PreStop has the opposite shape. It retracts a value's externally visible
// participation -- a lease, a published address, a registration -- and those
// are independent of each other, so there is no order for the phase to
// preserve and no first hook that any other hook depends on.
//
// Running them concurrently is therefore not a convenience, it is the property
// that makes the phase safe to rely on: hook A blocking must not cost hook B
// its chance to release. A sequential walk would let one stuck hook eat the
// whole budget and leave every later instance abandoned, which would turn a
// single plugin's bug into a cluster-wide failure to hand over. Starting all
// of them first also gives the strictest reading of the phase's own contract,
// that every PreStop is initiated before any Stop begins.
//
// # What the budget bounds
//
// budget bounds the phase, not one hook: every hook observes the same
// deadline, so N hooks that each block for longer than the budget still cost
// one budget. When it expires, the hooks still running are reported abandoned
// and left running, and this method returns. They are then concurrent with the
// Stops that follow, which is a contract Stop already has to satisfy: Stop
// must be correct whether or not its PreStop succeeded.
//
// It returns the records it gathered together with the collected errors. A
// non-nil error never means the phase failed to run; it means at least one
// hook failed, panicked, or was abandoned, and the caller is expected to log
// it and shut down anyway. The phase is entered only for instances that
// declare a hook, so an application that declares none produces an empty
// report and no error.
func (constructed *Constructed) PreStop(deadline context.Context, budget time.Duration) (PreStopReport, error) {
	var report PreStopReport
	if constructed == nil {
		return report, nil
	}
	type launched struct {
		instance *Instance
		done     chan preStopResult
		started  time.Time
	}
	var pending []launched
	var waiting sync.WaitGroup
	for _, instance := range constructed.instances {
		if !instance.HasPreStop() {
			continue
		}
		waiting.Add(1)
		done, started := instance.startPreStop(instance.lifecycle.preStop, deadline, &waiting)
		pending = append(pending, launched{instance: instance, done: done, started: started})
	}
	if len(pending) == 0 {
		return report, nil
	}

	// Wait for every hook, or for the shared deadline, whichever comes first.
	// The join runs on its own goroutine because the hooks are foreign code:
	// the phase must not depend on any of them observing anything, and waiting
	// on the WaitGroup inline would have no deadline to interleave with.
	all := make(chan struct{})
	go func() {
		waiting.Wait()
		close(all)
	}()
	select {
	case <-all:
	case <-deadline.Done():
	}

	var errs []error
	for _, item := range pending {
		outcome, elapsed, err := item.instance.awaitPreStop(item.done, item.started, deadline, budget)
		report.Records = append(report.Records, PreStopRecord{
			Identity: item.instance.identity,
			Outcome:  outcome,
			Err:      err,
			Duration: elapsed,
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

// ingressClosure returns the set of identities Unwind stops in phase A: every
// instance whose primary value implements plugin.TrafficOpener, plus every
// instance that transitively depends on one. The set is upward-closed by
// construction -- it is built by walking dependents, never dependencies -- so
// stopping it in reverse start order can never stop a producer before every
// one of its consumers, which is what keeps phase A dependency-safe without
// consulting the graph a second time.
//
// A Drainer inside this closure is deliberately not drained in phase B: see
// Constructed.Unwind for why it is simply stopped in phase A instead.
func (constructed *Constructed) ingressClosure() map[plugin.Identity]bool {
	ingress := make(map[plugin.Identity]bool)
	for _, instance := range constructed.instances {
		if instance.IsIngress() {
			ingress[instance.identity] = true
		}
	}
	if len(ingress) == 0 {
		return ingress
	}
	// dependents inverts the dependsOn edges once, so growing the ingress set
	// below is a map lookup per instance rather than a rescan of every other
	// instance's dependency list.
	dependents := make(map[plugin.Identity][]plugin.Identity)
	for _, instance := range constructed.instances {
		for _, dependency := range instance.dependsOn {
			dependents[dependency] = append(dependents[dependency], instance.identity)
		}
	}
	// Breadth-first closure over "depends on": every instance already in the
	// set contributes its own dependents, until nothing new is added. Graph
	// order already guarantees no cycle, so this always terminates.
	queue := make([]plugin.Identity, 0, len(ingress))
	for identity := range ingress {
		queue = append(queue, identity)
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, dependent := range dependents[current] {
			if ingress[dependent] {
				continue
			}
			ingress[dependent] = true
			queue = append(queue, dependent)
		}
	}
	return ingress
}

// DrainOutcome classifies how one owned value's Drain attempt ended.
//
// Its vocabulary matches StopOutcome's rather than PreStopOutcome's: unlike
// the pre-stop phase, which starts every hook before waiting on any of them,
// the drain phase runs its hooks strictly one at a time in reverse start
// order (see Constructed.Unwind), so a hook reached after the phase deadline
// has already expired is a real omission the report must be able to name.
type DrainOutcome string

const (
	// DrainSkipped means the instance declares no Drain hook, or the drain
	// phase was never entered because xbc.drain_timeout is 0s.
	DrainSkipped DrainOutcome = "skipped"
	// DrainCompleted means Drain returned nil inside the phase budget.
	DrainCompleted DrainOutcome = "completed"
	// DrainFailed means Drain returned an error inside the phase budget. The
	// error is reported, and the unwind proceeds: a drain that failed has no
	// better outcome left to offer during a shutdown already under way.
	DrainFailed DrainOutcome = "failed"
	// DrainPanicked means Drain panicked; the panic was recovered and
	// reported, and the unwind proceeds.
	DrainPanicked DrainOutcome = "panicked"
	// DrainAbandoned means Drain ignored the phase deadline and was left
	// running. That is a plugin contract violation, not a runtime choice. The
	// abandoned hook keeps running and is therefore concurrent with the Stop
	// that follows it, which is exactly why Stop must be correct whether or
	// not Drain succeeded.
	DrainAbandoned DrainOutcome = "abandoned"
	// DrainNotAttempted means the drain phase's own deadline was already
	// spent when the walk reached this instance, so no Drain was started for
	// it. The phase runs its hooks one at a time in reverse start order (see
	// Constructed.Unwind), so an instance reached after the budget expired is
	// a real omission rather than a hook that chose to do nothing.
	DrainNotAttempted DrainOutcome = "not-attempted"
)

// DrainRecord is one instance's entry in a DrainReport.
type DrainRecord struct {
	Identity plugin.Identity
	Outcome  DrainOutcome
	Err      error
	// Duration is how long the phase waited on this instance: until Drain
	// returned, or until the phase deadline expired and it was abandoned. It
	// is zero for a skipped or not-attempted instance.
	Duration time.Duration
}

// DrainReport is the observable result of one drain phase. It records only
// the remaining (non-ingress) instances that declared a hook, in reverse
// start order, exactly as Constructed.Unwind drains them.
type DrainReport struct {
	Records []DrainRecord
}

// Identities returns the recorded identities with the given outcome, in the
// order Unwind drained them (reverse start order).
func (report DrainReport) Identities(outcome DrainOutcome) []plugin.Identity {
	var found []plugin.Identity
	for _, record := range report.Records {
		if record.Outcome == outcome {
			found = append(found, record.Identity)
		}
	}
	return found
}

// Empty reports whether this phase drained no hook at all: either
// xbc.drain_timeout was 0s, or no remaining instance declared one. A phase
// that ran but drained nothing and a phase that never ran are the same thing
// to every caller, so they share one answer -- the same choice
// PreStopReport.Empty already makes.
func (report DrainReport) Empty() bool { return len(report.Records) == 0 }

// Waited renders how long the phase waited on each instance it actually
// waited for, in the order they were drained. Abandoned and not-attempted
// instances are omitted: nothing was waited for there.
func (report DrainReport) Waited() []string {
	var labels []string
	for _, record := range report.Records {
		if record.Outcome == DrainAbandoned || record.Outcome == DrainNotAttempted {
			continue
		}
		labels = append(labels, record.Identity.String()+" "+record.Duration.String())
	}
	return labels
}

// drainBounded runs one instance's Drain hook, if it declares one, under the
// phase's shared deadline, abandoning it if it outlives that deadline. It is
// the Drain counterpart of stopBounded: unlike PreStop's hooks, which all
// start together, the drain phase visits one instance at a time (see
// Constructed.Unwind), so each hook can simply run to completion or be
// abandoned on its own goroutine exactly as a Stop is.
func (instance *Instance) drainBounded(deadline context.Context, budget time.Duration) (DrainOutcome, time.Duration, error) {
	if instance.lifecycle.drain == nil {
		return DrainSkipped, 0, nil
	}
	type drainResult struct {
		outcome DrainOutcome
		err     error
	}
	started := time.Now()
	done := make(chan drainResult, 1)
	go func() {
		panicked, err := instance.invokeClassified(StageDrain, func() error {
			return instance.lifecycle.drain(instance.value, deadline)
		})
		switch {
		case panicked:
			done <- drainResult{outcome: DrainPanicked, err: err}
		case err != nil:
			done <- drainResult{outcome: DrainFailed, err: err}
		default:
			done <- drainResult{outcome: DrainCompleted}
		}
	}()
	select {
	case result := <-done:
		return result.outcome, time.Since(started), result.err
	case <-deadline.Done():
	}
	select {
	case result := <-done:
		return result.outcome, time.Since(started), result.err
	default:
		return DrainAbandoned, time.Since(started), fmt.Errorf("xbc: plugin %s Drain did not return within drain budget %s; abandoning it", instance.identity, budget)
	}
}

// Unwind visits every owned instance in three phases under one shared budget
// and returns what actually happened. afterStop is called even after Stop
// errors and lets runtime enforce Stop→cancel→join for each Plugin's managed
// task scope.
//
// # The three phases
//
// Phase A stops the ingress closure (see Constructed.ingressClosure) in
// reverse start order, exactly as an unwind with no drain phase at all would.
// That set is upward-closed, so stopping it first can never stop a producer
// out from under a consumer that has not been stopped yet.
//
// Phase B drains every remaining instance that declares a Drain hook,
// strictly one at a time in reverse start order, under one shared deadline:
// drainBudget bounds the whole phase, not one hook, for the same reason
// ShutdownTimeout bounds the whole walk and not one Stop -- a per-instance
// budget would make the worst case scale with the number of drainers. Running
// drain sequentially rather than concurrently (unlike PreStop) is deliberate:
// a drainer that depends on another drainer must drain after it finishes
// accepting work and before it is stopped, and reverse start order is the
// only order that can promise that for every pair at once. drainBudget of 0
// skips the phase entirely -- no Drain hook is called -- which is the
// documented off switch.
//
// An instance inside the ingress closure that also declares a Drain hook is
// deliberately not drained here: phase A already stopped it, in the same
// reverse order a dedicated drain pass would use, and running Drain on an
// instance already mid-Stop would hand it two deadlines for one shutdown. An
// ingress-dependent value's natural place to finish in-flight work is its own
// Stop, which it already controls; phase B exists for the instances Stop
// would otherwise reach with no chance to hand work over first.
//
// Phase C stops every remaining instance in reverse start order, exactly as
// before this phase split existed.
//
// # Budgets
//
// deadline and budget bound the whole call, phases A and C included, exactly
// as they did before Drain existed. drainDeadline and drainBudget bound phase
// B alone, and drainDeadline is already the earlier of "now plus
// drain_timeout" and deadline -- the caller computes that once, because
// Unwind has no clock of its own to derive a second deadline from the first.
//
// # Idempotency and abandonment
//
// Exactly one Stop or Drain is in flight at a time across every phase. Once
// the overall budget is spent, no further Stop is started and no further
// Drain is started: an abandoned hook is still running, so starting the next
// one would let two hook bodies execute concurrently and would make phase
// order a scheduling outcome rather than a contract. The instances phase A or
// C no longer reaches are reported as not-attempted; the instances phase B no
// longer reaches are reported as not-attempted the same way. Releasing
// whatever an abandoned or not-attempted instance still holds is the caller's
// job, exactly as it always was.
//
// # What the returned error covers
//
// The returned error is about Stop (phases A and C) only: a failed, panicked,
// abandoned, or not-attempted Stop is joined into it, exactly as before Drain
// existed. A Drain failure, panic, abandonment, or not-attempted outcome is
// never joined into it; it is recorded in the returned DrainReport instead,
// which the caller is expected to inspect and log. That asymmetry mirrors
// PreStop's: Drain hands work over voluntarily, so a Drain that did not
// succeed has no better outcome to offer during a shutdown already under way,
// exactly as a failed PreStop does not fail the run it precedes.
func (constructed *Constructed) Unwind(deadline context.Context, budget time.Duration, afterStop func(plugin.Identity) error) (ShutdownReport, error) {
	report, _, err := constructed.unwindPhases(deadline, budget, deadline, 0, afterStop)
	return report, err
}

// UnwindWithDrain is Unwind with the drain phase (phase B) enabled: see
// Constructed.Unwind for the complete three-phase contract. drainDeadline is
// the phase's own deadline -- min(now+drain_timeout, deadline), computed by
// the caller -- and drainBudget is drain_timeout itself, reported alongside
// each record exactly as PreStop's budget is. drainBudget of 0 skips phase B
// entirely and produces an empty DrainReport, which is what keeps an
// application with no Drainer anywhere byte-for-byte identical to one built
// before the stage existed.
func (constructed *Constructed) UnwindWithDrain(deadline context.Context, budget time.Duration, drainDeadline context.Context, drainBudget time.Duration, afterStop func(plugin.Identity) error) (ShutdownReport, DrainReport, error) {
	return constructed.unwindPhases(deadline, budget, drainDeadline, drainBudget, afterStop)
}

func (constructed *Constructed) unwindPhases(
	deadline context.Context,
	budget time.Duration,
	drainDeadline context.Context,
	drainBudget time.Duration,
	afterStop func(plugin.Identity) error,
) (ShutdownReport, DrainReport, error) {
	var report ShutdownReport
	var drainReport DrainReport
	if constructed == nil {
		return report, drainReport, nil
	}
	ingress := constructed.ingressClosure()
	var errs []error

	// Phase A: stop the ingress closure, in reverse start order, exactly as
	// an unwind with no drain phase would.
	for index := len(constructed.instances) - 1; index >= 0; index-- {
		instance := constructed.instances[index]
		if !ingress[instance.identity] {
			continue
		}
		record, taskErr, stopErr := constructed.stopOne(instance, deadline, budget, afterStop, &report)
		if stopErr != nil {
			errs = append(errs, stopErr)
		}
		if taskErr != nil {
			errs = append(errs, taskErr)
		}
		_ = record
	}

	// Phase B: drain every remaining instance that declares a hook, strictly
	// one at a time in reverse start order, under the shared phase deadline.
	// drainBudget of 0 is the documented off switch: no Drain is called and no
	// record is produced, which is what reportDrain depends on to stay silent
	// for an application that declares no Drainer at all.
	//
	// A Drain failure, panic, or abandonment is recorded in drainReport but
	// deliberately not joined into the error this method returns: Drain hands
	// work over voluntarily, exactly as PreStop retracts participation
	// voluntarily, and PreStop's own joined error is dropped for the identical
	// reason (see runtime.runPreStopPhase). The caller's report is the record;
	// the returned error stays about Stop, which is the stage whose outcome
	// still decides whether cleanup finished.
	if drainBudget > 0 {
		for index := len(constructed.instances) - 1; index >= 0; index-- {
			instance := constructed.instances[index]
			if ingress[instance.identity] || !instance.HasDrain() {
				continue
			}
			if drainDeadline.Err() != nil {
				drainReport.Records = append(drainReport.Records, DrainRecord{
					Identity: instance.identity,
					Outcome:  DrainNotAttempted,
					Err:      fmt.Errorf("xbc: plugin %s Drain was not attempted: the %s drain budget was already spent", instance.identity, drainBudget),
				})
				continue
			}
			outcome, elapsed, err := instance.drainBounded(drainDeadline, drainBudget)
			drainReport.Records = append(drainReport.Records, DrainRecord{
				Identity: instance.identity,
				Outcome:  outcome,
				Err:      err,
				Duration: elapsed,
			})
		}
	}

	// Phase C: stop every remaining instance in reverse start order, exactly
	// as before this phase split existed.
	for index := len(constructed.instances) - 1; index >= 0; index-- {
		instance := constructed.instances[index]
		if ingress[instance.identity] {
			continue
		}
		_, taskErr, stopErr := constructed.stopOne(instance, deadline, budget, afterStop, &report)
		if stopErr != nil {
			errs = append(errs, stopErr)
		}
		if taskErr != nil {
			errs = append(errs, taskErr)
		}
	}

	return report, drainReport, errors.Join(errs...)
}

// stopOne runs one instance's bounded Stop and folds its outcome into report,
// exactly as the single-phase walk used to do inline. It is shared by phases A
// and C so the not-attempted/abandoned bookkeeping has exactly one
// implementation regardless of which phase an instance's Stop belongs to.
func (constructed *Constructed) stopOne(
	instance *Instance,
	deadline context.Context,
	budget time.Duration,
	afterStop func(plugin.Identity) error,
	report *ShutdownReport,
) (StopRecord, error, error) {
	if deadline.Err() != nil {
		record := StopRecord{Identity: instance.identity, Outcome: StopNotAttempted}
		report.Records = append(report.Records, record)
		return record, nil, fmt.Errorf("xbc: plugin %s Stop was not attempted: the %s shutdown budget was already spent", instance.identity, budget)
	}
	report.Attempted = append(report.Attempted, instance.identity)
	outcome, elapsed, err := instance.stopBounded(deadline, budget)
	record := StopRecord{Identity: instance.identity, Outcome: outcome, Err: err, Duration: elapsed}
	var taskErr error
	if outcome != StopAbandoned {
		report.Completed = append(report.Completed, instance.identity)
		if afterStop != nil {
			if callErr := afterStop(instance.identity); callErr != nil {
				record.TaskErr = callErr
				taskErr = callErr
			}
		}
	}
	report.Records = append(report.Records, record)
	return record, taskErr, err
}
