package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// maxManageBatch caps how many dead rows one ListDead page returns and how many
// ids one Requeue call accepts. ListDead returns payloads, so an unbounded page
// would let one call load an arbitrary amount of data into memory.
const maxManageBatch = 1000

// manage runs one Manager call under the same admission as Enqueue: it is
// refused with ErrClosed once Stop begins, and Stop waits for a call already
// admitted, because the database must stay open until it returns. Unlike an
// accepted event, a management call is safe to abandon (a purge is resumable,
// a requeue is one atomic statement, a listing is read-only), so Stop also
// cancels the call's context and the call reports ErrClosed rather than
// holding shutdown for the rest of the caller's deadline.
func (s *Service) manage(ctx context.Context, call func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.begin() {
		return ErrClosed
	}
	defer s.inflight.Done()

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.manageCtx, cancel)()

	err := call(callCtx)
	if err != nil && ctx.Err() == nil && s.manageCtx.Err() != nil {
		return ErrClosed
	}
	return err
}

// Purge runs one retention pass: at most retention.batch_size rows are deleted
// for each state whose retention is non-zero, and a state with retention 0 is
// skipped. Call it again to drain a larger backlog. The dispatcher worker runs
// the same deletes on its own schedule; Purge is for hosts that drive cleanup
// themselves, for example from a cron Job, and it works without a worker.
func (s *Service) Purge(ctx context.Context) (published, dead int, err error) {
	err = s.manage(ctx, func(ctx context.Context) error {
		var purgeErr error
		published, dead, purgeErr = purgeBatch(ctx, s.store, s.config.Retention, time.Now().UTC())
		return purgeErr
	})
	return published, dead, err
}

// ListDead returns up to limit dead rows ordered by id, starting after afterID
// (empty for the first page). Pass the last returned Event.ID as the next
// afterID. limit must be between 1 and 1000. Each Record carries the event
// payload and headers plus LastError.
func (s *Service) ListDead(ctx context.Context, limit int, afterID string) ([]Record, error) {
	var records []Record
	err := s.manage(ctx, func(ctx context.Context) error {
		if limit <= 0 || limit > maxManageBatch {
			return fmt.Errorf("outbox: dead event page limit must be between 1 and %d", maxManageBatch)
		}
		var listErr error
		records, listErr = s.store.ListDead(ctx, limit, afterID)
		return listErr
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// Requeue returns the dead rows named by ids to pending with a fresh attempt
// budget and reports how many it moved. An id that names no dead row is
// ignored, so a row that is already pending, processing, or published is never
// touched. ids must hold between 1 and 1000 entries; duplicates count once.
func (s *Service) Requeue(ctx context.Context, ids []string) (int, error) {
	requeued := 0
	err := s.manage(ctx, func(ctx context.Context) error {
		if len(ids) == 0 {
			return errors.New("outbox: at least one event id is required")
		}
		if len(ids) > maxManageBatch {
			return fmt.Errorf("outbox: cannot requeue more than %d events at once", maxManageBatch)
		}
		unique := make([]string, 0, len(ids))
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
		var requeueErr error
		requeued, requeueErr = s.store.Requeue(ctx, time.Now().UTC(), unique)
		return requeueErr
	})
	if err != nil {
		return 0, err
	}
	return requeued, nil
}

// purgeBatch deletes at most r.BatchSize expired rows for each state with a
// non-zero retention. A failure in one state does not skip the other, so a
// permanent problem with one never starves the other's cleanup.
func purgeBatch(ctx context.Context, store Store, r RetentionConfig, now time.Time) (published, dead int, err error) {
	var errs []error
	if r.Published > 0 {
		var purgeErr error
		published, purgeErr = store.Purge(ctx, now, StatusPublished, r.Published, r.BatchSize)
		if purgeErr != nil {
			errs = append(errs, fmt.Errorf("outbox: purge published events: %w", purgeErr))
		}
	}
	if r.Dead > 0 {
		var purgeErr error
		dead, purgeErr = store.Purge(ctx, now, StatusDead, r.Dead, r.BatchSize)
		if purgeErr != nil {
			errs = append(errs, fmt.Errorf("outbox: purge dead events: %w", purgeErr))
		}
	}
	return published, dead, errors.Join(errs...)
}
