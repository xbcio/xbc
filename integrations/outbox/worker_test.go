package outbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func fastWorkerConfig() WorkerConfig {
	return WorkerConfig{
		PollInterval: 2 * time.Millisecond, BatchSize: 4, Concurrency: 2,
		LeaseDuration: 100 * time.Millisecond, RenewInterval: 20 * time.Millisecond,
		PublishTimeout: 100 * time.Millisecond, DatabaseTimeout: time.Second,
		MaxAttempts: 4, InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		Jitter: 0,
	}
}

func startWorker(t *testing.T, w *worker) {
	t.Helper()
	require.NoError(t, w.schedule())
	go w.run(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, w.stopAndWait(ctx))
	})
}

func waitRowStatus(t *testing.T, db *gorm.DB, table string, want Status) outboxRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var row outboxRow
		if err := db.Table(table).First(&row).Error; err == nil && row.Status == want {
			return row
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for status %s", want)
	return outboxRow{}
}

func waitStatusCount(t *testing.T, db *gorm.DB, table string, status Status, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int64
		if err := db.Table(table).Where("status = ?", status).Count(&count).Error; err == nil && count == want {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	var count int64
	_ = db.Table(table).Where("status = ?", status).Count(&count).Error
	t.Fatalf("status %s count = %d, want %d", status, count, want)
}

type observingStore struct {
	Store
	mu         sync.Mutex
	claimOwner []string
	renew      func(context.Context, string, string, time.Time) (bool, error)
}

func (s *observingStore) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit, maxAttempts int) ([]Record, error) {
	records, err := s.Store.Claim(ctx, owner, now, lease, limit, maxAttempts)
	if len(records) > 0 {
		s.mu.Lock()
		s.claimOwner = append(s.claimOwner, owner)
		s.mu.Unlock()
	}
	return records, err
}

func (s *observingStore) Renew(ctx context.Context, id, owner string, expires time.Time) (bool, error) {
	if s.renew != nil {
		return s.renew(ctx, id, owner, expires)
	}
	return s.Store.Renew(ctx, id, owner, expires)
}

func (s *observingStore) owners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.claimOwner...)
}

func TestWorkerRetriesWithFreshOwnerTokensThenPublishes(t *testing.T) {
	db, sqlStore := sqliteStore(t)
	store := &observingStore{Store: sqlStore}
	service := newService(store, 1024)
	enqueueCommitted(t, db, service, Event{Topic: "retry", Payload: []byte("body")})
	var attempts atomic.Int32
	publisher := PublisherFunc(func(_ context.Context, event Event) error {
		event.Payload[0] = 'X'
		if attempts.Add(1) < 3 {
			return errors.New("temporary")
		}
		return nil
	})
	w, err := newWorker(store, publisher, fastWorkerConfig())
	require.NoError(t, err)
	startWorker(t, w)
	row := waitRowStatus(t, db, sqlStore.table, StatusPublished)
	assert.Equal(t, 3, row.Attempts)
	assert.Equal(t, int32(3), attempts.Load())
	owners := store.owners()
	require.Len(t, owners, 3)
	assert.NotEqual(t, owners[0], owners[1])
	assert.NotEqual(t, owners[1], owners[2])
	assert.NotEqual(t, owners[0], owners[2])
}

func TestWorkerPublishesOutsideDatabaseTransaction(t *testing.T) {
	db, store := sqliteStore(t)
	require.NoError(t, db.Exec("CREATE TABLE publish_probe (id TEXT PRIMARY KEY)").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	service := newService(store, 1024)
	event := enqueueCommitted(t, db, service, Event{Topic: "probe"})

	publisher := PublisherFunc(func(ctx context.Context, got Event) error {
		return db.WithContext(ctx).Exec("INSERT INTO publish_probe(id) VALUES (?)", got.ID).Error
	})
	cfg := fastWorkerConfig()
	cfg.DatabaseTimeout = 250 * time.Millisecond
	cfg.PublishTimeout = 500 * time.Millisecond
	w, err := newWorker(store, publisher, cfg)
	require.NoError(t, err)
	startWorker(t, w)
	waitRowStatus(t, db, store.table, StatusPublished)
	var count int64
	require.NoError(t, db.Table("publish_probe").Where("id = ?", event.ID).Count(&count).Error)
	assert.Equal(t, int64(1), count, "publisher must be able to acquire the only DB connection")
}

func TestLeaseRenewalLossCancelsPublisherAndRecordsFailure(t *testing.T) {
	db, sqlStore := sqliteStore(t)
	renewed := make(chan struct{})
	var renewOnce sync.Once
	store := &observingStore{Store: sqlStore}
	store.renew = func(context.Context, string, string, time.Time) (bool, error) {
		renewOnce.Do(func() { close(renewed) })
		return false, nil
	}
	service := newService(store, 1024)
	enqueueCommitted(t, db, service, Event{Topic: "lease-loss"})
	publisherCanceled := make(chan struct{})
	publisher := PublisherFunc(func(ctx context.Context, _ Event) error {
		<-ctx.Done()
		close(publisherCanceled)
		return ctx.Err()
	})
	cfg := fastWorkerConfig()
	cfg.MaxAttempts = 1
	cfg.LeaseDuration = 200 * time.Millisecond
	cfg.RenewInterval = 10 * time.Millisecond
	cfg.PublishTimeout = time.Second
	w, err := newWorker(store, publisher, cfg)
	require.NoError(t, err)
	startWorker(t, w)
	row := waitRowStatus(t, db, sqlStore.table, StatusDead)
	select {
	case <-renewed:
	default:
		t.Fatal("lease was not renewed")
	}
	select {
	case <-publisherCanceled:
	default:
		t.Fatal("publisher context was not canceled after lease loss")
	}
	assert.Contains(t, row.LastError, "lease could not be maintained")
}

func TestWorkerMovesExhaustedAndPanickingEventsToDeadState(t *testing.T) {
	t.Run("returned errors", func(t *testing.T) {
		db, store := sqliteStore(t)
		service := newService(store, 1024)
		enqueueCommitted(t, db, service, Event{Topic: "dead"})
		cfg := fastWorkerConfig()
		cfg.MaxAttempts = 2
		w, err := newWorker(store, PublisherFunc(func(context.Context, Event) error { return errors.New("always fails") }), cfg)
		require.NoError(t, err)
		startWorker(t, w)
		row := waitRowStatus(t, db, store.table, StatusDead)
		assert.Equal(t, 2, row.Attempts)
		assert.Contains(t, row.LastError, "always fails")
	})

	t.Run("publisher panic", func(t *testing.T) {
		db, store := sqliteStore(t)
		service := newService(store, 1024)
		enqueueCommitted(t, db, service, Event{Topic: "panic"})
		cfg := fastWorkerConfig()
		cfg.MaxAttempts = 1
		w, err := newWorker(store, PublisherFunc(func(context.Context, Event) error { panic("boom") }), cfg)
		require.NoError(t, err)
		startWorker(t, w)
		row := waitRowStatus(t, db, store.table, StatusDead)
		assert.Equal(t, 1, row.Attempts)
		assert.Equal(t, "outbox: publisher panicked", row.LastError)
	})
}

func TestWorkerConcurrencyBoundsClaimsAndPublishing(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	const events = 6
	for i := range events {
		enqueueCommitted(t, db, service, Event{Topic: "bounded", Key: string(rune('a' + i))})
	}

	started := make(chan struct{}, events)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	publisher := PublisherFunc(func(context.Context, Event) error {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return nil
	})
	cfg := fastWorkerConfig()
	cfg.BatchSize = 100
	cfg.Concurrency = 2
	cfg.PublishTimeout = 2 * time.Second
	w, err := newWorker(store, publisher, cfg)
	require.NoError(t, err)
	startWorker(t, w)
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	for range cfg.Concurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("publisher did not fill configured concurrency")
		}
	}
	select {
	case <-started:
		t.Fatal("publisher exceeded configured concurrency before a slot was released")
	case <-time.After(40 * time.Millisecond):
	}

	var processing, pending int64
	require.NoError(t, db.Table(store.table).Where("status = ?", StatusProcessing).Count(&processing).Error)
	require.NoError(t, db.Table(store.table).Where("status = ?", StatusPending).Count(&pending).Error)
	assert.Equal(t, int64(cfg.Concurrency), processing)
	assert.Equal(t, int64(events-cfg.Concurrency), pending, "rows without a worker slot must remain unclaimed")
	releaseAll()
	waitStatusCount(t, db, store.table, StatusPublished, events)
	assert.Equal(t, int32(cfg.Concurrency), maximum.Load())
}

func TestWorkerStopCompletesWhenScheduledTaskNeverStarts(t *testing.T) {
	_, store := sqliteStore(t)
	w, err := newWorker(store, PublisherFunc(func(context.Context, Event) error { return nil }), fastWorkerConfig())
	require.NoError(t, err)
	require.NoError(t, w.schedule())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.stopAndWait(ctx))
	// A host that invokes an already-admitted task late must still return
	// without polling after Stop has completed.
	w.run(context.Background())
}

func TestWorkerStopDrainsAcceptedPublishAndIsConcurrent(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	enqueueCommitted(t, db, service, Event{Topic: "drain"})
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
	cfg := fastWorkerConfig()
	cfg.PublishTimeout = time.Second
	w, err := newWorker(store, publisher, cfg)
	require.NoError(t, err)
	startWorker(t, w)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publish did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	for range 2 {
		go func() { defer wg.Done(); results <- w.stopAndWait(ctx) }()
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-results:
		t.Fatalf("stop returned before accepted publish drained: %v", err)
	default:
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	waitRowStatus(t, db, store.table, StatusPublished)
}

func TestRetryDelayIsExponentialAndCapped(t *testing.T) {
	assert.Equal(t, time.Second, retryDelay(1, time.Second, 5*time.Second, 0))
	assert.Equal(t, 2*time.Second, retryDelay(2, time.Second, 5*time.Second, 0))
	assert.Equal(t, 4*time.Second, retryDelay(3, time.Second, 5*time.Second, 0))
	assert.Equal(t, 5*time.Second, retryDelay(4, time.Second, 5*time.Second, 0))
	assert.Equal(t, 5*time.Second, retryDelay(100, time.Second, 5*time.Second, 0))
}
