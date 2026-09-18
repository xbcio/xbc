package placement_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/extensions/coordination/placement"
	redisstore "github.com/xbcio/xbc/extensions/storage/redis"
	"github.com/xbcio/xbc/plugin"
)

// These tests drive a real application through the root facade, against a real
// Redis-backed store, so a lease-backed hosted set is exercised through the
// same seams a deployment uses rather than only through Resolve.

const (
	e2eWorkloadKey  plugin.WorkloadKey = "alpha"
	e2eSlotKey                         = "xbc:workload:alpha:0"
	e2eStartTimeout                    = 20 * time.Second
)

// probePlugin reports that its instance reached the point where traffic is
// about to be accepted. That is the observable proof that a Definition was
// constructed and started, which is what "this process hosts that workload"
// means downstream.
type probePlugin struct {
	opened chan struct{}
	once   sync.Once
}

func (p *probePlugin) openTraffic(*plugin.Context) error {
	p.once.Do(func() { close(p.opened) })
	return nil
}

func (p *probePlugin) stop(context.Context) error { return nil }

func probeDefinition(key plugin.Key, opened chan struct{}, constructed *atomic.Int32) plugin.Definition {
	return plugin.Define(key, func(plugin.BuildContext) (*probePlugin, error) {
		constructed.Add(1)
		return &probePlugin{opened: opened}, nil
	}, plugin.Options[*probePlugin]{Lifecycle: plugin.Lifecycle[*probePlugin]{
		OpenTraffic: (*probePlugin).openTraffic,
		Stop:        (*probePlugin).stop,
	}})
}

// unownedBundle declares a Definition that belongs to no workload, which is the
// part of the graph present in every process shape.
func unownedBundle(key plugin.Key, opened chan struct{}, constructed *atomic.Int32) plugin.Bundle {
	return plugin.BundleOf(probeDefinition(key, opened, constructed))
}

// workloadBundle declares the same shape as the member of a workload, so a
// process that does not host that workload contributes no instance of it at all.
func workloadBundle(key plugin.WorkloadKey, opened chan struct{}, constructed *atomic.Int32) plugin.Bundle {
	return plugin.WorkloadOf(key,
		plugin.BundleOf(probeDefinition(plugin.Key(key+"-member"), opened, constructed)),
		plugin.WithReplicas(1),
	)
}

// appConfig writes the framework section this file's assertions depend on: both
// stop budgets are spelled out, so the pre-stop phase -- where the slots are
// handed back -- actually runs.
func appConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: 5s\n  pre_stop_timeout: 1s\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

type appRun struct {
	done chan struct{}
	code int
	err  error
}

func startApp(t *testing.T, config string, source xbc.PlacementSource, bundles ...plugin.Bundle) (context.CancelFunc, *appRun) {
	t.Helper()
	app, err := xbc.New(xbc.WithPlacement(source), xbc.WithBundles(bundles...))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	run := &appRun{done: make(chan struct{})}
	go func() {
		defer close(run.done)
		run.code, run.err = app.Execute(ctx, []string{"--config", config})
	}()
	t.Cleanup(cancel)
	return cancel, run
}

func (r *appRun) await(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(e2eStartTimeout):
		t.Fatal("the application did not stop")
	}
}

func awaitClose(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(e2eStartTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func newMiniredis(t *testing.T) (*miniredis.Miniredis, *goredis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return server, client
}

// miniredisGet reads a slot key's value straight out of the backend, which is
// how a test asks what an operator reading the store would see rather than what
// this process reports about itself.
func miniredisGet(t *testing.T, server *miniredis.Miniredis, key string) string {
	t.Helper()
	value, err := server.Get(key)
	require.NoError(t, err)
	return value
}

func newLocker(t *testing.T, client *goredis.Client) lease.Locker {
	t.Helper()
	locker, err := redisstore.NewLocker(client)
	require.NoError(t, err)
	return locker
}

// TestALeaseHolderHostsTheDeclaredWorkloadAndGivesItBackOnStop runs the whole
// path: a slot is won before the graph is built, the workload's Definition is
// planned and started because of it, and the slot is handed back before the
// process stops.
func TestALeaseHolderHostsTheDeclaredWorkloadAndGivesItBackOnStop(t *testing.T) {
	server, client := newMiniredis(t)
	hosting, err := placement.New(newLocker(t, client), placement.WithRenewInterval(50*time.Millisecond))
	require.NoError(t, err)

	var workloadInstances, unownedInstances atomic.Int32
	workloadOpened := make(chan struct{})
	unownedOpened := make(chan struct{})

	cancel, run := startApp(t, appConfig(t), hosting,
		hosting.Bundle(),
		unownedBundle("e2e-unowned", unownedOpened, &unownedInstances),
		workloadBundle(e2eWorkloadKey, workloadOpened, &workloadInstances),
	)

	awaitClose(t, unownedOpened, "the unowned plugin to open traffic")
	awaitClose(t, workloadOpened, "the hosted workload to be constructed and started")

	assert.Equal(t, int32(1), workloadInstances.Load())
	assert.Equal(t, int32(1), unownedInstances.Load())
	assert.True(t, server.Exists(e2eSlotKey), "the hosted workload is backed by a held slot")

	stats := hosting.Stats()
	require.Len(t, stats.Held, 1)
	assert.Equal(t, e2eWorkloadKey, stats.Held[0].Workload)
	assert.False(t, stats.Standby)
	// The identity travels runtime → request → source through the real boot, and
	// this is the only test that runs that whole path: nothing here configures
	// xbc.instance_id, so a non-empty value means the runtime derived one and
	// handed it over. The token stays the store's own.
	assert.NotEmpty(t, stats.Instance, "a real boot hands the process identity to its placement source")
	assert.NotEqual(t, stats.Instance, stats.Held[0].Owner,
		"the store's owner token is not the process identity")
	assert.Equal(t, stats.Held[0].Owner, miniredisGet(t, server, e2eSlotKey),
		"the slot key still holds the backend's own token")

	cancel()
	run.await(t)
	require.NoError(t, run.err)
	assert.Equal(t, 0, run.code)
	assert.False(t, server.Exists(e2eSlotKey), "the slot is handed back before the process exits")
}

// TestAStaticDecisionDrivesTheSameComposition is the comparison the delivery
// boundary asks for: the same Bundles produce the same downstream behaviour,
// and the only difference is where the hosted set came from.
func TestAStaticDecisionDrivesTheSameComposition(t *testing.T) {
	var workloadInstances, unownedInstances atomic.Int32
	workloadOpened := make(chan struct{})
	unownedOpened := make(chan struct{})

	cancel, run := startApp(t, appConfig(t), xbc.StaticPlacement(),
		unownedBundle("e2e-unowned", unownedOpened, &unownedInstances),
		workloadBundle(e2eWorkloadKey, workloadOpened, &workloadInstances),
	)

	awaitClose(t, unownedOpened, "the unowned plugin to open traffic")
	awaitClose(t, workloadOpened, "the declared workload to be constructed and started")
	assert.Equal(t, int32(1), workloadInstances.Load())

	cancel()
	run.await(t)
	require.NoError(t, run.err)
	assert.Equal(t, 0, run.code)
}

// TestAStandbyStartsReadyWithOnlyTheUnownedPlugins pins the takeover shape. The
// process won nothing, so the workload it could not win contributes no
// Definition and no resource; the process is nevertheless a complete, ready
// application, which is what makes it available to take the role over later.
func TestAStandbyStartsReadyWithOnlyTheUnownedPlugins(t *testing.T) {
	server, client := newMiniredis(t)

	// Another process already holds the only slot.
	holder, err := placement.New(newLocker(t, client))
	require.NoError(t, err)
	held, err := holder.Resolve(plugin.PlacementRequest{
		Workloads: []plugin.Workload{{Key: e2eWorkloadKey, Replicas: 1}},
	})
	require.NoError(t, err)
	require.Equal(t, []plugin.WorkloadKey{e2eWorkloadKey}, held.Hosted)

	standby, err := placement.New(newLocker(t, client), placement.WithStandbyRetry(2*time.Second))
	require.NoError(t, err)

	var workloadInstances, unownedInstances atomic.Int32
	workloadOpened := make(chan struct{})
	unownedOpened := make(chan struct{})

	cancel, run := startApp(t, appConfig(t), standby,
		standby.Bundle(),
		unownedBundle("e2e-unowned", unownedOpened, &unownedInstances),
		workloadBundle(e2eWorkloadKey, workloadOpened, &workloadInstances),
	)

	awaitClose(t, unownedOpened, "the unowned plugin to open traffic")
	assert.Equal(t, int32(1), unownedInstances.Load())
	assert.Zero(t, workloadInstances.Load(), "an unhosted workload contributes no Definition at all")

	select {
	case <-workloadOpened:
		t.Fatal("the standby opened traffic for a workload it does not host")
	default:
	}
	assert.True(t, standby.Stats().Standby)
	assert.True(t, server.Exists(e2eSlotKey), "the standby left the holder's slot alone")

	cancel()
	run.await(t)
	require.NoError(t, run.err)
	assert.Equal(t, 0, run.code)
}

// TestAnUnreachableLeaseStoreFailsStartupWithActionableText pins the cold-start
// rule at the level an operator meets it. Falling back to hosting every
// workload would make the process shape depend on store availability -- doctor
// output, startup validation, exclusivity and capacity would all be
// non-deterministic and fail open -- so the only acceptable outcome is a
// failure that says why.
func TestAnUnreachableLeaseStoreFailsStartupWithActionableText(t *testing.T) {
	server, client := newMiniredis(t)
	hosting, err := placement.New(newLocker(t, client))
	require.NoError(t, err)
	server.Close()

	var workloadInstances atomic.Int32
	workloadOpened := make(chan struct{})
	config := appConfig(t)

	app, err := xbc.New(
		xbc.WithPlacement(hosting),
		xbc.WithBundles(hosting.Bundle(), workloadBundle(e2eWorkloadKey, workloadOpened, &workloadInstances)),
	)
	require.NoError(t, err)

	// The context is bounded so that a fail-open regression fails this test
	// instead of hanging it: if the process wrongly decides to host everything,
	// it starts and runs until something stops it, and a background context
	// would leave the assertion below unreached until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), e2eStartTimeout)
	defer cancel()
	code, err := app.Execute(ctx, []string{"--config", config})

	require.Error(t, err)
	assert.Equal(t, 1, code)
	assert.Contains(t, err.Error(), "cannot reach the lease store")
	assert.Contains(t, err.Error(), `workload "alpha"`)
	assert.Contains(t, err.Error(), "must not guess and host everything instead")
	assert.Zero(t, workloadInstances.Load(), "nothing is constructed when the role cannot be decided")
}
