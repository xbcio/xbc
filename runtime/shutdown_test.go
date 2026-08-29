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

// TestShutdownStopsInReverseDependencyOrder pins §5.3's "逆拓扑序" clause: the
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

	require.NoError(t, res.err, "两个 Stop 都成功返回，unwind 不应报告任何错误")
	assert.Equal(t, 0, res.code)
	// Asserting the exact slice, not merely that both names appear, is what
	// gives this test discriminating power: an implementation that walked
	// plugins in *forward* (init/topological) order instead of reverse would
	// still produce a slice containing both names, just in the wrong order,
	// and a weaker assertion would not catch it.
	assert.Equal(t, []string{"downstream", "upstream"}, order,
		"downstream 依赖 upstream，必须先于 upstream 被 Stop")
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
		"Stop 被调用的那一刻，该插件自己的任务 context 必须尚未被 cancel；"+
			"如果框架改成先全局 cancel 所有任务再 Stop，这里会观察到 context.Canceled")
	// Cancellation still has to happen -- just after Stop returns, not
	// before -- so once the whole run has finished the task context must be
	// done. Without this second assertion, a framework that simply never
	// cancelled per-plugin task contexts at all would also pass the check
	// above.
	assert.Error(t, inst.taskCtx.Err(),
		"Stop 返回之后框架必须 cancel 该插件的任务 context，否则托管任务永远不会被回收")
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

	assert.NotEqual(t, 0, res.code, "有插件的 Stop 未在预算内返回，退出码必须非零")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "hanger",
		"错误必须点名卡住的插件，而不是笼统地报告一次关闭失败")

	select {
	case <-dep.stopped:
		// dependency 的 Stop 仍被调用过，即便共享预算已经被 hanger 耗尽 --
		// 这正是 §5.3「预算耗尽后降级为继续调用但不等待」的可观察结果。
	case <-time.After(testTimeout):
		t.Fatal("hanger 耗尽共享预算后，依赖链上更靠后的插件的 Stop 也必须被调用过一次")
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

	assert.NotEqual(t, 0, res.code, "托管任务永远不会退出，run 仍必须返回，且退出码必须非零")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ignorer",
		"托管任务未按时退出时，错误必须点名任务所属的插件")
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
	panic("shutdown_test: 故意 panic，用于验证 Stop 的 panic 会被 recover")
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
		"聚合错误必须点名 panic 所在的插件")

	select {
	case <-dep.stopped:
	case <-time.After(testTimeout):
		t.Fatal("panicker 的 Stop panic 之后，它依赖的插件仍必须被 Stop")
	}
}

// --- multiple Stop errors aggregate, none is dropped ----------------------

// shutdownErrStopA and shutdownErrStopB are distinct sentinels so the
// aggregation test below can use errors.Is to prove *both* survive into the
// final error, rather than merely that *some* error came back.
var (
	shutdownErrStopA = errors.New("shutdown_test: 插件 a 的 Stop 失败")
	shutdownErrStopB = errors.New("shutdown_test: 插件 b 的 Stop 失败")
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
	assert.True(t, errors.Is(res.err, shutdownErrStopA), "两个插件的 Stop 错误都必须出现在聚合错误里（缺了 a）")
	assert.True(t, errors.Is(res.err, shutdownErrStopB), "两个插件的 Stop 错误都必须出现在聚合错误里（缺了 b）")
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
		"GoCritical 任务在自己插件的 Stop 内被主动放行退出，这是预期中的优雅停止，退出码必须是 0")
	// Asserting against the actual log output, not against a.stopReason,
	// is what makes this a real observable-behaviour test rather than a
	// tautology about the variable the log line is derived from.
	assert.False(t, cap.containsMessage(t, "收到 critical 信号"),
		"这次关闭不应被判定成 critical failure：判据必须是应用级「关机已开始」标志，"+
			"不能是该插件自己的任务 context 是否已被 cancel")
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
	require.NoError(t, res.err, "该插件的 Stop 从不返回错误，unwind 不应报告失败")
	assert.Equal(t, int32(1), inst.stopCalls.Load(),
		"无论 signal 与 critical 谁先到达，Closer.Stop 都只能被调用一次")
}
