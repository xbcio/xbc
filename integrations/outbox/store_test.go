package outbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func sqliteStore(t *testing.T) (*gorm.DB, *SQLStore) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", filepath.Join(t.TempDir(), "outbox.db"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	store, err := NewSQLStore(db, "xbc_outbox_events")
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	return db, store
}

func enqueueCommitted(t *testing.T, db *gorm.DB, service *Service, event Event) Event {
	t.Helper()
	var saved Event
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		saved, err = service.Enqueue(context.Background(), tx, event)
		return err
	}))
	return saved
}

func TestEnqueueRequiresCallerTransactionAndCommitsAtomically(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	require.NoError(t, db.Exec("CREATE TABLE business_orders (id TEXT PRIMARY KEY)").Error)

	_, err := service.Enqueue(context.Background(), nil, Event{Topic: "orders.created"})
	require.ErrorIs(t, err, ErrTransactionRequired)
	_, err = service.Enqueue(context.Background(), db, Event{Topic: "orders.created"})
	require.ErrorIs(t, err, ErrTransactionRequired)
	require.ErrorIs(t, store.Insert(context.Background(), db, Event{}), ErrTransactionRequired)

	event := Event{Topic: "orders.created", Key: "42", Payload: []byte("original"), Headers: map[string]string{"trace": "one"}}
	errRollback := errors.New("rollback business transaction")
	err = db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Exec("INSERT INTO business_orders(id) VALUES (?)", "rolled-back").Error)
		_, enqueueErr := service.Enqueue(context.Background(), tx, event)
		require.NoError(t, enqueueErr)
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)

	var businessCount, outboxCount int64
	require.NoError(t, db.Table("business_orders").Count(&businessCount).Error)
	require.NoError(t, db.Table(store.table).Count(&outboxCount).Error)
	assert.Zero(t, businessCount)
	assert.Zero(t, outboxCount)

	var saved Event
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Exec("INSERT INTO business_orders(id) VALUES (?)", "committed").Error)
		var enqueueErr error
		saved, enqueueErr = service.Enqueue(context.Background(), tx, event)
		return enqueueErr
	}))
	require.NoError(t, db.Table("business_orders").Count(&businessCount).Error)
	require.NoError(t, db.Table(store.table).Count(&outboxCount).Error)
	assert.Equal(t, int64(1), businessCount)
	assert.Equal(t, int64(1), outboxCount)

	event.Payload[0] = 'X'
	event.Headers["trace"] = "mutated"
	records, err := store.Claim(context.Background(), "owner", time.Now().Add(time.Second), time.Minute, 1, 10)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "original", string(records[0].Event.Payload))
	assert.Equal(t, "one", records[0].Event.Headers["trace"])
	records[0].Event.Payload[0] = 'Y'
	assert.Equal(t, "original", string(saved.Payload))
	assert.NotEmpty(t, saved.ID)
	assert.False(t, saved.OccurredAt.IsZero())
}

func TestConditionalClaimIsAtomicAcrossWorkers(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	for i := range 20 {
		enqueueCommitted(t, db, service, Event{Topic: "topic", Key: fmt.Sprint(i)})
	}
	now := time.Now().Add(time.Second)
	var first, second []Record
	var err1, err2 error
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		first, err1 = store.Claim(context.Background(), "one", now, time.Minute, 20, 10)
	}()
	go func() {
		defer wg.Done()
		<-start
		second, err2 = store.Claim(context.Background(), "two", now, time.Minute, 20, 10)
	}()
	close(start)
	wg.Wait()
	require.NoError(t, err1)
	require.NoError(t, err2)
	seen := map[string]string{}
	for _, group := range [][]Record{first, second} {
		for _, record := range group {
			if owner, duplicate := seen[record.Event.ID]; duplicate {
				t.Fatalf("event claimed by %s and %s", owner, record.LeaseOwner)
			}
			seen[record.Event.ID] = record.LeaseOwner
		}
	}
	assert.Len(t, seen, 20)
}

func TestExpiredLeaseIsRecoveredAndStaleOwnerIsFenced(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	event := enqueueCommitted(t, db, service, Event{Topic: "recover"})
	now := time.Now().Add(time.Second)
	first, err := store.Claim(context.Background(), "failed-replica-attempt-1", now, 50*time.Millisecond, 1, 3)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, 1, first[0].Attempts)
	early, err := store.Claim(context.Background(), "healthy-replica-attempt-1", now.Add(10*time.Millisecond), time.Minute, 1, 3)
	require.NoError(t, err)
	assert.Empty(t, early)

	recovered, err := store.Claim(context.Background(), "healthy-replica-attempt-2", now.Add(time.Second), time.Minute, 1, 3)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	assert.Equal(t, 2, recovered[0].Attempts)
	assert.Equal(t, "healthy-replica-attempt-2", recovered[0].LeaseOwner)

	owned, err := store.Renew(context.Background(), event.ID, first[0].LeaseOwner, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.False(t, owned)
	changed, err := store.MarkPublished(context.Background(), event.ID, first[0].LeaseOwner, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.False(t, changed)
	changed, err = store.MarkFailed(context.Background(), event.ID, first[0].LeaseOwner, now.Add(2*time.Second), false, "stale failure")
	require.NoError(t, err)
	assert.False(t, changed)

	changed, err = store.MarkPublished(context.Background(), event.ID, recovered[0].LeaseOwner, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.True(t, changed)
	var row outboxRow
	require.NoError(t, db.Table(store.table).Where("id = ?", event.ID).Take(&row).Error)
	assert.Equal(t, StatusPublished, row.Status)
}

func TestExpiredFinalLeaseBecomesDeadAfterClaimingReplicaCrashes(t *testing.T) {
	db, store := sqliteStore(t)
	service := newService(store, 1024)
	event := enqueueCommitted(t, db, service, Event{Topic: "final-attempt-crash"})
	now := time.Now().Add(time.Second)
	claimed, err := store.Claim(context.Background(), "crashed-final-attempt", now, 25*time.Millisecond, 1, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, 1, claimed[0].Attempts)

	reclaimed, err := store.Claim(context.Background(), "replacement", now.Add(time.Second), time.Minute, 1, 1)
	require.NoError(t, err)
	assert.Empty(t, reclaimed)
	var row outboxRow
	require.NoError(t, db.Table(store.table).Where("id = ?", event.ID).Take(&row).Error)
	assert.Equal(t, StatusDead, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.Empty(t, row.LeaseOwner)
	assert.Nil(t, row.LeaseExpiresAt)
	assert.Contains(t, row.LastError, "maximum dispatch attempts")
}
