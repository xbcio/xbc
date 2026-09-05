package outbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var errLeaseUncertain = errors.New("outbox: event lease could not be maintained")

type worker struct {
	store       Store
	publisher   Publisher
	cfg         WorkerConfig
	ownerPrefix string
	ownerSeq    atomic.Uint64

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	doneOnce sync.Once

	mu        sync.Mutex
	scheduled bool
	running   bool
	stopping  bool
}

func newWorker(store Store, publisher Publisher, cfg WorkerConfig) (*worker, error) {
	if store == nil {
		return nil, errors.New("outbox: worker requires a store")
	}
	if publisher == nil {
		return nil, errors.New("outbox: worker requires a publisher")
	}
	ownerPrefix, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	return &worker{
		store: store, publisher: publisher, cfg: cfg, ownerPrefix: ownerPrefix,
		stop: make(chan struct{}), done: make(chan struct{}),
	}, nil
}

// schedule freezes the worker for one managed run without starting any
// polling. OpenTraffic calls it immediately before submitting run.
func (w *worker) schedule() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.stopping:
		return ErrClosed
	case w.scheduled:
		return errors.New("outbox: worker was already scheduled")
	default:
		w.scheduled = true
		return nil
	}
}

func (w *worker) run(parent context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	if !w.scheduled || w.stopping {
		w.mu.Unlock()
		w.doneOnce.Do(func() { close(w.done) })
		return
	}
	w.running = true
	w.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	defer func() {
		cancel()
		w.doneOnce.Do(func() { close(w.done) })
	}()

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	sem := make(chan struct{}, w.cfg.Concurrency)
	fatal := make(chan struct{}, 1)
	var active sync.WaitGroup

	waitAndReturn := func() {
		cancel()
		active.Wait()
	}
	for {
		select {
		case <-ctx.Done():
			waitAndReturn()
			return
		case <-w.stop:
			active.Wait()
			return
		case <-fatal:
			waitAndReturn()
			return
		default:
		}

		available := cap(sem) - len(sem)
		if available > 0 {
			limit := min(w.cfg.BatchSize, available)
			owner := w.nextOwner()
			dbCtx, dbCancel := context.WithTimeout(ctx, w.cfg.DatabaseTimeout)
			records, err := w.store.Claim(dbCtx, owner, time.Now().UTC(), w.cfg.LeaseDuration, limit, w.cfg.MaxAttempts)
			dbCancel()
			if err == nil {
				if len(records) > limit {
					records = records[:limit]
				}
				for _, record := range records {
					sem <- struct{}{}
					active.Add(1)
					go func(record Record) {
						defer func() {
							if recover() != nil {
								select {
								case fatal <- struct{}{}:
								default:
								}
							}
							<-sem
							active.Done()
						}()
						w.publish(ctx, record)
					}(record)
				}
			}
		}

		select {
		case <-ctx.Done():
			waitAndReturn()
			return
		case <-w.stop:
			active.Wait()
			return
		case <-fatal:
			waitAndReturn()
			return
		case <-ticker.C:
		}
	}
}

func (w *worker) nextOwner() string {
	return fmt.Sprintf("%s-%016x", w.ownerPrefix, w.ownerSeq.Add(1))
}

func (w *worker) publish(runCtx context.Context, record Record) {
	publishCtx, cancelPublish := context.WithTimeout(runCtx, w.cfg.PublishTimeout)
	renewDone := make(chan error, 1)
	go func() {
		renewDone <- w.renew(publishCtx, cancelPublish, record.Event.ID, record.LeaseOwner)
	}()

	err := publishSafely(publishCtx, w.publisher, record.Event.Clone())
	cancelPublish()
	renewErr := <-renewDone
	if err != nil && renewErr != nil {
		err = errors.Join(err, renewErr)
	} else if err == nil && renewErr != nil {
		// Publish completed successfully. We still conditionally persist success;
		// the owner token prevents an old attempt from changing a newer lease.
		err = nil
	}

	dbCtx, dbCancel := context.WithTimeout(runCtx, w.cfg.DatabaseTimeout)
	defer dbCancel()
	if err == nil {
		_, _ = w.store.MarkPublished(dbCtx, record.Event.ID, record.LeaseOwner, time.Now().UTC())
		return
	}
	dead := record.Attempts >= w.cfg.MaxAttempts
	next := time.Now().UTC()
	if !dead {
		next = next.Add(retryDelay(record.Attempts, w.cfg.InitialBackoff, w.cfg.MaxBackoff, w.cfg.Jitter))
	}
	_, _ = w.store.MarkFailed(dbCtx, record.Event.ID, record.LeaseOwner, next, dead, safeError(err))
}

func (w *worker) renew(ctx context.Context, cancelPublish context.CancelFunc, id, owner string) error {
	ticker := time.NewTicker(w.cfg.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			dbCtx, cancel := context.WithTimeout(ctx, w.cfg.DatabaseTimeout)
			owned, err := w.store.Renew(dbCtx, id, owner, time.Now().UTC().Add(w.cfg.LeaseDuration))
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err != nil || !owned {
				cancelPublish()
				if err != nil {
					return fmt.Errorf("%w: %v", errLeaseUncertain, err)
				}
				return errLeaseUncertain
			}
		}
	}
}

func publishSafely(ctx context.Context, publisher Publisher, event Event) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("outbox: publisher panicked")
		}
	}()
	return publisher.Publish(ctx, event)
}

// requestStop prevents another polling cycle and returns the one completion
// signal. It never waits for database or publisher work.
func (w *worker) requestStop() <-chan struct{} {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopping = true
		close(w.stop)
		running := w.running
		w.mu.Unlock()
		// Task admission can close between TasksAccepted and GoCritical. If
		// run never entered, there is no work to drain and Stop must not hang.
		if !running {
			w.doneOnce.Do(func() { close(w.done) })
		}
	})
	return w.done
}

func (w *worker) stopAndWait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := w.requestStop()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func retryDelay(attempt int, initial, maximum time.Duration, jitter float64) time.Duration {
	exponent := max(attempt-1, 0)
	base := float64(initial)
	if exponent < 63 {
		base *= math.Pow(2, float64(exponent))
	} else {
		base = float64(maximum)
	}
	if base > float64(maximum) {
		base = float64(maximum)
	}
	if jitter > 0 {
		var bytes [8]byte
		if _, err := rand.Read(bytes[:]); err == nil {
			unit := float64(binary.BigEndian.Uint64(bytes[:])) / float64(math.MaxUint64)
			base *= 1 - jitter + 2*jitter*unit
		}
	}
	if base < 0 {
		return 0
	}
	if base > float64(maximum) {
		base = float64(maximum)
	}
	return time.Duration(base)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errLeaseUncertain) {
		return "event lease could not be maintained"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "publish deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "publish canceled"
	}
	return truncate(err.Error(), 512)
}
