package assembly

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"
	"time"

	"github.com/xbcio/xbc/internal/pluginmodel"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

const defaultShutdownTimeout = 15 * time.Second

// ContextFactory creates the lifecycle Context for one planned identity.
type ContextFactory func(plugin.Identity, log.Logger) *plugin.Context

// ConstructOptions configures the sole resource-owning transaction.
type ConstructOptions struct {
	ContextFactory  ContextFactory
	ShutdownTimeout time.Duration
}

// Instance is one successfully factory-returned, framework-owned primary
// value with lifecycle metadata compiled before construction began.
type Instance struct {
	identity  plugin.Identity
	value     any
	context   *plugin.Context
	lifecycle lifecycleDescriptor

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
		value, err := invokeFactory(planned, buildContext)
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
			identity:  identity,
			value:     value,
			context:   lifecycleContext,
			lifecycle: planned.lifecycle,
		}
		// Ownership transfers before Init. From here onward this instance is in
		// every rollback set, including an Init failure or panic.
		constructed.instances = append(constructed.instances, instance)
		constructed.byIdentity[identity] = instance

		if err := instance.invoke("Init", func() error {
			if instance.lifecycle.init == nil {
				return nil
			}
			return instance.lifecycle.init(instance.value, instance.context)
		}); err != nil {
			return nil, joinConstructionFailure(err, constructed, options.ShutdownTimeout)
		}
	}
	return constructed, nil
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
	return instance.invoke("Migrate", func() error {
		if instance.lifecycle.migrate == nil {
			return nil
		}
		return instance.lifecycle.migrate(instance.value, instance.context)
	})
}

// InvokeStart runs one frozen start descriptor under a panic boundary.
func (instance *Instance) InvokeStart() error {
	return instance.invoke("Start", func() error {
		if instance.lifecycle.start == nil {
			return nil
		}
		return instance.lifecycle.start(instance.value, instance.context)
	})
}

// InvokeTrafficPreparation runs one frozen traffic-preparation descriptor.
func (instance *Instance) InvokeTrafficPreparation() error {
	return instance.invoke("OpenTraffic", func() error {
		if instance.lifecycle.openTraffic == nil {
			return nil
		}
		return instance.lifecycle.openTraffic(instance.value, instance.context)
	})
}

func (instance *Instance) invoke(stage string, fn func() error) error {
	_, err := instance.invokeClassified(stage, fn)
	return err
}

// invokeClassified runs one lifecycle stage under a panic boundary and tells
// a recovered panic apart from a returned error without parsing diagnostics.
func (instance *Instance) invokeClassified(stage string, fn func() error) (panicked bool, err error) {
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
	_, err := instance.stopBounded(deadline, budget)
	return err
}

type stopResult struct {
	outcome StopOutcome
	err     error
}

func (instance *Instance) stopBounded(deadline context.Context, budget time.Duration) (StopOutcome, error) {
	instance.stopMu.Lock()
	if instance.stopped {
		instance.stopMu.Unlock()
		return StopSkipped, nil
	}
	instance.stopped = true
	instance.stopMu.Unlock()
	if instance.lifecycle.stop == nil {
		return StopSkipped, nil
	}
	done := make(chan stopResult, 1)
	go func() {
		panicked, err := instance.invokeClassified("Stop", func() error {
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
		return result.outcome, result.err
	case <-deadline.Done():
	}
	// The budget is spent. Read done once more without blocking before
	// declaring the Stop abandoned: a select whose cases are both ready picks
	// one at random, so without this second read a Stop that finished in the
	// same instant as the deadline would be classified by the scheduler
	// rather than by what it actually did.
	select {
	case result := <-done:
		return result.outcome, result.err
	default:
		return StopAbandoned, fmt.Errorf("xbc: plugin %s Stop did not return within shutdown budget %s; abandoning it", instance.identity, budget)
	}
}

// Unwind visits every owned instance in reverse graph order under one shared
// budget and returns what actually happened. afterStop is called even after
// Stop errors and lets runtime enforce Stop→cancel→join for each Plugin's
// managed task scope.
//
// Exactly one Stop is in flight at a time. Once the shared budget is spent the
// walk starts no further Stop and calls no further afterStop: an abandoned
// Stop is still running, so launching the next one would let two Stop bodies
// execute concurrently and would make reverse order a scheduling outcome
// rather than a contract. The instances the walk no longer reaches are
// reported as not-attempted; releasing whatever they still hold is the
// caller's job.
func (constructed *Constructed) Unwind(deadline context.Context, budget time.Duration, afterStop func(plugin.Identity) error) (ShutdownReport, error) {
	var report ShutdownReport
	if constructed == nil {
		return report, nil
	}
	var errs []error
	for index := len(constructed.instances) - 1; index >= 0; index-- {
		instance := constructed.instances[index]
		if deadline.Err() != nil {
			report.Records = append(report.Records, StopRecord{
				Identity: instance.identity,
				Outcome:  StopNotAttempted,
			})
			errs = append(errs, fmt.Errorf("xbc: plugin %s Stop was not attempted: the %s shutdown budget was already spent", instance.identity, budget))
			continue
		}
		report.Attempted = append(report.Attempted, instance.identity)
		outcome, err := instance.stopBounded(deadline, budget)
		record := StopRecord{Identity: instance.identity, Outcome: outcome, Err: err}
		if err != nil {
			errs = append(errs, err)
		}
		if outcome != StopAbandoned {
			report.Completed = append(report.Completed, instance.identity)
			if afterStop != nil {
				if taskErr := afterStop(instance.identity); taskErr != nil {
					record.TaskErr = taskErr
					errs = append(errs, taskErr)
				}
			}
		}
		report.Records = append(report.Records, record)
	}
	return report, errors.Join(errs...)
}
