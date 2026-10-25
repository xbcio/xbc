package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"gorm.io/gorm"
)

// Status is the persistent dispatch state.
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusPublished  Status = "published"
	StatusDead       Status = "dead"
)

// Record is a claimed event plus lease/retry metadata.
type Record struct {
	Event          Event
	Status         Status
	Attempts       int
	NextAttemptAt  time.Time
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	// LastError is the most recent failure message. Requeue keeps it, so a
	// replayed row still shows why it died until its next attempt overwrites it.
	LastError string
}

// Store is the persistence seam used by Service and the dispatcher worker.
// Claim must atomically transfer each returned row to owner.
//
// Purge deletes at most limit rows that entered status (published or dead)
// more than olderThan before now and returns how many it deleted; it must
// never delete a row whose state changed after it was selected. ListDead pages
// dead rows by ascending id, starting after afterID. Requeue returns dead rows
// to pending with a fresh attempt budget and reports how many ids named a dead
// row; every other row is left alone.
type Store interface {
	Migrate(context.Context) error
	Insert(context.Context, *gorm.DB, Event) error
	Claim(context.Context, string, time.Time, time.Duration, int, int) ([]Record, error)
	Renew(context.Context, string, string, time.Time) (bool, error)
	MarkPublished(context.Context, string, string, time.Time) (bool, error)
	MarkFailed(context.Context, string, string, time.Time, bool, string) (bool, error)
	Purge(ctx context.Context, now time.Time, status Status, olderThan time.Duration, limit int) (int, error)
	ListDead(ctx context.Context, limit int, afterID string) ([]Record, error)
	Requeue(ctx context.Context, now time.Time, ids []string) (int, error)
}

// SQLStore is a portable GORM Store. Claim uses a conditional UPDATE per
// candidate instead of SKIP LOCKED, making it work on SQLite as well as server
// databases while preserving single-owner atomicity.
type SQLStore struct {
	db    *gorm.DB
	table string
}

var tablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// NewSQLStore constructs a store over db and a validated table name.
func NewSQLStore(db *gorm.DB, table string) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("outbox: SQL store requires a database")
	}
	if !tablePattern.MatchString(table) {
		return nil, fmt.Errorf("outbox: invalid table name %q", table)
	}
	return &SQLStore{db: db, table: table}, nil
}

type outboxRow struct {
	ID             string     `gorm:"column:id;primaryKey;size:64"`
	Topic          string     `gorm:"column:topic;not null;index:idx_outbox_due,priority:3"`
	EventKey       string     `gorm:"column:event_key"`
	Payload        []byte     `gorm:"column:payload;not null"`
	Headers        []byte     `gorm:"column:headers;not null"`
	OccurredAt     time.Time  `gorm:"column:occurred_at;not null"`
	Status         Status     `gorm:"column:status;size:16;not null;index:idx_outbox_due,priority:1;index:idx_outbox_retention,priority:1"`
	Attempts       int        `gorm:"column:attempts;not null"`
	NextAttemptAt  time.Time  `gorm:"column:next_attempt_at;not null;index:idx_outbox_due,priority:2"`
	LeaseOwner     string     `gorm:"column:lease_owner;size:64;index"`
	LeaseExpiresAt *time.Time `gorm:"column:lease_expires_at;index"`
	LastError      string     `gorm:"column:last_error;size:512"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;not null;index:idx_outbox_retention,priority:2"`
	PublishedAt    *time.Time `gorm:"column:published_at"`
}

func (s *SQLStore) Migrate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.db.WithContext(ctx).Table(s.table).AutoMigrate(&outboxRow{}); err != nil {
		return fmt.Errorf("outbox: migrate table: %w", err)
	}
	return nil
}

func (s *SQLStore) Insert(ctx context.Context, tx *gorm.DB, event Event) error {
	if err := requireTransaction(tx); err != nil {
		return err
	}
	if event.Headers == nil {
		event.Headers = map[string]string{}
	}
	headers, err := json.Marshal(event.Headers)
	if err != nil {
		return errors.New("outbox: encode event headers")
	}
	now := time.Now().UTC()
	row := outboxRow{
		ID: event.ID, Topic: event.Topic, EventKey: event.Key,
		Payload: append([]byte{}, event.Payload...), Headers: headers,
		OccurredAt: event.OccurredAt.UTC(), Status: StatusPending,
		NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.WithContext(ctx).Table(s.table).Create(&row).Error; err != nil {
		return fmt.Errorf("outbox: insert row: %w", err)
	}
	return nil
}

func (s *SQLStore) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit, maxAttempts int) ([]Record, error) {
	if owner == "" || lease <= 0 || limit <= 0 || maxAttempts <= 0 {
		return nil, errors.New("outbox: invalid claim owner, lease, limit, or max attempts")
	}
	now = now.UTC()
	expires := now.Add(lease)

	// A process can disappear after claiming its last permitted attempt. Once
	// that lease expires there is no result to record, so make the row's final
	// state explicit instead of reclaiming it forever or exceeding the limit.
	terminal := s.db.WithContext(ctx).Table(s.table).
		Where("(status = ? AND next_attempt_at <= ? AND attempts >= ?) OR (status = ? AND lease_expires_at <= ? AND attempts >= ?)",
			StatusPending, now, maxAttempts, StatusProcessing, now, maxAttempts).
		Updates(map[string]any{
			"status": StatusDead, "updated_at": now, "lease_owner": "", "lease_expires_at": nil,
			"last_error": "maximum dispatch attempts exhausted before completion",
		})
	if terminal.Error != nil {
		return nil, fmt.Errorf("outbox: finalize exhausted events: %w", terminal.Error)
	}

	var ids []string
	candidateLimit := limit * 4
	if candidateLimit < limit {
		candidateLimit = limit
	}
	err := s.db.WithContext(ctx).Table(s.table).Select("id").
		Where("attempts < ? AND ((status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_expires_at <= ?))", maxAttempts, StatusPending, now, StatusProcessing, now).
		Order("next_attempt_at ASC, created_at ASC").Limit(candidateLimit).Scan(&ids).Error
	if err != nil {
		return nil, fmt.Errorf("outbox: select claim candidates: %w", err)
	}
	records := make([]Record, 0, min(limit, len(ids)))
	for _, id := range ids {
		if len(records) == limit {
			break
		}
		result := s.db.WithContext(ctx).Table(s.table).
			Where("id = ? AND attempts < ? AND ((status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_expires_at <= ?))", id, maxAttempts, StatusPending, now, StatusProcessing, now).
			Updates(map[string]any{
				"status": StatusProcessing, "lease_owner": owner,
				"lease_expires_at": expires, "attempts": gorm.Expr("attempts + 1"), "updated_at": now,
			})
		if result.Error != nil {
			return nil, fmt.Errorf("outbox: claim event: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			continue
		}
		var row outboxRow
		if err := s.db.WithContext(ctx).Table(s.table).
			Where("id = ? AND status = ? AND lease_owner = ?", id, StatusProcessing, owner).Take(&row).Error; err != nil {
			return nil, fmt.Errorf("outbox: load claimed event: %w", err)
		}
		record, err := row.record()
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func requireTransaction(tx *gorm.DB) error {
	if tx == nil || tx.Statement == nil || tx.Statement.ConnPool == nil {
		return ErrTransactionRequired
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return ErrTransactionRequired
	}
	return nil
}

func (s *SQLStore) Renew(ctx context.Context, id, owner string, expires time.Time) (bool, error) {
	result := s.db.WithContext(ctx).Table(s.table).
		Where("id = ? AND status = ? AND lease_owner = ?", id, StatusProcessing, owner).
		Updates(map[string]any{"lease_expires_at": expires.UTC(), "updated_at": time.Now().UTC()})
	return changed("renew lease", result)
}

func (s *SQLStore) MarkPublished(ctx context.Context, id, owner string, at time.Time) (bool, error) {
	at = at.UTC()
	result := s.db.WithContext(ctx).Table(s.table).
		Where("id = ? AND status = ? AND lease_owner = ?", id, StatusProcessing, owner).
		Updates(map[string]any{
			"status": StatusPublished, "published_at": at, "updated_at": at,
			"lease_owner": "", "lease_expires_at": nil, "last_error": "",
		})
	return changed("mark published", result)
}

func (s *SQLStore) MarkFailed(ctx context.Context, id, owner string, next time.Time, dead bool, message string) (bool, error) {
	status := StatusPending
	if dead {
		status = StatusDead
	}
	next = next.UTC()
	result := s.db.WithContext(ctx).Table(s.table).
		Where("id = ? AND status = ? AND lease_owner = ?", id, StatusProcessing, owner).
		Updates(map[string]any{
			"status": status, "next_attempt_at": next, "updated_at": time.Now().UTC(),
			"lease_owner": "", "lease_expires_at": nil, "last_error": truncate(message, 512),
		})
	return changed("mark failed", result)
}

// purgeDeleteChunk bounds how many ids one DELETE binds, so a large batch_size
// cannot exceed a database's bind-parameter limit.
const purgeDeleteChunk = 500

// Purge selects the expired ids first and deletes them in a second statement
// that repeats the status and age conditions. It avoids a DELETE over a
// LIMITed subquery, which MySQL rejects, and the repeated conditions mean a row
// that was requeued or otherwise changed between the two statements survives.
// updated_at is the time a row entered published or dead, so both states share
// one (status, updated_at) index.
func (s *SQLStore) Purge(ctx context.Context, now time.Time, status Status, olderThan time.Duration, limit int) (int, error) {
	if status != StatusPublished && status != StatusDead {
		return 0, fmt.Errorf("outbox: cannot purge rows with status %q", status)
	}
	if olderThan <= 0 || limit <= 0 {
		return 0, errors.New("outbox: invalid purge age or limit")
	}
	cutoff := now.UTC().Add(-olderThan)

	var ids []string
	err := s.db.WithContext(ctx).Table(s.table).Select("id").
		Where("status = ? AND updated_at < ?", status, cutoff).
		Order("updated_at ASC").Limit(limit).Scan(&ids).Error
	if err != nil {
		return 0, fmt.Errorf("outbox: select expired events: %w", err)
	}

	deleted := 0
	for start := 0; start < len(ids); start += purgeDeleteChunk {
		chunk := ids[start:min(start+purgeDeleteChunk, len(ids))]
		result := s.db.WithContext(ctx).Table(s.table).
			Where("id IN ? AND status = ? AND updated_at < ?", chunk, status, cutoff).
			Delete(&outboxRow{})
		if result.Error != nil {
			return deleted, fmt.Errorf("outbox: delete expired events: %w", result.Error)
		}
		deleted += int(result.RowsAffected)
	}
	return deleted, nil
}

// ListDead pages dead rows by id. Ids are random, so the order has no business
// meaning, but it is stable, which is all keyset paging needs.
func (s *SQLStore) ListDead(ctx context.Context, limit int, afterID string) ([]Record, error) {
	if limit <= 0 {
		return nil, errors.New("outbox: dead event page limit must be positive")
	}
	var rows []outboxRow
	err := s.db.WithContext(ctx).Table(s.table).
		Where("status = ? AND id > ?", StatusDead, afterID).
		Order("id ASC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("outbox: list dead events: %w", err)
	}
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		record, err := row.record()
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Requeue returns dead rows to pending. attempts must restart at zero: Claim
// converts any row whose attempts already reached the maximum straight back to
// dead. last_error is kept so the replayed row still shows why it died.
func (s *SQLStore) Requeue(ctx context.Context, now time.Time, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	now = now.UTC()
	result := s.db.WithContext(ctx).Table(s.table).
		Where("id IN ? AND status = ?", ids, StatusDead).
		Updates(map[string]any{
			"status": StatusPending, "attempts": 0, "next_attempt_at": now,
			"lease_owner": "", "lease_expires_at": nil, "updated_at": now,
		})
	if result.Error != nil {
		return 0, fmt.Errorf("outbox: requeue dead events: %w", result.Error)
	}
	return int(result.RowsAffected), nil
}

func changed(operation string, result *gorm.DB) (bool, error) {
	if result.Error != nil {
		return false, fmt.Errorf("outbox: %s: %w", operation, result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (r outboxRow) record() (Record, error) {
	headers := map[string]string(nil)
	if len(r.Headers) > 0 && string(r.Headers) != "null" {
		if err := json.Unmarshal(r.Headers, &headers); err != nil {
			return Record{}, fmt.Errorf("outbox: decode headers for event %q: %w", r.ID, err)
		}
	}
	return Record{
		Event:  Event{ID: r.ID, Topic: r.Topic, Key: r.EventKey, Payload: append([]byte(nil), r.Payload...), Headers: headers, OccurredAt: r.OccurredAt.UTC()},
		Status: r.Status, Attempts: r.Attempts, NextAttemptAt: r.NextAttemptAt.UTC(),
		LeaseOwner: r.LeaseOwner, LeaseExpiresAt: cloneTime(r.LeaseExpiresAt),
		LastError: r.LastError,
	}, nil
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
