package lease_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

// stubLease is the minimal Lease the scripted lockers below hand back. Only Key
// and Owner matter to Acquire; the renewal and release calls are never reached
// by the helper under test.
type stubLease struct{ key, owner string }

func (l stubLease) Key() string   { return l.key }
func (l stubLease) Owner() string { return l.owner }

func (l stubLease) Renew(context.Context, time.Duration) (bool, error) { return true, nil }

func (l stubLease) Release(context.Context) (bool, error) { return true, nil }

// scriptedLocker answers TryAcquire from a script: contend for failures calls,
// then grant, unless alwaysContend keeps it contended forever. err reports a
// backend failure on the first call.
type scriptedLocker struct {
	mu            sync.Mutex
	calls         int
	failures      int
	alwaysContend bool
	err           error
	before        func()
}

func (l *scriptedLocker) TryAcquire(_ context.Context, key, claimant string, _ time.Duration) (lease.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.before != nil {
		l.before()
	}
	if l.err != nil {
		return nil, false, l.err
	}
	if l.alwaysContend {
		return nil, false, nil
	}
	if l.failures > 0 {
		l.failures--
		return nil, false, nil
	}
	return stubLease{key: key, owner: claimant + "/stub"}, true, nil
}

func (l *scriptedLocker) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// singleWinnerLocker grants the key to exactly one caller and reports
// contention to every later one, which is the shape a real store produces.
type singleWinnerLocker struct {
	mu    sync.Mutex
	taken bool
}

func (l *singleWinnerLocker) TryAcquire(_ context.Context, key, claimant string, _ time.Duration) (lease.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.taken {
		return nil, false, nil
	}
	l.taken = true
	return stubLease{key: key, owner: claimant + "/stub"}, true, nil
}

// TestAcquireReturnsImmediatelyOnASuccessfulAttempt pins the uncontended path:
// one call, no wait, the lease the locker granted.
func TestAcquireReturnsImmediatelyOnASuccessfulAttempt(t *testing.T) {
	locker := &scriptedLocker{}

	held, err := lease.Acquire(context.Background(), locker, "locks:task", "worker-1", time.Second)
	mustNoError(t, err, "Acquire failed")
	mustTrue(t, held != nil, "a successful Acquire must return the granted lease")
	if key := held.Key(); key != "locks:task" {
		t.Errorf("Acquire returned key %q, want %q", key, "locks:task")
	}
	if calls := locker.callCount(); calls != 1 {
		t.Errorf("Acquire called the store %d times, want 1", calls)
	}
}

// TestAcquireWaitsThroughContentionAndThenWins pins the blocking behaviour the
// helper exists for: every contended result is retried, and the attempt that
// wins is returned.
func TestAcquireWaitsThroughContentionAndThenWins(t *testing.T) {
	locker := &scriptedLocker{failures: 2}

	held, err := lease.Acquire(context.Background(), locker, "locks:task", "worker-1", time.Second,
		lease.WithRetryInterval(2*time.Millisecond),
		lease.WithMaxAttempts(5),
	)
	mustNoError(t, err, "Acquire failed")
	mustTrue(t, held != nil, "Acquire must return the attempt that won")
	if calls := locker.callCount(); calls != 3 {
		t.Errorf("two contentions must be retried before the winning attempt: the store saw %d calls, want 3", calls)
	}
}

// TestAcquireReportsTheContextThatEndedTheWait pins the cancellation half of
// the contract: the error wraps ctx.Err() so a caller classifies it without
// matching text, and the attempt budget is not what ended the wait.
func TestAcquireReportsTheContextThatEndedTheWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	locker := &scriptedLocker{alwaysContend: true}

	held, err := lease.Acquire(ctx, locker, "locks:task", "worker-1", time.Second,
		lease.WithRetryInterval(5*time.Millisecond),
	)
	if err == nil {
		t.Fatal("Acquire must report the context that ended the wait")
	}
	if held != nil {
		t.Errorf("a failed Acquire must return no lease, got %v", held)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the returned error must wrap the context deadline, got %v", err)
	}
	if errors.Is(err, lease.ErrNotAcquired) {
		t.Errorf("the attempt budget must not be what ended the wait, got %v", err)
	}
	if calls := locker.callCount(); calls < 1 {
		t.Errorf("Acquire made %d calls, want at least one", calls)
	}
}

// TestAcquireReportsExhaustedAttempts pins the policy half: the dedicated
// sentinel lets a caller tell its own attempt budget from a deadline.
func TestAcquireReportsExhaustedAttempts(t *testing.T) {
	locker := &scriptedLocker{alwaysContend: true}

	held, err := lease.Acquire(context.Background(), locker, "locks:task", "worker-1", time.Second,
		lease.WithRetryInterval(time.Millisecond),
		lease.WithMaxAttempts(3),
	)
	if err == nil {
		t.Fatal("Acquire must report the exhausted attempt budget")
	}
	if held != nil {
		t.Errorf("a failed Acquire must return no lease, got %v", held)
	}
	if !errors.Is(err, lease.ErrNotAcquired) {
		t.Errorf("the returned error must wrap ErrNotAcquired, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an exhausted budget must not be reported as a deadline, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("an exhausted budget must not be reported as cancellation, got %v", err)
	}
	if calls := locker.callCount(); calls != 3 {
		t.Errorf("WithMaxAttempts counts the first call: the store saw %d calls, want 3", calls)
	}
}

// TestAcquireDoesNotCallTheStoreOnAnEndedContext keeps an already-cancelled
// caller from making a pointless attempt: the context is checked before every
// try, including the first.
func TestAcquireDoesNotCallTheStoreOnAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	locker := &scriptedLocker{}

	held, err := lease.Acquire(ctx, locker, "locks:task", "worker-1", time.Second)
	if err == nil {
		t.Fatal("Acquire must report the already-cancelled context")
	}
	if held != nil {
		t.Errorf("a failed Acquire must return no lease, got %v", held)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the returned error must wrap context.Canceled, got %v", err)
	}
	if calls := locker.callCount(); calls != 0 {
		t.Errorf("the store saw %d calls on an ended context, want 0", calls)
	}
}

// TestAcquireReturnsABackendFailureWithoutRetrying mirrors the redsync call
// this helper replaces: only contention is retried, because a store failure is
// not evidence that another attempt would be answered differently.
func TestAcquireReturnsABackendFailureWithoutRetrying(t *testing.T) {
	backendErr := errors.New("store unavailable")
	locker := &scriptedLocker{err: backendErr}

	held, err := lease.Acquire(context.Background(), locker, "locks:task", "worker-1", time.Second,
		lease.WithRetryInterval(time.Millisecond),
		lease.WithMaxAttempts(5),
	)
	if err == nil {
		t.Fatal("Acquire must report the backend failure")
	}
	if held != nil {
		t.Errorf("a failed Acquire must return no lease, got %v", held)
	}
	if !errors.Is(err, backendErr) {
		t.Errorf("the returned error must wrap the backend failure, got %v", err)
	}
	if calls := locker.callCount(); calls != 1 {
		t.Errorf("a backend failure must not be retried: the store saw %d calls, want 1", calls)
	}
}

// TestAcquireReturnsALeaseCommittedAsTheContextEnded pins the race lease.Locker
// documents: an acquisition that committed remotely just as the context ended
// is a lease the caller must release, so it is returned rather than discarded.
func TestAcquireReturnsALeaseCommittedAsTheContextEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	locker := &scriptedLocker{before: cancel}

	held, err := lease.Acquire(ctx, locker, "locks:task", "worker-1", time.Second)
	mustNoError(t, err, "a committed acquisition must be handed to the caller even though the context ended")
	mustTrue(t, held != nil, "a committed acquisition must return its lease")
}

func TestAcquireRejectsInvalidConfiguration(t *testing.T) {
	locker := &scriptedLocker{}
	ctx := context.Background()

	if _, err := lease.Acquire(ctx, nil, "locks:task", "", time.Second); err == nil {
		t.Fatal("a nil locker must be rejected")
	}

	cases := map[string]lease.AcquireOption{
		"zero retry interval":     lease.WithRetryInterval(0),
		"negative retry interval": lease.WithRetryInterval(-time.Second),
		"negative attempts":       lease.WithMaxAttempts(-1),
		"nil option":              nil,
	}
	for name, option := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := lease.Acquire(ctx, locker, "locks:task", "", time.Second, option); err == nil {
				t.Fatalf("Acquire accepted %s", name)
			}
		})
	}
	if calls := locker.callCount(); calls != 0 {
		t.Errorf("no invalid configuration may reach the store: the store saw %d calls, want 0", calls)
	}
}

// TestAcquireGrantsExactlyOneWinnerUnderConcurrentContention is the mutual
// exclusion assertion: every caller polls the same key, the store grants one,
// and the others end on their attempt budget rather than on an error that
// hides a second owner.
func TestAcquireGrantsExactlyOneWinnerUnderConcurrentContention(t *testing.T) {
	locker := &singleWinnerLocker{}
	const callers = 8

	results := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := lease.Acquire(context.Background(), locker, "locks:single", "", time.Minute,
				lease.WithRetryInterval(time.Millisecond),
				lease.WithMaxAttempts(20),
			)
			results <- err
		}()
	}
	wait.Wait()
	close(results)

	winners, exhausted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, lease.ErrNotAcquired):
			exhausted++
		default:
			t.Fatalf("loser ended with %v, want ErrNotAcquired", err)
		}
	}
	if winners != 1 {
		t.Errorf("exactly one caller must win the key, got %d", winners)
	}
	if exhausted != callers-1 {
		t.Errorf("the other %d callers must end on their attempt budget, got %d", callers-1, exhausted)
	}
}
