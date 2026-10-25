package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedRow inserts a row exactly as given, filling only the columns a test did
// not care about. Retention tests need control over status and updated_at,
// which the public API only ever sets to "now".
func seedRow(t *testing.T, db *gorm.DB, table string, row outboxRow) outboxRow {
	t.Helper()
	require.NoError(t, db.Table(table).Create(fillRow(&row)).Error)
	return row
}

func fillRow(row *outboxRow) *outboxRow {
	if row.Topic == "" {
		row.Topic = "retention"
	}
	if row.Payload == nil {
		row.Payload = []byte("payload-" + row.ID)
	}
	if row.Headers == nil {
		row.Headers = []byte(`{"trace":"` + row.ID + `"}`)
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = time.Now().UTC()
	}
	if row.OccurredAt.IsZero() {
		row.OccurredAt = row.UpdatedAt
	}
	if row.NextAttemptAt.IsZero() {
		row.NextAttemptAt = row.UpdatedAt
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = row.UpdatedAt
	}
	return row
}

func rowStatuses(t *testing.T, db *gorm.DB, table string) map[string]Status {
	t.Helper()
	var rows []outboxRow
	require.NoError(t, db.Table(table).Find(&rows).Error)
	statuses := make(map[string]Status, len(rows))
	for _, row := range rows {
		statuses[row.ID] = row.Status
	}
	return statuses
}

func takeRow(t *testing.T, db *gorm.DB, table, id string) outboxRow {
	t.Helper()
	var row outboxRow
	require.NoError(t, db.Table(table).Where("id = ?", id).Take(&row).Error)
	return row
}

func retentionService(store Store, retention RetentionConfig) *Service {
	config := defaultConfig()
	config.Retention = retention
	return newConfiguredService(config, store, nil)
}

func TestPurgeRemovesOnlyExpiredPublishedRowsUnlessDeadRetentionIsSet(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	expired, recent := now.Add(-48*time.Hour), now.Add(-time.Hour)
	seedRow(t, db, store.table, outboxRow{ID: "published-expired", Status: StatusPublished, UpdatedAt: expired})
	seedRow(t, db, store.table, outboxRow{ID: "published-recent", Status: StatusPublished, UpdatedAt: recent})
	seedRow(t, db, store.table, outboxRow{ID: "dead-expired", Status: StatusDead, UpdatedAt: expired})
	seedRow(t, db, store.table, outboxRow{ID: "dead-recent", Status: StatusDead, UpdatedAt: recent})

	service := retentionService(store, RetentionConfig{Published: 24 * time.Hour, Interval: time.Hour, BatchSize: 100})
	published, dead, err := service.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assert.Zero(t, dead, "dead rows are kept while retention.dead is 0")
	assert.Equal(t, map[string]Status{
		"published-recent": StatusPublished, "dead-expired": StatusDead, "dead-recent": StatusDead,
	}, rowStatuses(t, db, store.table))

	service = retentionService(store, RetentionConfig{Published: 24 * time.Hour, Dead: 24 * time.Hour, Interval: time.Hour, BatchSize: 100})
	published, dead, err = service.Purge(context.Background())
	require.NoError(t, err)
	assert.Zero(t, published)
	assert.Equal(t, 1, dead)
	assert.Equal(t, map[string]Status{
		"published-recent": StatusPublished, "dead-recent": StatusDead,
	}, rowStatuses(t, db, store.table))
}

func TestPurgeSkipsStatesWithZeroRetention(t *testing.T) {
	db, store := sqliteStore(t)
	ancient := time.Now().UTC().Add(-1000 * time.Hour)
	seedRow(t, db, store.table, outboxRow{ID: "published", Status: StatusPublished, UpdatedAt: ancient})
	seedRow(t, db, store.table, outboxRow{ID: "dead", Status: StatusDead, UpdatedAt: ancient})

	service := retentionService(store, RetentionConfig{Interval: time.Hour, BatchSize: 100})
	published, dead, err := service.Purge(context.Background())
	require.NoError(t, err)
	assert.Zero(t, published)
	assert.Zero(t, dead)
	assert.Len(t, rowStatuses(t, db, store.table), 2)
}

func TestPurgeNeverTouchesPendingOrProcessingRows(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	ancient := now.Add(-1000 * time.Hour)
	seedRow(t, db, store.table, outboxRow{ID: "pending", Status: StatusPending, UpdatedAt: ancient})
	// An abandoned lease is Claim's to recover, never retention's, however old
	// its updated_at is.
	expiredLease := ancient.Add(time.Minute)
	seedRow(t, db, store.table, outboxRow{
		ID: "processing", Status: StatusProcessing, UpdatedAt: ancient, Attempts: 1,
		LeaseOwner: "crashed-replica", LeaseExpiresAt: &expiredLease,
	})
	seedRow(t, db, store.table, outboxRow{ID: "published", Status: StatusPublished, UpdatedAt: ancient})
	seedRow(t, db, store.table, outboxRow{ID: "dead", Status: StatusDead, UpdatedAt: ancient})

	service := retentionService(store, RetentionConfig{Published: time.Hour, Dead: time.Hour, Interval: time.Hour, BatchSize: 100})
	published, dead, err := service.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, published)
	assert.Equal(t, 1, dead)
	assert.Equal(t, map[string]Status{"pending": StatusPending, "processing": StatusProcessing}, rowStatuses(t, db, store.table))

	for _, status := range []Status{StatusPending, StatusProcessing, "", "unknown"} {
		deleted, err := store.Purge(context.Background(), now, status, time.Hour, 100)
		require.Errorf(t, err, "Purge must refuse status %q", status)
		assert.Zero(t, deleted)
	}
	assert.Len(t, rowStatuses(t, db, store.table), 2)
}

func TestPurgeBatchLimitDeletesOldestFirstAndRepeatedCallsDrain(t *testing.T) {
	db, store := sqliteStore(t)
	base := time.Now().UTC().Add(-100 * time.Hour)
	for i := range 7 {
		seedRow(t, db, store.table, outboxRow{
			ID: fmt.Sprintf("p%d", i), Status: StatusPublished, UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	service := retentionService(store, RetentionConfig{Published: time.Hour, Interval: time.Hour, BatchSize: 3})

	published, _, err := service.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, published)
	assert.Equal(t, map[string]Status{
		"p3": StatusPublished, "p4": StatusPublished, "p5": StatusPublished, "p6": StatusPublished,
	}, rowStatuses(t, db, store.table), "the oldest rows go first")

	var deleted []int
	for range 3 {
		published, _, err = service.Purge(context.Background())
		require.NoError(t, err)
		deleted = append(deleted, published)
	}
	assert.Equal(t, []int{3, 1, 0}, deleted)
	assert.Empty(t, rowStatuses(t, db, store.table))
}

func TestPurgeDeletesBatchesLargerThanOneDeleteStatement(t *testing.T) {
	db, store := sqliteStore(t)
	ancient := time.Now().UTC().Add(-100 * time.Hour)
	const rows = purgeDeleteChunk*2 + 100
	batch := make([]outboxRow, rows)
	for i := range batch {
		batch[i] = *fillRow(&outboxRow{ID: fmt.Sprintf("row-%05d", i), Status: StatusPublished, UpdatedAt: ancient})
	}
	require.NoError(t, db.Table(store.table).CreateInBatches(batch, 100).Error)

	deleted, err := store.Purge(context.Background(), time.Now(), StatusPublished, time.Hour, rows)
	require.NoError(t, err)
	assert.Equal(t, rows, deleted)
	assert.Empty(t, rowStatuses(t, db, store.table))
}

func TestPurgeLeavesARowRequeuedBetweenSelectAndDelete(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	ancient := now.Add(-48 * time.Hour)
	seedRow(t, db, store.table, outboxRow{ID: "dead-requeued", Status: StatusDead, UpdatedAt: ancient, Attempts: 5, LastError: "boom"})
	seedRow(t, db, store.table, outboxRow{ID: "dead-expired", Status: StatusDead, UpdatedAt: ancient, Attempts: 5})

	// Purge selects both ids, then deletes. The callback runs after the SELECT
	// and before the DELETE statement executes, which is exactly the window a
	// concurrent replay can land in.
	var once sync.Once
	var fired bool
	var requeued int
	var requeueErr error
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("outbox_test:requeue_before_delete", func(*gorm.DB) {
		once.Do(func() {
			fired = true
			requeued, requeueErr = store.Requeue(context.Background(), now, []string{"dead-requeued"})
		})
	}))

	deleted, err := store.Purge(context.Background(), now, StatusDead, time.Hour, 10)
	require.NoError(t, err)
	require.True(t, fired, "the race window was not exercised")
	require.NoError(t, requeueErr)
	assert.Equal(t, 1, requeued)
	assert.Equal(t, 1, deleted, "only the row that is still an expired dead row may be deleted")

	survivor := takeRow(t, db, store.table, "dead-requeued")
	assert.Equal(t, StatusPending, survivor.Status)
	assert.Zero(t, survivor.Attempts)
	assert.Equal(t, map[string]Status{"dead-requeued": StatusPending}, rowStatuses(t, db, store.table))
}

func TestRequeueResetsAttemptsSoClaimCanDeliverAgain(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	event := enqueueCommitted(t, db, service, Event{Topic: "replay", Payload: []byte("body")})
	now := time.Now().Add(time.Second)

	claimed, err := store.Claim(context.Background(), "first", now, time.Minute, 1, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	changed, err := store.MarkFailed(context.Background(), event.ID, "first", now, true, "broker rejected")
	require.NoError(t, err)
	require.True(t, changed)
	dead := takeRow(t, db, store.table, event.ID)
	require.Equal(t, StatusDead, dead.Status)
	require.Equal(t, 1, dead.Attempts)

	requeued, err := store.Requeue(context.Background(), now, []string{event.ID})
	require.NoError(t, err)
	assert.Equal(t, 1, requeued)
	row := takeRow(t, db, store.table, event.ID)
	assert.Equal(t, StatusPending, row.Status)
	assert.Zero(t, row.Attempts, "Claim sends any row at max_attempts straight back to dead")
	assert.Empty(t, row.LeaseOwner)
	assert.Nil(t, row.LeaseExpiresAt)
	assert.Equal(t, "broker rejected", row.LastError, "the failure stays visible until the next attempt")
	assert.WithinDuration(t, now, row.NextAttemptAt, time.Second)

	again, err := store.Claim(context.Background(), "second", now.Add(time.Second), time.Minute, 1, 1)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, event.ID, again[0].Event.ID)
	assert.Equal(t, 1, again[0].Attempts)
}

func TestRequeuedDeadEventIsPublishedByTheWorker(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	event := enqueueCommitted(t, db, service, Event{Topic: "replay", Payload: []byte("body")})
	var failing atomic.Bool
	failing.Store(true)
	var calls atomic.Int32
	publisher := PublisherFunc(func(context.Context, Event) error {
		calls.Add(1)
		if failing.Load() {
			return errors.New("broker down")
		}
		return nil
	})
	cfg := fastWorkerConfig()
	cfg.MaxAttempts = 2
	w, err := newWorker(store, publisher, cfg)
	require.NoError(t, err)
	startWorker(t, w)

	dead := waitRowStatus(t, db, store.table, StatusDead)
	require.Equal(t, 2, dead.Attempts)
	require.Equal(t, int32(2), calls.Load())

	failing.Store(false)
	requeued, err := service.Requeue(context.Background(), []string{event.ID})
	require.NoError(t, err)
	assert.Equal(t, 1, requeued)
	published := waitRowStatus(t, db, store.table, StatusPublished)
	assert.Equal(t, 1, published.Attempts)
	assert.Equal(t, int32(3), calls.Load())
	assert.Empty(t, published.LastError)
}

func TestRequeuedEventStillDiesAfterMaxAttempts(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	event := enqueueCommitted(t, db, service, Event{Topic: "replay"})
	var calls atomic.Int32
	publisher := PublisherFunc(func(context.Context, Event) error {
		calls.Add(1)
		return errors.New("always fails")
	})
	cfg := fastWorkerConfig()
	cfg.MaxAttempts = 2
	w, err := newWorker(store, publisher, cfg)
	require.NoError(t, err)
	startWorker(t, w)

	waitRowStatus(t, db, store.table, StatusDead)
	require.Equal(t, int32(2), calls.Load())

	requeued, err := service.Requeue(context.Background(), []string{event.ID})
	require.NoError(t, err)
	require.Equal(t, 1, requeued)
	row := waitRowStatus(t, db, store.table, StatusDead)
	assert.Equal(t, 2, row.Attempts)
	assert.Equal(t, int32(4), calls.Load(), "a replayed event gets exactly one more max_attempts budget")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(4), calls.Load(), "and then stays dead")
}

func TestRequeueOnlyMovesDeadRows(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	lease := now.Add(time.Hour)
	seedRow(t, db, store.table, outboxRow{ID: "published", Status: StatusPublished, UpdatedAt: now.Add(-time.Hour), Attempts: 2})
	seedRow(t, db, store.table, outboxRow{
		ID: "processing", Status: StatusProcessing, UpdatedAt: now.Add(-time.Minute), Attempts: 1,
		LeaseOwner: "healthy-replica", LeaseExpiresAt: &lease,
	})
	seedRow(t, db, store.table, outboxRow{ID: "pending", Status: StatusPending, UpdatedAt: now.Add(-time.Minute), Attempts: 3})
	seedRow(t, db, store.table, outboxRow{ID: "dead", Status: StatusDead, UpdatedAt: now.Add(-time.Hour), Attempts: 10, LastError: "boom"})
	before := map[string]outboxRow{}
	for _, id := range []string{"published", "processing", "pending"} {
		before[id] = takeRow(t, db, store.table, id)
	}

	service := newService(store, 1024)
	requeued, err := service.Requeue(context.Background(), []string{"published", "processing", "pending", "dead", "missing"})
	require.NoError(t, err)
	assert.Equal(t, 1, requeued)
	for id, want := range before {
		assert.Equal(t, want, takeRow(t, db, store.table, id), "%s must be untouched", id)
	}
	dead := takeRow(t, db, store.table, "dead")
	assert.Equal(t, StatusPending, dead.Status)
	assert.Zero(t, dead.Attempts)
	assert.Equal(t, "boom", dead.LastError)
}

// requeueSpyStore records the ids Service hands to the Store.
type requeueSpyStore struct {
	Store
	mu    sync.Mutex
	calls [][]string
}

func (s *requeueSpyStore) Requeue(ctx context.Context, now time.Time, ids []string) (int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), ids...))
	s.mu.Unlock()
	return s.Store.Requeue(ctx, now, ids)
}

func TestRequeueValidatesAndDeduplicatesIDs(t *testing.T) {
	_, sqlStore := sqliteStore(t)
	spy := &requeueSpyStore{Store: sqlStore}
	service := newService(spy, 1024)
	ctx := context.Background()

	_, err := service.Requeue(ctx, nil)
	require.Error(t, err)
	_, err = service.Requeue(ctx, []string{})
	require.Error(t, err)

	tooMany := make([]string, maxManageBatch+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("id-%d", i)
	}
	_, err = service.Requeue(ctx, tooMany)
	require.Error(t, err)
	assert.Empty(t, spy.calls, "rejected input must never reach the store")

	requeued, err := service.Requeue(ctx, tooMany[:maxManageBatch])
	require.NoError(t, err, "exactly the maximum is accepted")
	assert.Zero(t, requeued)

	spy.calls = nil
	_, err = service.Requeue(ctx, []string{"a", "b", "a", "c", "b"})
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"a", "b", "c"}}, spy.calls, "duplicates are collapsed, first occurrence wins")
}

func TestListDeadPagesWithoutGapsOrRepeats(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	for i := range 5 {
		id := fmt.Sprintf("dead-%d", i)
		seedRow(t, db, store.table, outboxRow{ID: id, Status: StatusDead, UpdatedAt: now, LastError: "boom " + id, Attempts: 10})
	}
	// Rows in other states sort between the dead ones and must not appear.
	seedRow(t, db, store.table, outboxRow{ID: "dead-1a", Status: StatusPending})
	seedRow(t, db, store.table, outboxRow{ID: "dead-3a", Status: StatusPublished})
	seedRow(t, db, store.table, outboxRow{ID: "dead-3b", Status: StatusProcessing})

	service := newService(store, 1024)
	var got []string
	after := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10, "paging must terminate")
		page, err := service.ListDead(context.Background(), 2, after)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		require.LessOrEqual(t, len(page), 2)
		for _, record := range page {
			got = append(got, record.Event.ID)
			assert.Equal(t, StatusDead, record.Status)
			assert.Equal(t, "boom "+record.Event.ID, record.LastError)
			assert.Equal(t, "payload-"+record.Event.ID, string(record.Event.Payload))
			assert.Equal(t, map[string]string{"trace": record.Event.ID}, record.Event.Headers)
		}
		after = page[len(page)-1].Event.ID
	}
	assert.Equal(t, []string{"dead-0", "dead-1", "dead-2", "dead-3", "dead-4"}, got)
}

func TestListDeadValidatesLimit(t *testing.T) {
	_, store := sqliteStore(t)
	service := newService(store, 1024)
	for _, limit := range []int{-1, 0, maxManageBatch + 1} {
		_, err := service.ListDead(context.Background(), limit, "")
		require.Errorf(t, err, "limit %d", limit)
	}
	records, err := service.ListDead(context.Background(), maxManageBatch, "")
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestManagerRejectsCanceledContextAndClosedService(t *testing.T) {
	_, store := sqliteStore(t)
	service := newService(store, 1024)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := service.Purge(canceled)
	require.ErrorIs(t, err, context.Canceled)
	_, err = service.ListDead(canceled, 1, "")
	require.ErrorIs(t, err, context.Canceled)
	_, err = service.Requeue(canceled, []string{"a"})
	require.ErrorIs(t, err, context.Canceled)

	require.NoError(t, service.stop(context.Background()))
	_, _, err = service.Purge(context.Background())
	require.ErrorIs(t, err, ErrClosed)
	_, err = service.ListDead(context.Background(), 1, "")
	require.ErrorIs(t, err, ErrClosed)
	_, err = service.Requeue(context.Background(), []string{"a"})
	require.ErrorIs(t, err, ErrClosed)
}

// stallingStore blocks Purge and ListDead until their context is canceled,
// then models a database call that is slow to return by waiting for release.
type stallingStore struct {
	Store
	entered     chan struct{}
	canceled    chan struct{}
	release     chan struct{}
	cancelOnce  sync.Once
	releaseOnce sync.Once
}

func newStallingStore(store Store) *stallingStore {
	return &stallingStore{
		Store: store, entered: make(chan struct{}, 1),
		canceled: make(chan struct{}), release: make(chan struct{}),
	}
}

// unblock lets stalled calls return. It is safe to call more than once.
func (s *stallingStore) unblock() { s.releaseOnce.Do(func() { close(s.release) }) }

func (s *stallingStore) stall(ctx context.Context) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	s.cancelOnce.Do(func() { close(s.canceled) })
	<-s.release
	return ctx.Err()
}

func (s *stallingStore) Purge(ctx context.Context, _ time.Time, _ Status, _ time.Duration, _ int) (int, error) {
	return 0, s.stall(ctx)
}

func (s *stallingStore) ListDead(ctx context.Context, _ int, _ string) ([]Record, error) {
	return nil, s.stall(ctx)
}

func (s *stallingStore) Requeue(ctx context.Context, _ time.Time, _ []string) (int, error) {
	return 0, s.stall(ctx)
}

func TestStopCancelsAnInFlightManagerCall(t *testing.T) {
	_, sqlStore := sqliteStore(t)
	store := newStallingStore(sqlStore)
	store.unblock()
	service := newService(store, 1024)

	result := make(chan error, 1)
	go func() {
		_, err := service.ListDead(context.Background(), 10, "")
		result <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("ListDead did not reach the store")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, service.stop(stopCtx), "Stop must not wait out a stuck management call")
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("ListDead did not return after Stop")
	}
}

// managerCalls lists one valid call per Manager method, so a test can run the
// same property against all three.
var managerCalls = []struct {
	name string
	call func(*Service) error
}{
	{"Purge", func(s *Service) error { _, _, err := s.Purge(context.Background()); return err }},
	{"ListDead", func(s *Service) error { _, err := s.ListDead(context.Background(), 10, ""); return err }},
	{"Requeue", func(s *Service) error { _, err := s.Requeue(context.Background(), []string{"id"}); return err }},
}

// TestStopWaitsForAnAdmittedManagerCallBeforeReleasingTheDatabase pins the
// admission half of manage: Stop finishes only after a call it admitted has
// returned, because the database is released as soon as Stop completes. The
// store keeps the call stuck after Stop has canceled its context, so only the
// admission accounting can hold Stop back.
func TestStopWaitsForAnAdmittedManagerCallBeforeReleasingTheDatabase(t *testing.T) {
	for _, tc := range managerCalls {
		t.Run(tc.name, func(t *testing.T) {
			_, sqlStore := sqliteStore(t)
			store := newStallingStore(sqlStore)
			t.Cleanup(store.unblock)
			service := newService(store, 1024)

			result := make(chan error, 1)
			go func() { result <- tc.call(service) }()
			waitSignal(t, store.entered, "the call did not reach the store")

			stopped := make(chan error, 1)
			go func() { stopped <- service.stop(context.Background()) }()
			waitSignal(t, store.canceled, "Stop did not cancel the admitted call")
			// The call's context is canceled, but the call is still inside the
			// store, so neither Stop nor the admission drain may be done.
			select {
			case err := <-stopped:
				t.Fatalf("Stop returned (%v) while an admitted call was still inside the store", err)
			case <-service.closeDone:
				t.Fatal("the admission drain finished while an admitted call was still inside the store")
			case <-time.After(40 * time.Millisecond):
			}

			store.unblock()
			select {
			case err := <-stopped:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("Stop did not finish after the admitted call returned")
			}
			select {
			case err := <-result:
				require.ErrorIs(t, err, ErrClosed)
			case <-time.After(2 * time.Second):
				t.Fatal("the admitted call did not return")
			}
		})
	}
}

// callCountingStore counts calls that reach the Store's management methods.
type callCountingStore struct {
	Store
	calls atomic.Int32
}

func (s *callCountingStore) Purge(ctx context.Context, now time.Time, status Status, olderThan time.Duration, limit int) (int, error) {
	s.calls.Add(1)
	return s.Store.Purge(ctx, now, status, olderThan, limit)
}

func (s *callCountingStore) ListDead(ctx context.Context, limit int, afterID string) ([]Record, error) {
	s.calls.Add(1)
	return s.Store.ListDead(ctx, limit, afterID)
}

func (s *callCountingStore) Requeue(ctx context.Context, now time.Time, ids []string) (int, error) {
	s.calls.Add(1)
	return s.Store.Requeue(ctx, now, ids)
}

// TestManagerCallsAfterStopNeverReachTheStore pins the other half of manage:
// admission itself refuses a call after Stop, rather than starting it and
// canceling it afterwards. A refused call must not touch the database, which
// Stop may already have released.
func TestManagerCallsAfterStopNeverReachTheStore(t *testing.T) {
	_, sqlStore := sqliteStore(t)
	store := &callCountingStore{Store: sqlStore}
	// Retention must be on, or Purge would return before reaching the store
	// even with admission broken.
	service := retentionService(store, RetentionConfig{Published: time.Hour, Interval: time.Hour, BatchSize: 10})

	for _, tc := range managerCalls {
		require.NoError(t, tc.call(service), tc.name)
	}
	require.Equal(t, int32(len(managerCalls)), store.calls.Load(),
		"control: before Stop every method reaches the store")

	require.NoError(t, service.stop(context.Background()))
	store.calls.Store(0)
	for _, tc := range managerCalls {
		require.ErrorIs(t, tc.call(service), ErrClosed, tc.name)
	}
	assert.Zero(t, store.calls.Load(), "no method may reach the store after Stop")
}

// TestRequeueMakesRowsClaimableWhateverTheirOldNextAttemptAt keeps Requeue from
// depending on the row's history or on clock skew between replicas: it stamps
// next_attempt_at itself, so a dead row whose old retry time lies far in the
// future is delivered at once.
func TestRequeueMakesRowsClaimableWhateverTheirOldNextAttemptAt(t *testing.T) {
	db, store := sqliteStore(t)
	now := time.Now().UTC()
	seedRow(t, db, store.table, outboxRow{
		ID: "future", Status: StatusDead, UpdatedAt: now.Add(-time.Hour), Attempts: 3,
		NextAttemptAt: now.Add(24 * time.Hour),
	})
	seedRow(t, db, store.table, outboxRow{
		ID: "past", Status: StatusDead, UpdatedAt: now.Add(-time.Hour), Attempts: 3,
		NextAttemptAt: now.Add(-72 * time.Hour),
	})

	before, err := store.Claim(context.Background(), "control", now, time.Minute, 10, 10)
	require.NoError(t, err)
	require.Empty(t, before, "control: dead rows are never claimable")

	requeued, err := store.Requeue(context.Background(), now, []string{"future", "past"})
	require.NoError(t, err)
	require.Equal(t, 2, requeued)
	for _, id := range []string{"future", "past"} {
		assert.WithinDuration(t, now, takeRow(t, db, store.table, id).NextAttemptAt, time.Second, id)
	}

	claimed, err := store.Claim(context.Background(), "replayer", now, time.Minute, 10, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2, "both rows must be claimable at the instant they were requeued")
}
