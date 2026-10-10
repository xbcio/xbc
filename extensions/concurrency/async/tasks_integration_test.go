package async_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/extensions/concurrency/async"
	"github.com/xbcio/xbc/extensions/tasks"
	"github.com/xbcio/xbc/plugin"
)

// e2eConfirm is the method task the composition test submits and runs. It is
// package-level because tasks.Method claims its name process-wide and panics
// on a duplicate definition.
var e2eConfirm = tasks.Method("test.async.e2e.confirm", (*providerService).confirm)

// providerService is a plugin that provides one method task. Start submits
// through the installed local executor, Stop observes whether the executor is
// still installed, and both report on channels so the test can assert the
// lifecycle ordering rather than a timing.
type providerService struct {
	executed  chan string
	stopGoErr chan error
}

func (s *providerService) confirm(ctx context.Context, arg string) error {
	s.executed <- arg
	return nil
}

// Tasks declares the method task on this plugin, making it one of async's
// collected providers.
func (s *providerService) Tasks() []tasks.Binding { return tasks.Bind(s, e2eConfirm) }

func (s *providerService) start(ctx *plugin.Context) error {
	// A critical managed task keeps the application resident: this test
	// composes no transport, and a composition whose plugins all come and go
	// is refused at startup unless something outlives the startup itself.
	if !ctx.GoCritical(func(taskCtx context.Context) {
		<-taskCtx.Done()
	}) {
		return errors.New("tasks-e2e-provider: the resident task was refused")
	}
	// Every Init has already run by the time any Start does, so the executor
	// is installed: this is the documented moment for a plugin to submit
	// start-up work.
	return e2eConfirm.Submit(context.Background(), "from-start")
}

func (s *providerService) stop(context.Context) error {
	// async is this plugin's dependent, so it was drained and stopped --
	// including uninstalling its local executor -- before this runs.
	s.stopGoErr <- tasks.Go(context.Background(), func(context.Context) {})
	return nil
}

func providerDefinition(service *providerService) plugin.Definition {
	return plugin.Define("tasks-e2e-provider", func(plugin.BuildContext) (*providerService, error) {
		return service, nil
	}, plugin.Options[*providerService]{
		Exports: plugin.Contracts(
			plugin.ExportAs[tasks.Provider](func(service *providerService) tasks.Provider { return service }),
		),
		Lifecycle: plugin.Lifecycle[*providerService]{
			Start: (*providerService).start,
			Stop:  (*providerService).stop,
		},
	})
}

// TestTasksFlowThroughARealComposition runs the whole path against a real
// application: a plugin exports tasks.Provider, async collects it into the
// local executor at construction, Start submits a method task through the
// process-wide executor, Run resolves the binding synchronously while the
// application serves, and the provider's Stop runs only after async has
// stopped and uninstalled the executor -- the graph order that keeps a
// handler's dependencies alive for as long as it can run.
func TestTasksFlowThroughARealComposition(t *testing.T) {
	service := &providerService{executed: make(chan string, 4), stopGoErr: make(chan error, 1)}
	bundle := plugin.BundleOf(providerDefinition(service))
	app, err := xbc.New(xbc.WithBundles(async.Bundle(), bundle))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, execErr := app.Execute(ctx, []string{"--config", writeAppConfig(t)})
		done <- struct {
			code int
			err  error
		}{code, execErr}
	}()
	t.Cleanup(cancel)

	select {
	case got := <-service.executed:
		assert.Equal(t, "from-start", got, "the submission from the provider's Start must run through the local executor")
	case <-time.After(5 * time.Second):
		t.Fatal("the task submitted during Start did not execute")
	}

	// Run resolves the provider's binding through the installed executor and
	// executes the method right in this goroutine.
	require.NoError(t, e2eConfirm.Run(context.Background(), "direct"))
	select {
	case got := <-service.executed:
		assert.Equal(t, "direct", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the direct Run did not execute")
	}

	cancel()
	select {
	case result := <-done:
		assert.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(5 * time.Second):
		t.Fatal("application did not exit")
	}

	select {
	case goErr := <-service.stopGoErr:
		assert.ErrorIs(t, goErr, tasks.ErrNotInstalled,
			"async must have stopped and uninstalled its local executor before its provider plugin stops")
	case <-time.After(2 * time.Second):
		t.Fatal("the provider's Stop did not run")
	}
}

// TestSubmissionWithoutAnExecutorIsNotInstalled pins the other end: an
// application that selected no executor reports that plainly, in one process
// that also runs a composed one, without disturbing it.
func TestSubmissionWithoutAnExecutorIsNotInstalled(t *testing.T) {
	service := &providerService{executed: make(chan string, 4), stopGoErr: make(chan error, 1)}
	bundle := plugin.BundleOf(providerDefinition(service))
	app, err := xbc.New(xbc.WithBundles(async.Bundle(), bundle))
	require.NoError(t, err)

	// Before any application runs, nothing is installed.
	assert.ErrorIs(t, e2eConfirm.Submit(context.Background(), "too-early"), tasks.ErrNotInstalled)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := app.Execute(ctx, []string{"--config", writeAppConfig(t)})
		done <- code
	}()
	t.Cleanup(cancel)

	select {
	case got := <-service.executed:
		assert.Equal(t, "from-start", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the composed application did not submit its start-up task")
	}

	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code)
	case <-time.After(5 * time.Second):
		t.Fatal("application did not exit")
	}

	// After the application's executor was uninstalled, the same call the
	// composed application accepted reports uninstalled again.
	err = e2eConfirm.Submit(context.Background(), "too-late")
	assert.True(t, errors.Is(err, tasks.ErrNotInstalled), "Submit after exit returned %v, want ErrNotInstalled", err)
}

// cyclicProvider both depends on async explicitly and exports tasks.Provider.
// async collects every Provider, so the provider sits upstream of async while
// its own RefTo puts it downstream: the composition is a cycle that must be
// split into two Definitions, and the graph must say so rather than hang.
type cyclicProvider struct{}

func (*cyclicProvider) Tasks() []tasks.Binding { return nil }

// TestProviderThatAlsoDependsOnAsyncIsACycle pins the rule the tasks package
// documents: a plugin may provide tasks or depend on the executor explicitly,
// not both. The failure must name the cycle at composition time.
func TestProviderThatAlsoDependsOnAsyncIsACycle(t *testing.T) {
	spawner := plugin.RefTo[async.Spawner](async.Key)
	cyclic := plugin.Define("tasks-cyclic-provider", func(ctx plugin.BuildContext) (*cyclicProvider, error) {
		_ = spawner.Get(ctx)
		return &cyclicProvider{}, nil
	}, plugin.Options[*cyclicProvider]{
		Inputs: plugin.Inputs(spawner),
		Exports: plugin.Contracts(
			plugin.ExportAs[tasks.Provider](func(value *cyclicProvider) tasks.Provider { return value }),
		),
	})

	app, err := xbc.New(xbc.WithBundles(async.Bundle(), plugin.BundleOf(cyclic)))
	if err == nil {
		code, execErr := app.Execute(context.Background(), []string{"--config", writeAppConfig(t)})
		assert.NotZero(t, code)
		err = execErr
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugin dependency cycle")
	assert.Contains(t, err.Error(), "tasks-cyclic-provider")
}

// residentDefinition is a plugin that only keeps its application resident, so
// a test can run an application whose executor slot is taken by another one.
func residentDefinition(started chan<- struct{}) plugin.Definition {
	type resident struct{}
	return plugin.Define("tasks-coexist-resident", func(plugin.BuildContext) (*resident, error) {
		return &resident{}, nil
	}, plugin.Options[*resident]{
		Lifecycle: plugin.Lifecycle[*resident]{
			Start: func(_ *resident, ctx *plugin.Context) error {
				if !ctx.GoCritical(func(taskCtx context.Context) { <-taskCtx.Done() }) {
					return errors.New("tasks-coexist-resident: the resident task was refused")
				}
				started <- struct{}{}
				return nil
			},
		},
	})
}

// TestTwoApplicationsCoexistInOneProcess pins the first-come-first-served
// slot: a second application in the same process finds the local executor
// already installed, keeps running without it rather than failing to start,
// and leaves the first one's binding untouched.
func TestTwoApplicationsCoexistInOneProcess(t *testing.T) {
	started := make(chan struct{}, 2)
	first, err := xbc.New(xbc.WithBundles(async.Bundle(), plugin.BundleOf(residentDefinition(started))))
	require.NoError(t, err)
	second, err := xbc.New(xbc.WithBundles(async.Bundle(), plugin.BundleOf(residentDefinition(started))))
	require.NoError(t, err)

	run := func(app *xbc.App) (context.CancelFunc, <-chan int) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		done := make(chan int, 1)
		go func() {
			code, _ := app.Execute(ctx, []string{"--config", writeAppConfig(t)})
			done <- code
		}()
		return cancel, done
	}
	awaitStart := func() {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("an application did not start")
		}
	}
	awaitExit := func(done <-chan int) {
		select {
		case code := <-done:
			assert.Equal(t, 0, code)
		case <-time.After(5 * time.Second):
			t.Fatal("application did not exit")
		}
	}

	cancelFirst, firstDone := run(first)
	awaitStart()
	cancelSecond, secondDone := run(second)
	awaitStart()

	// The second application stopping must not uninstall the first one's
	// executor: its own uninstall is a no-op because it never held the slot.
	cancelSecond()
	awaitExit(secondDone)
	ran := make(chan struct{})
	require.NoError(t, tasks.Go(context.Background(), func(context.Context) { close(ran) }))
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the first application's executor did not run the task")
	}

	cancelFirst()
	awaitExit(firstDone)
	assert.ErrorIs(t, tasks.Go(context.Background(), func(context.Context) {}), tasks.ErrNotInstalled)
}
