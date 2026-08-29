package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// This file pins the shutdown contract described in package-layout design
// §5.3-§5.5: reverse dependency order, the per-plugin Stop-before-cancel
// interleaving, bounded unwind in the face of a Stop that never returns or a
// managed task that ignores its context, panic recovery, error aggregation,
// the application-level (not per-plugin) "was this critical return
// requested" judgement, and idempotence of Closer.Stop under a race between
// two shutdown triggers.

// --- §5.3 reverse dependency order ----------------------------------------

// shutdownUpstream is the dependency side of a two-node graph: it must still
// be alive -- and get its own Closer.Stop call -- only after everything that
// depends on it has already been stopped.
type shutdownUpstream struct {
	plugin.Base
	onStop func()
}

var _ plugin.Closer = (*shutdownUpstream)(nil)

func (p *shutdownUpstream) Stop(context.Context) error {
	if p.onStop != nil {
		p.onStop()
	}
	return nil
}

// shutdownDownstream declares a hard dependency on shutdownUpstream by
// its stable plugin key, which is what gives the assembly graph something
// non-trivial to topologically sort -- exactly the mechanism
// package assembly's own order tests use (see container_test.go's
// orderProducer/orderConsumer).
type shutdownDownstream struct {
	plugin.Base
	onStop func()
}

var (
	_ plugin.Closer   = (*shutdownDownstream)(nil)
	_ plugin.Declarer = (*shutdownDownstream)(nil)
)

func (p *shutdownDownstream) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("upstream")}}
}

func (p *shutdownDownstream) Stop(context.Context) error {
	if p.onStop != nil {
		p.onStop()
	}
	return nil
}

// TestShutdownStopsInReverseDependencyOrder pins §5.3's "inverse topological order" clause: the
// plugin that depends on another must be Stopped first, so that by the time
// the dependency itself is Stopped, nothing still using it is left running.
func TestShutdownStopsInReverseDependencyOrder(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
		}
	}

	app := newTestApp(t,
		def("upstream", func() plugin.Plugin { return &shutdownUpstream{onStop: record("upstream")} }),
		def("downstream", func() plugin.Plugin { return &shutdownDownstream{onStop: record("downstream")} }),
		liveness("live"),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	require.NoError(t, res.err, "Both Stop calls return successfully; unwind should not report any errors")
	assert.Equal(t, 0, res.code)
	// Asserting the exact slice, not merely that both names appear, is what
	// gives this test discriminating power: an implementation that walked
	// plugins in *forward* (init/topological) order instead of reverse would
	// still produce a slice containing both names, just in the wrong order,
	// and a weaker assertion would not catch it.
	assert.Equal(t, []string{"downstream", "upstream"}, order,
		"Downstream depends on upstream; downstream must be stopped before upstream")
}

// --- §5.3 the interleaving pin: Stop before that plugin's own cancel -----

// shutdownInterleaved pins the single most important ordering promise in
// §5.3: Closer.Stop must run while that plugin's own managed task is still
// alive and able to make progress, and the task's context must only be
// cancelled once Stop has returned. A web.Server-shaped Stop that drains
// in-flight requests through its own serving goroutine depends on exactly
// this.
type shutdownInterleaved struct {
	plugin.Base

	release     chan struct{}
	taskStarted chan struct{}
	taskDone    chan struct{}
	taskCtx     context.Context

	// ctxErrObservedInStop is written once, inside Stop, before it does
	// anything else. It is read only after run() has returned, so no
	// synchronization beyond that ordering is needed.
	ctxErrObservedInStop error
}

var _ plugin.Closer = (*shutdownInterleaved)(nil)

func (p *shutdownInterleaved) Init(ctx *plugin.Context) error {
	p.release = make(chan struct{})
	p.taskStarted = make(chan struct{})
	p.taskDone = make(chan struct{})

	ctx.Go(func(taskCtx context.Context) {
		p.taskCtx = taskCtx
		close(p.taskStarted)
		// A real drain loop: block until told to stop, either by the
		// plugin's own Stop (the release channel) or by the task context
		// being cancelled. Selecting on both keeps this goroutine from
		// leaking even if the interleaving under test were wrong.
		select {
		case <-p.release:
		case <-taskCtx.Done():
		}
		close(p.taskDone)
	})
	return nil
}

// Stop records whether its own task's context is already cancelled *before*
// it signals that task to finish. A framework that cancelled every task
// context up front and only then called Stop -- the exact inversion §5.3
// forbids -- would make ctxErrObservedInStop non-nil here, because
// taskCtx.Done() would already be closed by the time this line runs.
func (p *shutdownInterleaved) Stop(context.Context) error {
	<-p.taskStarted
	p.ctxErrObservedInStop = p.taskCtx.Err()
	close(p.release)
	<-p.taskDone
	return nil
}

func TestShutdownStopRunsBeforeItsOwnTaskContextIsCancelled(t *testing.T) {
	var inst *shutdownInterleaved
	app := newTestApp(t, def("interleaved", func() plugin.Plugin {
		inst = &shutdownInterleaved{}
		return inst
	}))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)
	require.NoError(t, inst.ctxErrObservedInStop,
		"At the moment Stop is called, the plugin's own task context must not yet be canceled;"+
			"If the framework changes to first globally cancel all tasks before Stop, this will observe context.Canceled")
	// Cancellation still has to happen -- just after Stop returns, not
	// before -- so once the whole run has finished the task context must be
	// done. Without this second assertion, a framework that simply never
	// cancelled per-plugin task contexts at all would also pass the check
	// above.
	assert.Error(t, inst.taskCtx.Err(),
		"After Stop returns, the framework must cancel the plugin's task context, otherwise the managed task will never be reclaimed")
}

// --- §5.3 bounded unwind: a Stop that never returns -----------------------

// shutdownHangsDependency is the plugin a hanging Stop depends on. Because
// dependency order puts it *before* the hanging plugin during Init, the
// reverse walk during unwind reaches it *after* the hanging plugin -- so it
// is exactly the plugin whose Stop must still be attempted once the shared
// budget the hanger burned through is gone.
type shutdownHangsDependency struct {
	plugin.Base
	stopped chan struct{}
}

var _ plugin.Closer = (*shutdownHangsDependency)(nil)

func (p *shutdownHangsDependency) Stop(context.Context) error {
	close(p.stopped)
	return nil
}

// shutdownHangingConsumer never returns from Stop -- select{} blocks
// forever, modelling a Closer that ignores the deadline context it was
// handed entirely. The framework, not the plugin, has to make progress here.
type shutdownHangingConsumer struct {
	plugin.Base
}

var (
	_ plugin.Closer   = (*shutdownHangingConsumer)(nil)
	_ plugin.Declarer = (*shutdownHangingConsumer)(nil)
)

func (p *shutdownHangingConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("dependency")}}
}

func (p *shutdownHangingConsumer) Stop(context.Context) error {
	select {}
}

func TestShutdownBoundedWhenStopNeverReturns(t *testing.T) {
	dep := &shutdownHangsDependency{stopped: make(chan struct{})}

	app := newTestApp(t,
		def("hanger", func() plugin.Plugin { return &shutdownHangingConsumer{} }),
		def("dependency", func() plugin.Plugin { return dep }),
		liveness("live"),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)

	// The whole run -- not just the hung Stop's own goroutine -- must return
	// within testTimeout. If the framework ever fell back to a plain
	// `closer.Stop(ctx)` call awaited synchronously, this would wedge the
	// test binary instead of failing it; awaitResult's own bound is what
	// turns that failure mode into a reported test failure.
	res := awaitResult(t, done)

	assert.NotEqual(t, 0, res.code, "If the plugin's Stop does not return within the budget, the exit code must be non-zero")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "hanger",
		"The error must name the stalled plugin, not generically report a single shutdown failure")

	select {
	case <-dep.stopped:
		// dependency's Stop is still called, even if the shared budget has been exhausted by hanger --
		// This is exactly the observable result of §5.3 ``degrading to continue calling but not waiting after budget exhaustion''
	case <-time.After(testTimeout):
		t.Fatal("After the hanger exhausts the shared budget, the Stop of plugins further down the dependency chain must also be called once")
	}
}

// --- §5.3 bounded unwind: a managed task that ignores its context --------

// shutdownIgnoresContext models a plugin bug: its managed task never selects
// on the context handed to it at all. This is the case waitTasks's own bound
// exists to survive.
type shutdownIgnoresContext struct {
	plugin.Base
}

func (p *shutdownIgnoresContext) Init(ctx *plugin.Context) error {
	ctx.Go(func(context.Context) {
		select {}
	})
	return nil
}

func TestShutdownBoundedWhenManagedTaskIgnoresContext(t *testing.T) {
	app := newTestApp(t, def("ignorer", func() plugin.Plugin { return &shutdownIgnoresContext{} }))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	assert.NotEqual(t, 0, res.code, "The managed task will never exit, run must still return, and the exit code must be non-zero")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ignorer",
		"When the managed task does not exit on time, the error must name the plugin to which the task belongs")
}

// --- panic recovery ---------------------------------------------------------

// shutdownPanicsDependency is the plugin a panicking Stop depends on, used
// the same way shutdownHangsDependency is above: it must still be Stopped
// after the panic, proving the panic did not abort the rest of the unwind.
type shutdownPanicsDependency struct {
	plugin.Base
	stopped chan struct{}
}

var _ plugin.Closer = (*shutdownPanicsDependency)(nil)

func (p *shutdownPanicsDependency) Stop(context.Context) error {
	close(p.stopped)
	return nil
}

type shutdownPanickingConsumer struct {
	plugin.Base
}

var (
	_ plugin.Closer   = (*shutdownPanickingConsumer)(nil)
	_ plugin.Declarer = (*shutdownPanickingConsumer)(nil)
)

func (p *shutdownPanickingConsumer) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{plugin.RefTo("dependency")}}
}

func (p *shutdownPanickingConsumer) Stop(context.Context) error {
	panic("shutdown_test: Intentionally panic, used to verify that the panic from Stop will be recovered")
}

func TestShutdownStopPanicIsRecoveredAndDoesNotBlockOtherPlugins(t *testing.T) {
	dep := &shutdownPanicsDependency{stopped: make(chan struct{})}

	app := newTestApp(t,
		def("panicker", func() plugin.Plugin { return &shutdownPanickingConsumer{} }),
		def("dependency", func() plugin.Plugin { return dep }),
		liveness("live"),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)

	// A panic escaping stopBounded's goroutine and reaching this one would
	// crash the whole test binary, not merely fail an assertion -- so simply
	// reaching the lines below already proves recover() did its job; it is
	// not one of several outcomes being distinguished after the fact.
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	assert.NotEqual(t, 0, res.code)
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "panicker",
		"The aggregated error must name the plugin where the panic occurred")

	select {
	case <-dep.stopped:
	case <-time.After(testTimeout):
		t.Fatal("After the panic from the panicker's Stop, its dependent plugins must still be stopped")
	}
}

// --- multiple Stop errors aggregate, none is dropped ----------------------

// shutdownErrStopA and shutdownErrStopB are distinct sentinels so the
// aggregation test below can use errors.Is to prove *both* survive into the
// final error, rather than merely that *some* error came back.
var (
	shutdownErrStopA = errors.New("shutdown_test: Plugin a's Stop failed")
	shutdownErrStopB = errors.New("shutdown_test: Plugin b's Stop failed")
)

type shutdownFailsWith struct {
	plugin.Base
	err error
}

var _ plugin.Closer = (*shutdownFailsWith)(nil)

func (p *shutdownFailsWith) Stop(context.Context) error {
	return p.err
}

func TestShutdownAggregatesMultipleStopErrors(t *testing.T) {
	app := newTestApp(t,
		def("plugin-a", func() plugin.Plugin { return &shutdownFailsWith{err: shutdownErrStopA} }),
		def("plugin-b", func() plugin.Plugin { return &shutdownFailsWith{err: shutdownErrStopB} }),
		liveness("live"),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	assert.NotEqual(t, 0, res.code)
	require.Error(t, res.err)
	// errors.Is walks the %w chain wrapStopError builds and the errors.Join
	// doUnwind runs on top of it. A weaker "err != nil" assertion would still
	// pass an implementation that kept only the last plugin's failure and
	// silently dropped the first one.
	assert.True(t, errors.Is(res.err, shutdownErrStopA), "Both plugins' Stop errors must appear in the aggregated error (missing a)")
	assert.True(t, errors.Is(res.err, shutdownErrStopB), "Both plugins' Stop errors must appear in the aggregated error (missing b)")
}

// --- §5.5 the global, not per-plugin, "was this critical return requested" flag ---

// shutdownCriticalSelfStop is the pinned §5.5 scenario verbatim: a plugin
// whose Stop makes its own GoCritical task return by itself. A judge that
// derives "was this expected" from *this plugin's own* task context (rather
// than a single application-level "shutdown has started" flag) would see the
// task return while its own context is still live -- because Stop runs
// before that cancellation, per §5.3 -- and misjudge the return as
// unrequested, escalating a clean shutdown into a critical failure.
type shutdownCriticalSelfStop struct {
	plugin.Base
	release chan struct{}
}

var _ plugin.Closer = (*shutdownCriticalSelfStop)(nil)

func (p *shutdownCriticalSelfStop) Init(ctx *plugin.Context) error {
	p.release = make(chan struct{})
	ctx.GoCritical(func(taskCtx context.Context) {
		select {
		case <-p.release:
		case <-taskCtx.Done():
		}
	})
	return nil
}

func (p *shutdownCriticalSelfStop) Stop(context.Context) error {
	close(p.release)
	return nil
}

func TestShutdownCriticalTaskReturningFromOwnStopIsNotAFailure(t *testing.T) {
	extra, cap := captureLogs(t)

	app := newTestApp(t, def("self-stopping", func() plugin.Plugin {
		return &shutdownCriticalSelfStop{}
	}))

	// captureLogs's extra and quietConfig both write a top-level "log:" key,
	// so they cannot simply be concatenated -- see harness_test.go's
	// captureLogs doc comment. Building the YAML by hand here keeps the file
	// valid while still getting both the file sink and a short budget.
	done := runAsync(app, writeConfig(t, extra+"xbc:\n  shutdown_timeout: 500ms\n")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)

	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code,
		"A GoCritical task actively allows itself to exit during its own plugin's Stop, this is an expected graceful shutdown, exit code must be 0")
	// Asserting against the actual log output, not against a.stopReason,
	// is what makes this a real observable-behaviour test rather than a
	// tautology about the variable the log line is derived from.
	assert.False(t, cap.containsMessage(t, "received critical signal"),
		"This shutdown should not be considered a critical failure: the criterion must be the application-level ``shutdown has started'' flag,"+
			"Not whether the plugin's own task context has been canceled")
}

// --- Closer.Stop is called at most once, even under a race --------------

// shutdownIdempotentStop's GoCritical task blocks on release, so the test can
// make it return at (as near as the scheduler allows) the same instant an
// independent, explicit requestStop(signal) call is made -- racing the
// signal path against the critical-escalation path that the task's own
// return triggers.
type shutdownIdempotentStop struct {
	plugin.Base
	release   chan struct{}
	stopCalls atomic.Int32
}

var _ plugin.Closer = (*shutdownIdempotentStop)(nil)

func (p *shutdownIdempotentStop) Init(ctx *plugin.Context) error {
	p.release = make(chan struct{})
	ctx.GoCritical(func(context.Context) {
		<-p.release
	})
	return nil
}

func (p *shutdownIdempotentStop) Stop(context.Context) error {
	p.stopCalls.Add(1)
	return nil
}

func TestShutdownUnwindIdempotentUnderConcurrentSignalAndCritical(t *testing.T) {
	inst := &shutdownIdempotentStop{}
	app := newTestApp(t, def("racer", func() plugin.Plugin { return inst }))

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)

	// Two independent triggers for the same shutdown, fired without any
	// ordering between them: the explicit signal on this goroutine, and the
	// critical escalation that releasing the task provokes (its GoCritical
	// function returning unprompted). requestStop's sync.Once has to hold
	// under this race, not merely under a call made once, sequentially.
	go app.requestStop(stopReasonSignal)
	close(inst.release)

	res := awaitResult(t, done)
	require.NoError(t, res.err, "The plugin's Stop never returns an error, unwind should not report a failure")
	assert.Equal(t, int32(1), inst.stopCalls.Load(),
		"Regardless of which arrives first, signal or critical, Closer.Stop can only be called once")
}
