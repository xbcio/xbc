package async

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/tasks"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// The handles below are package-level because tasks.Method claims its name
// process-wide and panics on a duplicate definition, so defining them inside
// a test would explode the second time the package's tests run in one binary.

// contextMarkerKey carries the value the context-propagation test records.
type contextMarkerKey struct{}

// call is one observed invocation of taskService.handle.
type call struct {
	arg    string
	marker string
}

// taskService is the plugin value the local-executor tests hang the method
// task on. ran receives each call, so the tests synchronize on real execution
// instead of sleeping.
type taskService struct {
	ran   chan call
	fail  error
	crash bool
}

func (s *taskService) handle(ctx context.Context, arg string) error {
	if s.crash {
		panic("task service panic")
	}
	marker, _ := ctx.Value(contextMarkerKey{}).(string)
	s.ran <- call{arg: arg, marker: marker}
	return s.fail
}

var testDispatch = tasks.Method("test.async.dispatch", (*taskService).handle)

// testFunction is a function task the table tests expect every Pool to hold.
// It validates its own argument so the tests can drive its encoded handler
// synchronously and read the outcome from the returned error.
var testFunction = tasks.New("test.async.function", func(ctx context.Context, arg int) error {
	if arg != 7 {
		return errors.New("test.async.function received an argument other than 7")
	}
	return nil
})

// testProvider stands in for a plugin exporting tasks.Provider.
type testProvider struct {
	bindings []tasks.Binding
	panics   bool
}

func (p testProvider) Tasks() []tasks.Binding {
	if p.panics {
		panic("provider panic")
	}
	return p.bindings
}

func providerEntry(identity string, provider tasks.Provider) plugin.Entry[tasks.Provider] {
	return plugin.Entry[tasks.Provider]{Identity: plugin.Identity{Plugin: plugin.Key(identity)}, Value: provider}
}

// boundDispatch binds the package's method task to a fresh receiver.
func boundDispatch(service *taskService) []tasks.Binding {
	return tasks.Bind(service, testDispatch)
}

// newProviderPool builds an open, directly usable Pool whose table holds
// service's method binding, reading providers through the same construction
// path the Definition uses.
func newProviderPool(t *testing.T, cfg Config, logger log.Logger, identity string, service *taskService) *Pool {
	t.Helper()
	pool, err := newPool(cfg, logger, []plugin.Entry[tasks.Provider]{
		providerEntry(identity, testProvider{bindings: boundDispatch(service)}),
	})
	require.NoError(t, err)
	pool.open()
	return pool
}

// receiveCall waits for one observed invocation.
func receiveCall(t *testing.T, service *taskService) call {
	t.Helper()
	select {
	case observed := <-service.ran:
		return observed
	case <-time.After(2 * time.Second):
		t.Fatal("the task handler did not run")
		return call{}
	}
}

// recorded reports whether the logger saw an entry with this message, at any
// level.
func (l *captureLogger) recorded(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.msg == msg {
			return true
		}
	}
	return false
}

func TestBuildTaskTableHoldsFunctionTasksAndProviderBindings(t *testing.T) {
	service := &taskService{ran: make(chan call, 1)}
	table, err := buildTaskTable([]plugin.Entry[tasks.Provider]{providerEntry("tasks-table-test", testProvider{bindings: boundDispatch(service)})})
	require.NoError(t, err)

	require.Contains(t, table, "test.async.function")
	require.Contains(t, table, "test.async.dispatch")

	// The encoded handler decodes the argument and fails through to the
	// function task's own validation and error.
	require.NoError(t, table["test.async.function"].Handler()(context.Background(), []byte("7")))
	require.Error(t, table["test.async.function"].Handler()(context.Background(), []byte("8")))

	// The provider binding decodes and calls the receiver's method.
	require.NoError(t, table["test.async.dispatch"].Handler()(context.Background(), []byte(`"hello"`)))
	assert.Equal(t, call{arg: "hello"}, receiveCall(t, service))

	// A payload the argument type cannot decode is a payload error.
	err = table["test.async.dispatch"].Handler()(context.Background(), []byte("{not json"))
	require.Error(t, err)
	assert.ErrorIs(t, err, tasks.ErrPayload)
}

func TestBuildTaskTableRejectsConflictingAndInvalidProviders(t *testing.T) {
	first := &taskService{ran: make(chan call, 1)}
	second := &taskService{ran: make(chan call, 1)}

	_, err := buildTaskTable([]plugin.Entry[tasks.Provider]{
		providerEntry("first-provider", testProvider{bindings: boundDispatch(first)}),
		providerEntry("second-provider", testProvider{bindings: boundDispatch(second)}),
	})
	require.Error(t, err)
	for _, want := range []string{`"test.async.dispatch"`, "first-provider", "second-provider"} {
		assert.Truef(t, strings.Contains(err.Error(), want), "error %q does not mention %q", err.Error(), want)
	}

	_, err = buildTaskTable([]plugin.Entry[tasks.Provider]{providerEntry("invalid-provider", testProvider{bindings: []tasks.Binding{{}}})})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid-provider")
	assert.Contains(t, err.Error(), "without a name")

	_, err = buildTaskTable([]plugin.Entry[tasks.Provider]{providerEntry("panicking-provider", testProvider{panics: true})})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panicked")
	assert.Contains(t, err.Error(), "panicking-provider")
}

func TestLocalSubmitDecodesAndRunsTheHandler(t *testing.T) {
	service := &taskService{ran: make(chan call, 1)}
	pool := newProviderPool(t, DefaultConfig(), nil, "local-submit", service)
	executor := localExecutor{pool: pool, bindings: pool.bindings}

	require.NoError(t, executor.Submit(context.Background(), "test.async.dispatch", []byte(`"hello"`)))
	assert.Equal(t, call{arg: "hello"}, receiveCall(t, service))
}

func TestLocalSubmitReportsAcceptanceEvenWhenTheHandlerFails(t *testing.T) {
	service := &taskService{ran: make(chan call, 1), fail: errors.New("handler refused")}
	pool := newProviderPool(t, DefaultConfig(), nil, "local-submit-failing", service)
	executor := localExecutor{pool: pool, bindings: pool.bindings}

	// A handler failure is the accepting Submit's business only as a log
	// entry: acceptance was already reported.
	require.NoError(t, executor.Submit(context.Background(), "test.async.dispatch", []byte(`"again"`)))
	assert.Equal(t, call{arg: "again"}, receiveCall(t, service))
}

func TestLocalSubmitWithoutHandlerIsErrNoHandler(t *testing.T) {
	pool := newTestPool(t, DefaultConfig())
	executor := localExecutor{pool: pool, bindings: pool.bindings}

	err := executor.Submit(context.Background(), "test.async.missing", []byte("null"))
	assert.ErrorIs(t, err, tasks.ErrNoHandler)
	assert.Contains(t, err.Error(), `"test.async.missing"`)

	// Refusing before looking at capacity is the point: the miss took
	// neither a running slot nor queue space.
	assert.Equal(t, Stats{}, pool.Stats())
}

func TestLocalSubmitWhileShuttingDownIsErrClosed(t *testing.T) {
	service := &taskService{ran: make(chan call, 1)}
	pool := newProviderPool(t, DefaultConfig(), nil, "local-closed", service)
	executor := localExecutor{pool: pool, bindings: pool.bindings}
	require.NoError(t, pool.drain(context.Background()))

	err := executor.Submit(context.Background(), "test.async.dispatch", []byte(`"hello"`))
	assert.ErrorIs(t, err, tasks.ErrClosed)
	assert.ErrorIs(t, err, ErrShuttingDown)

	err = executor.Go(context.Background(), "test.async.go", func(context.Context) {})
	assert.ErrorIs(t, err, tasks.ErrClosed)
	assert.ErrorIs(t, err, ErrShuttingDown)
}

func TestLocalSubmitOnASaturatedPoolIsErrSaturated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 1
	cfg.QueueCapacity = 0
	cfg.SubmitTimeout = 0
	pool := newProviderPool(t, cfg, nil, "local-saturated", &taskService{ran: make(chan call, 1)})

	hold := newBlockingTask()
	require.NoError(t, pool.Spawn(context.Background(), "hold", hold.run))
	select {
	case <-hold.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the holding task did not start")
	}

	executor := localExecutor{pool: pool, bindings: pool.bindings}
	err := executor.Submit(context.Background(), "test.async.dispatch", []byte(`"hello"`))
	assert.ErrorIs(t, err, tasks.ErrSaturated)
	assert.ErrorIs(t, err, ErrSaturated)

	close(hold.release)
	select {
	case <-hold.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the holding task did not finish")
	}
}

func TestLocalSubmitKeepsContextValuesPastCancellation(t *testing.T) {
	service := &taskService{ran: make(chan call, 1)}
	pool := newProviderPool(t, DefaultConfig(), nil, "local-context", service)
	executor := localExecutor{pool: pool, bindings: pool.bindings}

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextMarkerKey{}, "trace-1"))
	require.NoError(t, executor.Submit(ctx, "test.async.dispatch", []byte(`"hello"`)))
	cancel()

	observed := receiveCall(t, service)
	assert.Equal(t, "trace-1", observed.marker, "the submission context's values must reach the handler")
	assert.Equal(t, "hello", observed.arg)
}

func TestLocalSubmitRecoversAPanickingHandler(t *testing.T) {
	service := &taskService{ran: make(chan call, 1), crash: true}
	logger := &captureLogger{}
	pool := newProviderPool(t, DefaultConfig(), logger, "local-crash", service)
	executor := localExecutor{pool: pool, bindings: pool.bindings}

	require.NoError(t, executor.Submit(context.Background(), "test.async.dispatch", []byte(`"hello"`)))
	waitForCondition(t, func() bool { return logger.recorded("async: task panicked") },
		"the pool to log the handler panic")
	assert.Equal(t, Stats{}, pool.Stats(), "a panicking handler must still release its slot")
}

func TestTheFirstPoolOwnsTheLocalExecutorSlot(t *testing.T) {
	resetGlobal(t)
	firstService := &taskService{ran: make(chan call, 2)}
	secondService := &taskService{ran: make(chan call, 2)}

	first := newProviderPool(t, DefaultConfig(), nil, "slot-first", firstService)
	require.NoError(t, initPool(first, runtimeContext(newFakeHost(), "")))
	t.Cleanup(func() { _ = first.stop(context.Background()) })

	secondLogger := &captureLogger{}
	secondHost := &fakeHost{ctx: context.Background(), log: secondLogger}
	second := newProviderPool(t, DefaultConfig(), nil, "slot-second", secondService)
	require.NoError(t, initPool(second, runtimeContext(secondHost, "")), "the second pool must still initialize")
	t.Cleanup(func() { _ = second.stop(context.Background()) })
	assert.True(t, secondLogger.recorded("async: a local task executor is already installed; keeping the existing binding"),
		"the pool that lost the slot must warn")

	// Submit and Run both route to the first pool's table.
	require.NoError(t, testDispatch.Submit(context.Background(), "submitted"))
	assert.Equal(t, call{arg: "submitted"}, receiveCall(t, firstService))
	require.NoError(t, testDispatch.Run(context.Background(), "ran"))
	assert.Equal(t, call{arg: "ran"}, receiveCall(t, firstService))

	goRan := make(chan struct{})
	require.NoError(t, tasks.Go(context.Background(), func(context.Context) { close(goRan) }))
	select {
	case <-goRan:
	case <-time.After(2 * time.Second):
		t.Fatal("tasks.Go did not reach the installed pool")
	}

	// Stopping the pool that never owned the slot must not uninstall it.
	require.NoError(t, second.stop(context.Background()))
	require.NoError(t, testDispatch.Submit(context.Background(), "still-routed"))
	assert.Equal(t, call{arg: "still-routed"}, receiveCall(t, firstService))

	select {
	case <-secondService.ran:
		t.Fatal("the second pool's handler ran although it never owned the slot")
	default:
	}
}

func TestDrainingReportsErrClosedAndStoppingUninstalls(t *testing.T) {
	resetGlobal(t)
	service := &taskService{ran: make(chan call, 1)}
	pool := newProviderPool(t, DefaultConfig(), nil, "slot-lifecycle", service)
	require.NoError(t, initPool(pool, runtimeContext(newFakeHost(), "")))

	require.NoError(t, testDispatch.Submit(context.Background(), "hello"))
	assert.Equal(t, call{arg: "hello"}, receiveCall(t, service))

	// Draining keeps the executor installed: work is refused as closed, not
	// as uninstalled, so a caller can tell a winding-down process apart from
	// one that never selected the capability.
	require.NoError(t, pool.drain(context.Background()))
	assert.ErrorIs(t, tasks.Go(context.Background(), func(context.Context) {}), tasks.ErrClosed)

	require.NoError(t, pool.stop(context.Background()))
	assert.ErrorIs(t, tasks.Go(context.Background(), func(context.Context) {}), tasks.ErrNotInstalled)
	assert.ErrorIs(t, testDispatch.Submit(context.Background(), "hello"), tasks.ErrNotInstalled)
}
