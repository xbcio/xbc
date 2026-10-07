package auditlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xbcio/xbc/log"
)

// ErrQueueFull is returned for the explicit drop_newest overflow policy. The
// middleware also logs it, so an audit loss is never silent.
var ErrQueueFull = errors.New("auditlog: async queue is full; newest event dropped")

// ErrStopped reports that shutdown has closed queue admission.
var ErrStopped = errors.New("auditlog: async dispatcher is stopped")

type asyncDispatcher struct {
	sink        Sink
	logger      log.Logger
	overflow    string
	sinkTimeout time.Duration
	queue       chan Event
	reject      chan struct{}
	drain       chan struct{}
	done        chan struct{}

	admission sync.RWMutex
	stopped   bool
	senders   sync.WaitGroup
	stopOnce  sync.Once
	drainOnce sync.Once
	// syncDrain serializes drainWithoutWorker, so a Drain and a Stop that
	// overlap never write to the sink from two goroutines at once.
	syncDrain sync.Mutex

	cancelMu sync.Mutex
	cancel   context.CancelFunc
	canceled bool
	dropped  atomic.Uint64
}

func newAsyncDispatcher(sink Sink, logger log.Logger, size int, overflow string, sinkTimeout ...time.Duration) *asyncDispatcher {
	timeout := 2 * time.Second
	if len(sinkTimeout) != 0 && sinkTimeout[0] > 0 {
		timeout = sinkTimeout[0]
	}
	return &asyncDispatcher{
		sink: sink, logger: logger, overflow: overflow, sinkTimeout: timeout,
		queue: make(chan Event, size), reject: make(chan struct{}), drain: make(chan struct{}), done: make(chan struct{}),
	}
}

func (d *asyncDispatcher) run(ctx context.Context) {
	workerCtx, cancel := context.WithCancel(ctx)
	d.cancelMu.Lock()
	d.cancel = cancel
	canceled := d.canceled
	d.cancelMu.Unlock()
	if canceled {
		cancel()
	}
	defer cancel()
	defer close(d.done)
	for {
		select {
		case event := <-d.queue:
			d.write(workerCtx, event)
		case <-d.drain:
			for {
				select {
				case event := <-d.queue:
					d.write(workerCtx, event)
				default:
					return
				}
			}
		case <-workerCtx.Done():
			return
		}
	}
}

func (d *asyncDispatcher) write(parent context.Context, event Event) {
	ctx, cancel := context.WithTimeout(parent, d.sinkTimeout)
	defer cancel()
	if err := callSink(d.sink, ctx, event); err != nil {
		d.logger.Error("audit sink write failed", "error", err)
	}
}

func (d *asyncDispatcher) submit(ctx context.Context, event Event) error {
	// Add is protected by admission's read lock; stopAndWait takes the write
	// lock before Wait, so no WaitGroup Add can race the shutdown Wait.
	d.admission.RLock()
	if d.stopped {
		d.admission.RUnlock()
		return ErrStopped
	}
	d.senders.Add(1)
	d.admission.RUnlock()
	defer d.senders.Done()

	if d.overflow == OverflowBlock {
		select {
		case d.queue <- event:
			return nil
		case <-d.reject:
			return ErrStopped
		case <-ctx.Done():
			return fmt.Errorf("auditlog: enqueue: %w", ctx.Err())
		}
	}
	// Bias toward shutdown before attempting the non-blocking send. If both
	// become ready immediately afterwards, stopAndWait still waits for this
	// sender before signaling the worker's drain phase.
	select {
	case <-d.reject:
		return ErrStopped
	default:
	}
	select {
	case d.queue <- event:
		return nil
	case <-d.reject:
		return ErrStopped
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// stopAndWait closes admission and waits, within ctx, for every accepted event
// to reach the sink. abandon decides what an expired ctx does to the events
// still queued: Stop abandons them by cancelling the worker, while Drain only
// stops waiting and leaves the worker writing for the Stop that follows. An
// abandoned worker is left to unwind on its own cancellation instead of being
// awaited, so the Flush that follows runs against a settled sink only when the
// drain finished inside ctx; on a spent deadline it is a best-effort call.
func (d *asyncDispatcher) stopAndWait(ctx context.Context, workerAccepted, abandon bool) error {
	d.stopOnce.Do(func() {
		d.admission.Lock()
		d.stopped = true
		close(d.reject)
		d.admission.Unlock()
	})

	// Every admitted sender either queued its event or observed reject before
	// drain is signaled, so the worker cannot miss a late queue write.
	sendersDone := make(chan struct{})
	go func() {
		d.senders.Wait()
		close(sendersDone)
	}()
	select {
	case <-sendersDone:
	case <-ctx.Done():
		if abandon {
			d.abandonWorker(ctx, workerAccepted)
		}
		return fmt.Errorf("auditlog: stop queue admission: %w", ctx.Err())
	}

	if !workerAccepted {
		return d.drainWithoutWorker(ctx)
	}
	d.drainOnce.Do(func() { close(d.drain) })
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		if abandon {
			d.abandonWorker(ctx, true)
		}
		return fmt.Errorf("auditlog: drain async queue: %w", ctx.Err())
	}
}

func (d *asyncDispatcher) drainWithoutWorker(ctx context.Context) error {
	d.syncDrain.Lock()
	defer d.syncDrain.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("auditlog: drain async queue: %w", err)
		}
		select {
		case event := <-d.queue:
			d.write(ctx, event)
		default:
			return nil
		}
	}
}

// abandonWorker cancels the worker and waits for its exit only within ctx --
// the deadline that abandoned the queue and is already spent by the time this
// runs. The worker is told to stop rather than awaited to completion: the sink
// contract requires every write to honor context cancellation, so a compliant
// sink's worker exits at once, while one that ignores cancellation cannot hold
// Stop past the caller's deadline. accepted is false for a dispatcher whose run
// was never submitted, which has no worker to wait for.
func (d *asyncDispatcher) abandonWorker(ctx context.Context, accepted bool) {
	d.cancelWorker()
	if !accepted {
		return
	}
	select {
	case <-d.done:
	case <-ctx.Done():
	}
}

// cancelWorker cancels the worker's context. It is safe whether or not run has
// installed its cancel func yet: run consults canceled when it starts, so a
// Stop that abandons before the worker is scheduled still ends it.
func (d *asyncDispatcher) cancelWorker() {
	d.cancelMu.Lock()
	d.canceled = true
	cancel := d.cancel
	d.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (d *asyncDispatcher) droppedCount() uint64 { return d.dropped.Load() }
