package placement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

// ── a store a test can hold a renewal inside ───────────────────────────────

// gatedRenewLocker is a lease store whose renewals reach the store and then
// block until the test lets them go.
//
// It exists because the claim made by the readiness probe and by Stats -- that
// neither waits for an in-flight renewal -- is only observable against a
// renewal that is genuinely slow. The acquisition path is unaffected, so a test
// can win a slot and then decide when the store answers.
type gatedRenewLocker struct {
	inner *memoryLocker

	// entered is signalled once when a renewal has reached the store call.
	entered chan struct{}
	// release is closed by the test to let every blocked renewal finish.
	release chan struct{}
}

func newGatedRenewLocker() *gatedRenewLocker {
	return &gatedRenewLocker{
		inner:   newMemoryLocker(),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (l *gatedRenewLocker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (lease.Lease, bool, error) {
	won, acquired, err := l.inner.TryAcquire(ctx, key, ttl)
	if err != nil || !acquired {
		return nil, false, err
	}
	return &gatedRenewLease{inner: won, gate: l}, true, nil
}

// awaitRenewal blocks until a renewal is inside the store, so a test knows the
// read it is about to make is racing a call that cannot return.
func (l *gatedRenewLocker) awaitRenewal(t *testing.T) {
	t.Helper()
	select {
	case <-l.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no renewal reached the store")
	}
}

// releaseRenewal lets every renewal that is waiting in the store finish.
func (l *gatedRenewLocker) releaseRenewal() { close(l.release) }

type gatedRenewLease struct {
	inner lease.Lease
	gate  *gatedRenewLocker
}

func (l *gatedRenewLease) Key() string   { return l.inner.Key() }
func (l *gatedRenewLease) Owner() string { return l.inner.Owner() }

func (l *gatedRenewLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	select {
	case l.gate.entered <- struct{}{}:
	default:
	}
	<-l.gate.release
	return l.inner.Renew(ctx, ttl)
}

func (l *gatedRenewLease) Release(ctx context.Context) (bool, error) {
	return l.inner.Release(ctx)
}

// assertReturnsPromptly fails the test if call has not returned inside the
// budget. The budget is a fraction of a second rather than the store's own
// timeout because of the shape of the defect: a read that queues behind a
// blocked store call does not return slowly, it does not return at all.
func assertReturnsPromptly(t *testing.T, description string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("%s waited on an in-flight renewal; a read must not inherit the lease store's latency", description)
	}
}

// ── reads must not inherit store latency ───────────────────────────────────

// TestAReadinessProbeAndStatsDoNotWaitForASlowRenewal is the defect this file
// exists for.
//
// A renewal holds its slot's mutex across the remote call, which is what orders
// it against a release. Anything that only wants to *read* the slot must not
// queue behind that mutex: a readiness probe or a metrics scrape that did would
// inherit the lease store's latency, report a slow store as this process being
// unready, and add load to the very component whose availability is in
// question. The assertion is the promptness of both reads while the renewal is
// provably still inside the store.
func TestAReadinessProbeAndStatsDoNotWaitForASlowRenewal(t *testing.T) {
	store := newGatedRenewLocker()
	// The interval is long enough that no loop of this test's own runs: the
	// renewal below is driven directly, so the only renewal in the store is the
	// one the test holds.
	value := mustNew(t, store, WithRenewInterval(time.Hour))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	checks := newHealthProbe(value).HealthChecks()
	require.Len(t, checks, 1)

	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		value.renewHeld(context.Background())
	}()
	store.awaitRenewal(t)

	assertReturnsPromptly(t, "the readiness probe", func() error {
		return checks[0].Checker.Check(context.Background())
	})
	assertReturnsPromptly(t, "Stats", func() error {
		_ = value.Stats()
		return nil
	})

	// Everything still finishes cleanly once the store answers.
	store.releaseRenewal()
	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("the renewal never returned after the store was released")
	}
	require.NoError(t, checks[0].Checker.Check(context.Background()), "a confirmed renewal leaves the probe up")
}

// TestTheReadinessProbeAndStatsMakeNoStoreCall pins the other half of the same
// contract, and the reason the latency above is not paid for elsewhere: both
// surfaces report what this process already knows. A probe that re-derived its
// answer from the store would turn a diagnostics surface into load on the
// component whose availability is in question.
func TestTheReadinessProbeAndStatsMakeNoStoreCall(t *testing.T) {
	recorder := newRecordingLocker(newMemoryLocker())
	value := mustNew(t, recorder)
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	checks := newHealthProbe(value).HealthChecks()
	require.Len(t, checks, 1)

	calls := recorder.storeCalls()
	require.NotZero(t, calls, "the decision itself reached the store, so the assertion below is about the reads")

	for round := 0; round < 3; round++ {
		require.NoError(t, checks[0].Checker.Check(context.Background()))
		_ = value.Stats()
	}
	assert.Equal(t, calls, recorder.storeCalls(), "a readiness probe and a metrics snapshot read in-memory state and must not touch the lease store")
}

// storeCalls counts every call that reached the store, so a test can prove a
// read-only surface made none.
func (l *recordingLocker) storeCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// ── stopping while a renewal is in flight ──────────────────────────────────

// TestStopWaitsForAnInFlightRenewalAndThenReturnsPromptly covers stop racing a
// live renewal round.
//
// stop is the backstop release, and it waits on the managed-task group, so a
// renewal that is inside the store when the process is asked to stop is the
// round it has to wait for. Two things are pinned at once: stop does not run
// its release past a renewal that could still write the slot back, and it
// returns as soon as that round ends rather than waiting for the loop's next
// tick.
func TestStopWaitsForAnInFlightRenewalAndThenReturnsPromptly(t *testing.T) {
	store := newGatedRenewLocker()
	value := mustNew(t, store, WithRenewInterval(20*time.Millisecond))
	_, err := value.Resolve(workloadRequest(ordinary("sast", 1)))
	require.NoError(t, err)

	host := newTestHost()
	host.openGate()
	require.NoError(t, value.start(host.context()))
	store.awaitRenewal(t)

	stopped := make(chan error, 1)
	go func() { stopped <- value.stop(context.Background()) }()

	// The window is several renewal intervals wide on purpose. This asserts the
	// absence of a return, and the only way to observe an absence is to wait
	// longer than the work involved would take; the round being held is
	// guaranteed not to be able to finish, so a return here is a defect rather
	// than a race.
	select {
	case err := <-stopped:
		t.Fatalf("stop returned while a renewal was still inside the store: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	store.releaseRenewal()

	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the in-flight renewal completed")
	}
	assert.False(t, store.inner.holds("xbc:workload:sast:0"), "the backstop still gives the slot back on the way out")
	host.awaitTasks(t)
}
