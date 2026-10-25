package outbox

import (
	"context"
	"errors"
	"maps"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xbcio/xbc/plugin"
	"gorm.io/gorm"
)

// maintenanceStacks returns the stacks of every goroutine running the
// retention maintenance loop or one of its passes. The module carries no
// goroutine-leak dependency, so tests that must prove "no leaked goroutine"
// look for exactly the frames that would be left behind.
func maintenanceStacks() string {
	buffer := make([]byte, 1<<20)
	buffer = buffer[:runtime.Stack(buffer, true)]
	var found []string
	for stack := range strings.SplitSeq(string(buffer), "\n\n") {
		if strings.Contains(stack, "(*worker).maintain") || strings.Contains(stack, "(*worker).purgePass") {
			found = append(found, stack)
		}
	}
	return strings.Join(found, "\n\n")
}

func requireNoMaintenanceGoroutine(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stacks := maintenanceStacks()
		if stacks == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance goroutine outlived Stop:\n%s", stacks)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitRowStatuses(t *testing.T, db *gorm.DB, table string, want map[string]Status) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got map[string]Status
	for time.Now().Before(deadline) {
		var rows []outboxRow
		if err := db.Table(table).Find(&rows).Error; err == nil {
			got = make(map[string]Status, len(rows))
			for _, row := range rows {
				got[row.ID] = row.Status
			}
			if maps.Equal(got, want) {
				return
			}
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("rows = %v, want %v", got, want)
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func newMaintenanceWorker(t *testing.T, store Store, retention RetentionConfig) *worker {
	t.Helper()
	w, err := newWorker(store, PublisherFunc(func(context.Context, Event) error { return nil }), fastWorkerConfig())
	require.NoError(t, err)
	w.retention = retention
	return w
}

// purgeSpyStore counts Purge calls and can fail or observe them.
type purgeSpyStore struct {
	Store
	calls atomic.Int32
	fail  func(call int32, status Status) error
	after func()
}

func (s *purgeSpyStore) Purge(ctx context.Context, now time.Time, status Status, olderThan time.Duration, limit int) (int, error) {
	call := s.calls.Add(1)
	if s.fail != nil {
		if err := s.fail(call, status); err != nil {
			return 0, err
		}
	}
	deleted, err := s.Store.Purge(ctx, now, status, olderThan, limit)
	if s.after != nil {
		s.after()
	}
	return deleted, err
}

func seedExpired(t *testing.T, db *gorm.DB, table string, status Status, ids ...string) {
	t.Helper()
	ancient := time.Now().UTC().Add(-1000 * time.Hour)
	for _, id := range ids {
		seedRow(t, db, table, outboxRow{ID: id, Status: status, UpdatedAt: ancient})
	}
}

func TestMaintenancePurgesRepeatedlyOnItsOwnTick(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	seedExpired(t, db, store.table, StatusPublished, "expired-1")
	seedExpired(t, db, store.table, StatusDead, "dead-kept")
	seedRow(t, db, store.table, outboxRow{ID: "recent", Status: StatusPublished, UpdatedAt: now})

	w := newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: 5 * time.Millisecond, BatchSize: 10})
	startWorker(t, w)
	waitRowStatuses(t, db, store.table, map[string]Status{"recent": StatusPublished, "dead-kept": StatusDead})

	// A row that expires after the first pass is removed by a later tick, so
	// the loop really repeats rather than running once at start.
	seedExpired(t, db, store.table, StatusPublished, "expired-2")
	waitRowStatuses(t, db, store.table, map[string]Status{"recent": StatusPublished, "dead-kept": StatusDead})
}

func TestMaintenanceRunsOnlyWhenARetentionIsConfigured(t *testing.T) {
	_, store := sqliteStore(t)

	idle := &purgeSpyStore{Store: store}
	startWorker(t, newMaintenanceWorker(t, idle, RetentionConfig{Interval: time.Millisecond, BatchSize: 10}))
	time.Sleep(60 * time.Millisecond)
	assert.Zero(t, idle.calls.Load(), "zero retention must not purge")
	assert.Empty(t, maintenanceStacks(), "zero retention must not start the goroutine")

	// The control: the same detector sees the goroutine when retention is on,
	// so the empty result above means something.
	busy := &purgeSpyStore{Store: store}
	startWorker(t, newMaintenanceWorker(t, busy, RetentionConfig{Published: time.Hour, Interval: time.Millisecond, BatchSize: 10}))
	require.Eventually(t, func() bool { return busy.calls.Load() > 0 }, 2*time.Second, time.Millisecond)
	assert.NotEmpty(t, maintenanceStacks())
}

func TestMaintenanceStartsOnlyAfterTrafficOpens(t *testing.T) {
	db, store := sqliteStore(t)
	config := defaultConfig()
	config.Worker = fastWorkerConfig()
	config.Worker.Enabled = true
	config.Retention = RetentionConfig{Published: time.Hour, Interval: 5 * time.Millisecond, BatchSize: 10}
	service := newConfiguredService(config, store, PublisherFunc(func(context.Context, Event) error { return nil }))
	host := newTestRuntimeHost()
	t.Cleanup(host.close)
	lifecycle := lifecycleContext(host, plugin.DefaultInstance)
	seedExpired(t, db, store.table, StatusPublished, "expired")

	require.NoError(t, service.start(lifecycle))
	require.NoError(t, service.openTraffic(lifecycle))
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, map[string]Status{"expired": StatusPublished}, rowStatuses(t, db, store.table),
		"nothing may be purged before the global traffic gate opens")

	host.releaseTraffic()
	waitRowStatuses(t, db, store.table, map[string]Status{})
	require.NoError(t, service.stop(context.Background()))
	requireNoMaintenanceGoroutine(t)
}

func TestPurgePassIsBoundedAndLeavesTheRemainderForTheNextPass(t *testing.T) {
	t.Run("a full pass stops after the batch cap", func(t *testing.T) {
		db, sqlStore := sqliteStore(t)
		store := &purgeSpyStore{Store: sqlStore}
		ids := make([]string, maxPurgeBatches+5)
		for i := range ids {
			ids[i] = "row-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		}
		seedExpired(t, db, sqlStore.table, StatusPublished, ids...)
		w := newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: time.Hour, BatchSize: 1})

		w.purgePass(context.Background())
		assert.Equal(t, int32(maxPurgeBatches), store.calls.Load())
		assert.Len(t, rowStatuses(t, db, sqlStore.table), 5, "the remainder waits for the next pass")

		w.purgePass(context.Background())
		assert.Empty(t, rowStatuses(t, db, sqlStore.table))
	})

	t.Run("a short batch ends the pass", func(t *testing.T) {
		db, sqlStore := sqliteStore(t)
		store := &purgeSpyStore{Store: sqlStore}
		seedExpired(t, db, sqlStore.table, StatusPublished, "a", "b", "c", "d", "e", "f", "g")
		w := newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: time.Hour, BatchSize: 3})

		w.purgePass(context.Background())
		assert.Empty(t, rowStatuses(t, db, sqlStore.table))
		assert.Equal(t, int32(3), store.calls.Load(), "3 + 3 + 1 rows take three batches and no more")
	})

	t.Run("a canceled context deletes nothing", func(t *testing.T) {
		db, sqlStore := sqliteStore(t)
		seedExpired(t, db, sqlStore.table, StatusPublished, "a")
		w := newMaintenanceWorker(t, sqlStore, RetentionConfig{Published: time.Hour, Interval: time.Hour, BatchSize: 3})
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		w.purgePass(canceled)
		assert.Len(t, rowStatuses(t, db, sqlStore.table), 1)
	})

	t.Run("cancellation between batches ends the pass", func(t *testing.T) {
		db, sqlStore := sqliteStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := &purgeSpyStore{Store: sqlStore, after: cancel}
		seedExpired(t, db, sqlStore.table, StatusPublished, "a", "b", "c", "d")
		w := newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: time.Hour, BatchSize: 1})

		w.purgePass(ctx)
		assert.Equal(t, int32(1), store.calls.Load())
		assert.Len(t, rowStatuses(t, db, sqlStore.table), 3)
	})
}

func TestPurgeBatchPurgesEachStateEvenWhenTheOtherFails(t *testing.T) {
	db, sqlStore := sqliteStore(t)
	store := &purgeSpyStore{Store: sqlStore, fail: func(_ int32, status Status) error {
		if status == StatusPublished {
			return errors.New("published purge unavailable")
		}
		return nil
	}}
	seedExpired(t, db, sqlStore.table, StatusPublished, "published")
	seedExpired(t, db, sqlStore.table, StatusDead, "dead")

	published, dead, err := purgeBatch(context.Background(), store,
		RetentionConfig{Published: time.Hour, Dead: time.Hour, Interval: time.Hour, BatchSize: 10}, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "purge published events")
	assert.Zero(t, published)
	assert.Equal(t, 1, dead, "a failing state must not starve the other")
	assert.Equal(t, map[string]Status{"published": StatusPublished}, rowStatuses(t, db, sqlStore.table))
}

func TestMaintenanceSurvivesAFailedPass(t *testing.T) {
	db, sqlStore := sqliteStore(t)
	store := &purgeSpyStore{Store: sqlStore, fail: func(call int32, _ Status) error {
		if call <= 2 {
			return errors.New("database unavailable")
		}
		return nil
	}}
	seedExpired(t, db, sqlStore.table, StatusPublished, "expired")

	startWorker(t, newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: 3 * time.Millisecond, BatchSize: 10}))
	waitRowStatuses(t, db, sqlStore.table, map[string]Status{})
	assert.GreaterOrEqual(t, store.calls.Load(), int32(3))
}

func TestStopCancelsAndWaitsForAMidPassMaintenance(t *testing.T) {
	_, sqlStore := sqliteStore(t)
	store := newStallingStore(sqlStore)
	t.Cleanup(store.unblock)
	w := newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: time.Millisecond, BatchSize: 10})
	require.NoError(t, w.schedule())
	go w.run(context.Background())
	waitSignal(t, store.entered, "maintenance did not reach the store")

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- w.stopAndWait(ctx)
	}()
	waitSignal(t, store.canceled, "Stop did not cancel the pass in flight")
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) while the maintenance pass was still running", err)
	case <-time.After(40 * time.Millisecond):
	}

	store.unblock()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the pass finished")
	}
	requireNoMaintenanceGoroutine(t)
}

func TestStopCancelsMaintenanceWithoutWaitingForPublishers(t *testing.T) {
	db, sqlStore := sqliteStore(t)
	store := newStallingStore(sqlStore)
	t.Cleanup(store.unblock)
	service := newService(store, 1024)
	enqueueCommitted(t, db, service, Event{Topic: "slow-publish"})

	publishing := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePublisher := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releasePublisher)
	cfg := fastWorkerConfig()
	cfg.PublishTimeout = 5 * time.Second
	// The per-batch database timeout must far exceed every wait below. If it
	// were short, the pass would end on its own timeout and the test would pass
	// even when Stop never reached the maintenance goroutine.
	cfg.DatabaseTimeout = 30 * time.Second
	w, err := newWorker(store, PublisherFunc(func(context.Context, Event) error {
		close(publishing)
		<-release
		return nil
	}), cfg)
	require.NoError(t, err)
	w.retention = RetentionConfig{Published: time.Hour, Interval: time.Millisecond, BatchSize: 10}
	require.NoError(t, w.schedule())
	go w.run(context.Background())
	waitSignal(t, publishing, "publish did not start")
	waitSignal(t, store.entered, "maintenance did not reach the store")

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- w.stopAndWait(ctx)
	}()
	// Stop drains the accepted publish, which can take a while. Cleanup must
	// not sit behind it.
	waitSignal(t, store.canceled, "maintenance kept running while Stop waited for a publisher")
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) before the accepted publish finished", err)
	default:
	}

	releasePublisher()
	store.unblock()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	waitRowStatus(t, db, sqlStore.table, StatusPublished)
	requireNoMaintenanceGoroutine(t)
}

func TestStopWithMaintenanceConfiguredNeverHangs(t *testing.T) {
	retentionConfig := func() Config {
		config := defaultConfig()
		config.Worker = fastWorkerConfig()
		config.Worker.Enabled = true
		config.Retention = RetentionConfig{Published: time.Hour, Dead: time.Hour, Interval: time.Millisecond, BatchSize: 10}
		return config
	}
	publisher := PublisherFunc(func(context.Context, Event) error { return nil })
	stopWithin := func(t *testing.T, service *Service) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, service.stop(ctx))
		require.NoError(t, service.stop(ctx), "Stop must be idempotent")
		requireNoMaintenanceGoroutine(t)
	}

	t.Run("before Start", func(t *testing.T) {
		_, store := sqliteStore(t)
		stopWithin(t, newConfiguredService(retentionConfig(), store, publisher))
	})

	t.Run("after a failed Start", func(t *testing.T) {
		_, store := sqliteStore(t)
		service := newConfiguredService(retentionConfig(), store, publisher)
		host := newTestRuntimeHost()
		host.accepting = false
		t.Cleanup(host.close)
		require.Error(t, service.start(lifecycleContext(host, plugin.DefaultInstance)))
		stopWithin(t, service)
	})

	t.Run("after Start but before traffic opens", func(t *testing.T) {
		_, store := sqliteStore(t)
		service := newConfiguredService(retentionConfig(), store, publisher)
		host := newTestRuntimeHost()
		t.Cleanup(host.close)
		require.NoError(t, service.start(lifecycleContext(host, plugin.DefaultInstance)))
		stopWithin(t, service)
	})

	t.Run("worker scheduled but never run", func(t *testing.T) {
		_, store := sqliteStore(t)
		w := newMaintenanceWorker(t, store, RetentionConfig{Published: time.Hour, Interval: time.Millisecond, BatchSize: 10})
		require.NoError(t, w.schedule())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, w.stopAndWait(ctx))
		w.run(context.Background())
		requireNoMaintenanceGoroutine(t)
	})

	t.Run("while running", func(t *testing.T) {
		_, store := sqliteStore(t)
		service := newConfiguredService(retentionConfig(), store, publisher)
		host := newTestRuntimeHost()
		t.Cleanup(host.close)
		lifecycle := lifecycleContext(host, plugin.DefaultInstance)
		require.NoError(t, service.start(lifecycle))
		require.NoError(t, service.openTraffic(lifecycle))
		host.releaseTraffic()
		require.Eventually(t, func() bool { return maintenanceStacks() != "" }, 2*time.Second, time.Millisecond)
		stopWithin(t, service)
	})
}
