package outbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/config"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type testRuntimeHost struct {
	ctx         context.Context
	cancel      context.CancelFunc
	trafficGate chan struct{}
	gateOnce    sync.Once
	accepting   bool

	mu sync.Mutex
	wg sync.WaitGroup
}

func newTestRuntimeHost() *testRuntimeHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &testRuntimeHost{
		ctx: ctx, cancel: cancel, trafficGate: make(chan struct{}), accepting: true,
	}
}

func (host *testRuntimeHost) ExecutionContext() context.Context { return host.ctx }
func (*testRuntimeHost) Logger() corelog.Logger                 { return corelog.Nop() }
func (host *testRuntimeHost) TrafficGate() <-chan struct{}      { return host.trafficGate }
func (*testRuntimeHost) RequestShutdown(plugin.Identity, string) bool {
	return false
}

func (host *testRuntimeHost) SubmitTask(_ plugin.Identity, task func(context.Context), _ bool) bool {
	host.mu.Lock()
	if !host.accepting || task == nil {
		host.mu.Unlock()
		return false
	}
	host.wg.Add(1)
	host.mu.Unlock()
	go func() {
		defer host.wg.Done()
		task(host.ctx)
	}()
	return true
}

func (host *testRuntimeHost) releaseTraffic() {
	host.gateOnce.Do(func() { close(host.trafficGate) })
}

func (host *testRuntimeHost) close() {
	host.cancel()
	host.wg.Wait()
}

func lifecycleContext(host plugin.RuntimeHost, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: instance})
}

func TestDefinitionIsCanonicalAndDefaultsRemainStable(t *testing.T) {
	first := Definition()
	assert.True(t, first == Definition(), "Definition must return one canonical handle")
	_ = Bundle()

	want := Config{
		DBInstance:      "default",
		Table:           "xbc_outbox_events",
		Migrate:         false,
		MaxPayloadBytes: 1 << 20,
		Worker: WorkerConfig{
			Enabled:         false,
			PollInterval:    time.Second,
			BatchSize:       100,
			Concurrency:     4,
			LeaseDuration:   30 * time.Second,
			RenewInterval:   10 * time.Second,
			PublishTimeout:  10 * time.Second,
			DatabaseTimeout: 5 * time.Second,
			MaxAttempts:     10,
			InitialBackoff:  time.Second,
			MaxBackoff:      5 * time.Minute,
			Jitter:          0.2,
		},
	}
	assert.Equal(t, want, defaultConfig())
	prepared, err := prepareConfig(Config{})
	require.NoError(t, err)
	assert.Equal(t, want, prepared)
}

func TestPlannedDefinitionSelectsNamedDatabaseAndExportsDispatcher(t *testing.T) {
	db := openPluginTestDB(t)
	gormDefinition := plugin.Define(
		gormPluginKey,
		func(plugin.BuildContext) (*gorm.DB, error) { return db, nil },
		plugin.Options[*gorm.DB]{Instances: plugin.MultipleInstances},
	)
	environment := outboxEnvironment(t, map[string]any{
		"db_instance":       "writer",
		"table":             "plugin_outbox",
		"migrate":           true,
		"max_payload_bytes": 2048,
	})
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(gormDefinition), Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{
		{Plugin: gormPluginKey, Instance: "writer"},
		{Plugin: Key, Instance: plugin.DefaultInstance},
	}, plan.Order())
	assert.Equal(t,
		[]plugin.Identity{{Plugin: Key, Instance: plugin.DefaultInstance}},
		plan.Contracts(reflect.TypeOf((*Dispatcher)(nil)).Elem()),
	)

	host := newTestRuntimeHost()
	t.Cleanup(host.close)
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ corelog.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	require.NoError(t, err)
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	require.True(t, found)
	service, ok := instance.Primary().(*Service)
	require.True(t, ok)
	sqlStore, ok := service.store.(*SQLStore)
	require.True(t, ok)
	assert.Same(t, db, sqlStore.db)
	assert.Equal(t, "plugin_outbox", sqlStore.table)
	assert.Equal(t, 2048, service.maxPayloadBytes)

	assert.False(t, db.Migrator().HasTable("plugin_outbox"))
	require.NoError(t, instance.InvokeMigration())
	assert.True(t, db.Migrator().HasTable("plugin_outbox"))
	require.NoError(t, instance.InvokeStart())
	require.NoError(t, instance.InvokeTrafficPreparation())

	event := enqueueCommitted(t, db, service, Event{Topic: "orders.created"})
	assert.NotEmpty(t, event.ID)
	require.NoError(t, service.stop(context.Background()))
	require.NoError(t, service.stop(context.Background()), "Stop must be idempotent")
}

func TestPlannedPublisherInputIsOptionalAndUsedWhenWorkerEnabled(t *testing.T) {
	db := openPluginTestDB(t)
	gormDefinition := plugin.Define(
		gormPluginKey,
		func(plugin.BuildContext) (*gorm.DB, error) { return db, nil },
	)
	environment := outboxEnvironment(t, map[string]any{
		"worker": map[string]any{"enabled": true},
	})

	withoutPublisher, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(gormDefinition), Bundle()},
		Env:     environment,
	})
	require.NoError(t, err, "Optional Publisher must not make graph planning fail")
	_, err = assembly.Construct(withoutPublisher, assembly.ConstructOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker is enabled but no Publisher is available")

	publisher := &recordingPublisher{events: make(chan Event, 1)}
	publisherDefinition := plugin.Define(
		"outbox-test-publisher",
		func(plugin.BuildContext) (*recordingPublisher, error) { return publisher, nil },
		plugin.Options[*recordingPublisher]{
			Exports: plugin.Contracts(
				plugin.ExportAs(func(value *recordingPublisher) Publisher { return value }),
			),
		},
	)
	withPublisher, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(gormDefinition, publisherDefinition), Bundle()},
		Env:     environment,
	})
	require.NoError(t, err)
	constructed, err := assembly.Construct(withPublisher, assembly.ConstructOptions{})
	require.NoError(t, err)
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	require.True(t, found)
	service := instance.Primary().(*Service)
	assert.Same(t, publisher, service.publisher)
	require.NoError(t, service.stop(context.Background()))
}

func TestWorkerStartsOnlyAtTrafficPreparation(t *testing.T) {
	db, store := sqliteStore(t)
	published := make(chan Event, 1)
	publisher := PublisherFunc(func(_ context.Context, event Event) error {
		published <- event
		return nil
	})
	config := defaultConfig()
	config.Worker = fastWorkerConfig()
	config.Worker.Enabled = true
	service := newConfiguredService(config, store, publisher)
	host := newTestRuntimeHost()
	t.Cleanup(host.close)
	lifecycle := lifecycleContext(host, plugin.DefaultInstance)

	event := enqueueCommitted(t, db, service, Event{Topic: "traffic.barrier"})
	require.NoError(t, service.start(lifecycle))
	select {
	case got := <-published:
		t.Fatalf("event %q published before OpenTraffic", got.ID)
	case <-time.After(60 * time.Millisecond):
	}

	require.NoError(t, service.openTraffic(lifecycle))
	host.releaseTraffic()
	select {
	case got := <-published:
		assert.Equal(t, event.ID, got.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("event was not published after OpenTraffic")
	}
	waitRowStatus(t, db, store.table, StatusPublished)
	require.NoError(t, service.stop(context.Background()))
}

func TestStopDeadlineDoesNotPoisonSharedCleanup(t *testing.T) {
	db, store := sqliteStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	publisher := PublisherFunc(func(ctx context.Context, _ Event) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	config := defaultConfig()
	config.Worker = fastWorkerConfig()
	config.Worker.Enabled = true
	config.Worker.PublishTimeout = 2 * time.Second
	service := newConfiguredService(config, store, publisher)
	host := newTestRuntimeHost()
	t.Cleanup(host.close)
	lifecycle := lifecycleContext(host, plugin.DefaultInstance)

	enqueueCommitted(t, db, service, Event{Topic: "shared-stop"})
	require.NoError(t, service.start(lifecycle))
	require.NoError(t, service.openTraffic(lifecycle))
	host.releaseTraffic()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("publish did not start")
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := service.stop(short)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	tx := db.Begin()
	require.NoError(t, tx.Error)
	_, err = service.Enqueue(context.Background(), tx, Event{Topic: "late"})
	require.ErrorIs(t, err, ErrClosed)
	require.NoError(t, tx.Rollback().Error)

	later := make(chan error, 1)
	go func() { later <- service.stop(context.Background()) }()
	select {
	case err := <-later:
		t.Fatalf("later Stop returned before shared cleanup completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-later:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("later Stop did not receive shared cleanup result")
	}
	require.NoError(t, service.stop(context.Background()))
	waitRowStatus(t, db, store.table, StatusPublished)
}

func TestStopIsSafeAndIdempotentFromPartiallyStartedState(t *testing.T) {
	_, store := sqliteStore(t)
	config := defaultConfig()
	config.Worker = fastWorkerConfig()
	config.Worker.Enabled = true
	publisher := PublisherFunc(func(context.Context, Event) error { return nil })
	service := newConfiguredService(config, store, publisher)
	dispatch, err := newWorker(store, publisher, config.Worker)
	require.NoError(t, err)
	require.NoError(t, dispatch.schedule())

	service.lifecycleMu.Lock()
	service.starting = true
	service.worker = dispatch
	service.lifecycleMu.Unlock()

	require.NoError(t, service.stop(nil))
	require.NoError(t, service.stop(context.Background()))
	service.lifecycleMu.Lock()
	assert.False(t, service.starting)
	assert.True(t, service.stopped)
	assert.Nil(t, service.worker)
	service.lifecycleMu.Unlock()
}

func TestStartRejectsTaskAdmissionAndRemainsStoppable(t *testing.T) {
	_, store := sqliteStore(t)
	config := defaultConfig()
	config.Worker = fastWorkerConfig()
	config.Worker.Enabled = true
	service := newConfiguredService(config, store, PublisherFunc(func(context.Context, Event) error { return nil }))
	host := newTestRuntimeHost()
	host.accepting = false
	t.Cleanup(host.close)

	err := service.start(lifecycleContext(host, plugin.DefaultInstance))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not accepting")
	require.NoError(t, service.stop(context.Background()))
	require.NoError(t, service.stop(context.Background()))
}

type recordingPublisher struct {
	events chan Event
}

func (publisher *recordingPublisher) Publish(_ context.Context, event Event) error {
	publisher.events <- event
	return nil
}

func openPluginTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}

func outboxEnvironment(t *testing.T, outbox map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"gorm": map[string]any{
				"writer": map[string]any{},
			},
			"outbox": outbox,
		},
	}, "")
	require.NoError(t, err)
	return environment
}

func TestPrepareConfigRejectsInvalidValues(t *testing.T) {
	config := defaultConfig()
	config.DBInstance = "bad instance"
	_, err := prepareConfig(config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid db_instance")

	config = defaultConfig()
	config.Worker.RenewInterval = config.Worker.LeaseDuration
	_, err = prepareConfig(config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renew_interval")
}

func TestStopReturnsCanceledCallerWithoutSkippingCleanup(t *testing.T) {
	_, store := sqliteStore(t)
	service := newService(store, 1024)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := service.stop(canceled)
	require.True(t, errors.Is(err, context.Canceled))
	require.NoError(t, service.stop(context.Background()))
}
