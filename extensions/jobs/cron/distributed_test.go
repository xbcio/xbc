package cron

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

func distributedConfig(config *Config) {
	config.Distributed.Enabled = true
	config.Distributed.TTL = 120 * time.Millisecond
	config.Distributed.RenewInterval = 20 * time.Millisecond
	config.RunImmediately = true
}

func TestDistributedTwoReplicasExecuteOnlyOnce(t *testing.T) {
	locker := newFakeLocker()
	var executions atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	job := &funcJob{name: "singleton", spec: "@hourly", run: func(ctx context.Context) error {
		executions.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}

	hostA := newTestHost()
	pluginA, ctxA := initTestPlugin(t, hostA, distributedConfig, locker, job)
	if err := pluginA.start(ctxA); err != nil {
		t.Fatal(err)
	}
	hostA.openTraffic()
	awaitSignal(t, started, "first replica execution")

	hostB := newTestHost()
	pluginB, ctxB := initTestPlugin(t, hostB, distributedConfig, locker, job)
	if err := pluginB.start(ctxB); err != nil {
		t.Fatal(err)
	}
	hostB.openTraffic()
	awaitSignal(t, locker.attempts, "contending lease attempt")
	if got := executions.Load(); got != 1 {
		t.Fatalf("distributed executions = %d, want 1", got)
	}

	close(release)
	stopTestPlugin(t, pluginA, hostA)
	stopTestPlugin(t, pluginB, hostB)
}

// TestTheSchedulerLockNamesNoClaimant pins a decision that is otherwise only a
// comment at the call site.
//
// lease.Locker lets an acquirer publish who it is, and placement uses that to
// make a held slot traceable to a process. cron deliberately does not: a cron
// replica has no process identity to offer -- plugin.Context.Instance is this
// Plugin's instance name, identical in every replica -- and publishing a name
// every replica shares would fill the stored value with a word that
// distinguishes nobody, while looking exactly like an answer. The lock is still
// owner-safe, because the token is unique whether or not a claimant was named;
// the only thing given up is reading the holder back out of the store.
//
// If cron is ever given a real process identity, this test is the one to change,
// and changing it is meant to be a decision rather than an accident.
func TestTheSchedulerLockNamesNoClaimant(t *testing.T) {
	locker := newFakeLocker()
	ran := make(chan struct{}, 1)
	job := &funcJob{name: "nightly", spec: "@hourly", run: func(context.Context) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	}}

	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, locker, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, ran, "the distributed job to run")
	stopTestPlugin(t, p, host)

	claimants := locker.recordedClaimants()
	if len(claimants) == 0 {
		t.Fatal("no acquisition was recorded, so this test proves nothing")
	}
	for index, claimant := range claimants {
		if claimant != "" {
			t.Fatalf("acquisition %d named claimant %q; the scheduler has no process identity to publish", index, claimant)
		}
	}
}

func TestLongDistributedJobRenewsLease(t *testing.T) {
	locker := newFakeLocker()
	started := make(chan struct{})
	finish := make(chan struct{})
	job := &funcJob{name: "renewed", spec: "@hourly", run: func(ctx context.Context) error {
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return nil
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, locker, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	if submitted, critical := host.counts(); submitted != 2 || critical != 2 {
		t.Fatalf("distributed managed tasks = %d/%d, want 2/2", submitted, critical)
	}
	host.openTraffic()
	awaitSignal(t, started, "distributed job start")
	awaitSignal(t, locker.renewed, "initial ownership confirmation")
	awaitSignal(t, locker.renewed, "periodic lease renewal")
	close(finish)
	awaitSignal(t, locker.released, "lease release")
	stopTestPlugin(t, p, host)
}

func TestStopCancelsJobAndReleasesHeldLease(t *testing.T) {
	locker := newFakeLocker()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	job := &funcJob{name: "shutdown", spec: "@hourly", run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, locker, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, started, "distributed job start")
	stopTestPlugin(t, p, host)
	awaitSignal(t, cancelled, "distributed job cancellation")
	awaitSignal(t, locker.released, "held lease release")

	locker.mu.Lock()
	held := len(locker.held)
	locker.mu.Unlock()
	if held != 0 {
		t.Fatalf("held leases after Stop = %d, want 0", held)
	}
}

func TestStopDoesNotReleaseLeaseWhileJobIgnoresCancellation(t *testing.T) {
	locker := newFakeLocker()
	started := make(chan struct{})
	finish := make(chan struct{})
	job := &funcJob{name: "stubborn", spec: "@hourly", run: func(context.Context) error {
		close(started)
		<-finish
		return nil
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, locker, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, started, "stubborn distributed job")

	stopContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.stop(stopContext); err == nil {
		t.Fatal("stop error = nil, want deadline")
	}
	assertNoSignal(t, locker.released, "lease release while job still runs")
	close(finish)
	awaitSignal(t, locker.released, "lease release after job return")
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("retry stop error = %v", err)
	}
	host.close()
}

func TestStopWaitsForInFlightAcquireAndReleasesCommittedLease(t *testing.T) {
	inner := newFakeLocker()
	locker := &blockingAcquireLocker{
		inner:     inner,
		entered:   make(chan struct{}, 1),
		cancelled: make(chan struct{}, 1),
		proceed:   make(chan struct{}),
	}
	var executions atomic.Int32
	job := &funcJob{name: "acquiring", spec: "@hourly", run: func(context.Context) error {
		executions.Add(1)
		return nil
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, locker, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, locker.entered, "lease acquisition start")

	stopContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.stop(stopContext); err == nil {
		t.Fatal("stop error = nil, want blocked acquisition deadline")
	}
	awaitSignal(t, locker.cancelled, "in-flight acquisition cancellation")
	close(locker.proceed)
	awaitSignal(t, inner.released, "committed lease release")
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("retry stop error = %v", err)
	}
	host.close()
	if executions.Load() != 0 {
		t.Fatalf("job executions during shutdown = %d, want 0", executions.Load())
	}
}

type blockingAcquireLocker struct {
	inner     *fakeLocker
	entered   chan struct{}
	cancelled chan struct{}
	proceed   chan struct{}
}

func (l *blockingAcquireLocker) TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
	l.entered <- struct{}{}
	<-ctx.Done()
	l.cancelled <- struct{}{}
	<-l.proceed
	// A remote SET NX may commit concurrently with local cancellation.
	return l.inner.TryAcquire(context.Background(), key, claimant, ttl)
}

func TestDistributedPartialAdmissionCancelsAcceptedRenewalManager(t *testing.T) {
	job := &funcJob{name: "partial", spec: "@hourly", run: func(context.Context) error { return nil }}
	host := newTestHost()
	host.setAdmissionLimit(1)
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, newFakeLocker(), job)
	if err := p.start(runtimeContext); err == nil {
		t.Fatal("start error = nil")
	}
	if submitted, critical := host.counts(); submitted != 1 || critical != 1 {
		t.Fatalf("accepted tasks = %d/%d, want 1/1", submitted, critical)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop after partial admission error = %v", err)
	}
	host.close()
}
