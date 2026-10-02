package cron

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDrainLetsTheRunningInvocationFinishWithALiveContext pins the graceful
// half of shutdown: drain stops scheduling, waits for the invocation in
// flight, and never cancels the context that invocation runs under.
func TestDrainLetsTheRunningInvocationFinishWithALiveContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cancelled := make(chan error, 1)
	job := &funcJob{name: "graceful", spec: "@hourly", run: func(ctx context.Context) error {
		close(started)
		<-release
		cancelled <- ctx.Err()
		return nil
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, func(config *Config) { config.RunImmediately = true }, nil, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, started, "graceful job start")

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline while the job is running", err)
	}
	close(release)
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() after the job finished error = %v", err)
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("drain cancelled the running invocation: %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() after drain error = %v", err)
	}
	host.close()
}

// TestDrainKeepsRenewingTheLeaseOfADistributedJobInFlight covers the reason
// drain must not cancel the run context: a distributed job still holds a
// lease, and its renewal manager has to keep renewing until the job returns,
// after which the lease is released and drain completes.
func TestDrainKeepsRenewingTheLeaseOfADistributedJobInFlight(t *testing.T) {
	locker := newFakeLocker()
	started := make(chan struct{})
	release := make(chan struct{})
	job := &funcJob{name: "leased", spec: "@hourly", run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, distributedConfig, locker, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, started, "leased job start")

	drained := make(chan error, 1)
	go func() { drained <- p.drain(context.Background()) }()
	for range 2 {
		select {
		case <-locker.renewed:
		case <-time.After(time.Second):
			t.Fatal("the lease was not renewed while drain waited for the job")
		}
	}
	assertNoDrain(t, drained)
	close(release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not return after the job finished")
	}
	select {
	case <-locker.released:
	case <-time.After(time.Second):
		t.Fatal("the lease was not released after the job finished")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("stop() after drain error = %v", err)
	}
	host.close()
}

// TestDrainBeforeTrafficOrStartReturnsAndBlocksALaterStart covers the abort
// path: a runner still waiting for the traffic gate exits on drain, and a
// plugin drained before Start refuses to start afterwards.
func TestDrainBeforeTrafficOrStartReturnsAndBlocksALaterStart(t *testing.T) {
	job := &funcJob{name: "never", spec: "@hourly", run: func(context.Context) error { return nil }}

	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, nil, nil, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() with the gate still closed error = %v", err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.close()

	host = newTestHost()
	p, runtimeContext = initTestPlugin(t, host, nil, nil, job)
	if err := p.drain(context.Background()); err != nil {
		t.Fatalf("drain() before Start error = %v", err)
	}
	if err := p.start(runtimeContext); err == nil {
		t.Fatal("start() after drain error = nil")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.close()
}

func assertNoDrain(t *testing.T, drained <-chan error) {
	t.Helper()
	select {
	case err := <-drained:
		t.Fatalf("drain returned while the job was still running: %v", err)
	default:
	}
}
