package websocket

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderwebsocket "github.com/coder/websocket"
)

// holdServe reports when the handler starts and when it returns, and blocks in
// a single Read for the life of the connection otherwise.
type holdServe struct {
	started  chan struct{}
	returned chan error
}

func newHoldServe() *holdServe {
	return &holdServe{started: make(chan struct{}, 1), returned: make(chan error, 1)}
}

func (h *holdServe) serve(ctx context.Context, conn *Conn) error {
	h.started <- struct{}{}
	_, _, err := conn.Read(ctx)
	h.returned <- err
	return nil
}

func TestDrainStartsGoingAwayAndRefusesNewUpgrades(t *testing.T) {
	hold := newHoldServe()
	app := startTestApp(t, Options{}, hold.serve, 0)

	conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	drained := make(chan error, 1)
	go func() { drained <- app.plugin.Drain(context.Background()) }()
	waitFor(t, 5*time.Second, "the plugin to start draining", app.plugin.isDraining)

	_, response, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err == nil {
		t.Fatal("an upgrade during drain succeeded")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade during drain response = %v, want status %d", response, http.StatusServiceUnavailable)
	}
	body := readAll(t, response)
	if !strings.Contains(string(body), `"code":"service_unavailable"`) {
		t.Fatalf("upgrade during drain body = %q, want service_unavailable", body)
	}
	if strings.Contains(string(body), `"code":"server_overloaded"`) {
		t.Fatalf("upgrade during drain body = %q, want a shutdown refusal, not an admission refusal", body)
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); coderwebsocket.CloseStatus(err) != coderwebsocket.StatusGoingAway {
		t.Fatalf("first read during drain = %v, want a going-away close", err)
	}
	select {
	case <-hold.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after the going-away close")
	}
	if err := <-drained; err != nil {
		t.Fatalf("Drain() error = %v, want nil", err)
	}
	if !app.plugin.registryEmpty() {
		t.Fatal("the registry still holds a connection after Drain returned")
	}
}

func TestDrainDeadlineLeavesTheSocketForStop(t *testing.T) {
	hold := newHoldServe()
	app := startTestApp(t, Options{}, hold.serve, 0)

	conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	// The client never reads, so it never answers the close handshake the
	// server starts. The drain expires while the handler is still running.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer drainCancel()
	if err := app.plugin.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() error = %v, want context.DeadlineExceeded", err)
	}
	select {
	case err := <-hold.returned:
		t.Fatalf("the handler returned during an expired drain: %v", err)
	default:
	}

	// Drain ends the wait, not the transport: the client can still write. The
	// frame may be consumed by the peer's close handshake, which is why the
	// frame's delivery is not asserted; what matters here is that the
	// transport is open and the handler is still serving it.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer writeCancel()
	if err := conn.Write(writeCtx, coderwebsocket.MessageText, []byte("still here")); err != nil {
		t.Fatalf("write after an expired drain = %v, want the socket left open", err)
	}
	select {
	case err := <-hold.returned:
		t.Fatalf("the handler returned while the socket was still open: %v", err)
	default:
	}

	// Stop owns the force-close. It waits for the in-progress close handshake
	// to give up, then the handler's read unblocks and reports completion.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := app.plugin.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() after an expired Drain error = %v", err)
	}
	select {
	case err := <-hold.returned:
		if err == nil {
			t.Fatal("the handler's read returned nil after Stop force-closed the connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after Stop")
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("the client read succeeded after Stop")
	}
}

func TestStopWithoutDrainForceClosesConnections(t *testing.T) {
	hold := newHoldServe()
	app := startTestApp(t, Options{}, hold.serve, 0)

	conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := app.plugin.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-hold.returned:
		if err == nil {
			t.Fatal("the handler's read returned nil after Stop force-closed the connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after Stop")
	}
	if !app.plugin.registryEmpty() {
		t.Fatal("the registry still holds a connection after Stop returned")
	}
	// Stop is idempotent: a second call, after every connection is gone, is a
	// no-op.
	if err := app.plugin.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("the client read succeeded after Stop")
	}
}

func TestLifecycleBeforeStartAndWithoutConnections(t *testing.T) {
	instance := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := instance.Drain(ctx); err != nil {
		t.Fatalf("Drain() before start error = %v", err)
	}
	if err := instance.Stop(ctx); err != nil {
		t.Fatalf("Stop() before start error = %v", err)
	}
	if err := instance.Stop(ctx); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if err := instance.Drain(ctx); err != nil {
		t.Fatalf("Drain() after Stop error = %v", err)
	}
	if !instance.registryEmpty() {
		t.Fatal("the registry is not empty without connections")
	}
}

func TestConcurrentDialCloseDrainAndStop(t *testing.T) {
	var started, returned atomic.Int64
	serve := func(ctx context.Context, conn *Conn) error {
		started.Add(1)
		defer returned.Add(1)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return nil
			}
		}
	}
	app := startTestApp(t, Options{}, serve, 0)

	const dialers = 6
	var wg sync.WaitGroup
	begin := make(chan struct{})

	for i := 0; i < dialers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-begin
			for j := 0; j < 3; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				conn, _, err := app.dialContext(ctx, "/ws", coderwebsocket.DialOptions{})
				if err == nil {
					// The handshake may have succeeded just before Stop began;
					// both the write and the close may then fail, which is the
					// condition this test exercises rather than an error.
					if err := conn.Write(ctx, coderwebsocket.MessageText, []byte("concurrent")); err != nil {
						_ = conn.CloseNow()
					} else {
						_ = conn.Close(coderwebsocket.StatusNormalClosure, "done")
					}
				}
				cancel()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-begin
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_ = app.plugin.Drain(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-begin
		time.Sleep(20 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := app.plugin.Stop(ctx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	close(begin)
	wg.Wait()

	// Handlers may still be unwinding when Stop returns; wait until the
	// started and returned counts are equal and stay equal.
	waitFor(t, 10*time.Second, "every handler to return", func() bool {
		firstStarted, firstReturned := started.Load(), returned.Load()
		if firstStarted != firstReturned {
			return false
		}
		time.Sleep(5 * time.Millisecond)
		return started.Load() == firstStarted && returned.Load() == firstReturned
	})
	if !app.plugin.registryEmpty() {
		t.Fatal("the registry still holds a connection after every handler returned")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := app.plugin.Stop(stopCtx); err != nil {
		t.Fatalf("final Stop() error = %v", err)
	}
}

// TestDrainWaitsForTheHandlerToFinishClosing pins the defer order in Handler:
// the socket is closed before the registry publishes the handler's completion.
// The handler returns while the drain's close handshake is still waiting for
// the client, so its deferred close blocks; Drain must not report a finished
// shutdown until that close returns.
func TestDrainWaitsForTheHandlerToFinishClosing(t *testing.T) {
	serving := make(chan struct{})
	release := make(chan struct{})
	serve := func(context.Context, *Conn) error {
		close(serving)
		<-release
		return nil
	}
	app := startTestApp(t, Options{}, serve, 0)

	conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	select {
	case <-serving:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started serving")
	}

	drained := make(chan error, 1)
	go func() { drained <- app.plugin.Drain(context.Background()) }()
	waitFor(t, 5*time.Second, "the plugin to start draining", app.plugin.isDraining)
	// Drain sets the flag before it spawns the per-connection close goroutine.
	// Give that goroutine a moment to enter the handshake: the assertion below
	// is about a handler that returns while its close is still in progress, not
	// about a handler that has not returned yet.
	time.Sleep(20 * time.Millisecond)

	close(release)
	// The handler returns and blocks in its deferred CloseNow, which waits
	// behind the drain's close handshake. That handshake cannot finish until
	// the client answers, so Drain has nothing to report yet.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-drained:
		t.Fatalf("Drain() returned before the handler finished closing the connection: %v", err)
	default:
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); coderwebsocket.CloseStatus(err) != coderwebsocket.StatusGoingAway {
		t.Fatalf("Read() during drain = %v, want a going-away close", err)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain() did not return after the close handshake completed")
	}
	if !app.plugin.registryEmpty() {
		t.Fatal("the registry still holds a connection after Drain returned")
	}
}

// TestStopReturnsItsContextErrorWhenHandlersOutlastIt pins Stop's bounded wait:
// each force-close runs on its own goroutine, so one connection stuck in an
// in-progress close handshake cannot make Stop outlast its context.
func TestStopReturnsItsContextErrorWhenHandlersOutlastIt(t *testing.T) {
	hold := newHoldServe()
	app := startTestApp(t, Options{}, hold.serve, 0)

	conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	// The client never reads, so the drain's close handshake cannot finish and
	// its budget expires while the handler is still blocked in Read.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer drainCancel()
	if err := app.plugin.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain() error = %v, want context.DeadlineExceeded", err)
	}

	// Stop's context bounds its wait for the handlers, not the library's wait
	// for the in-progress close handshake. A synchronous force-close would
	// block for the handshake timeout (seconds) and ignore the 100ms budget.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stopCancel()
	started := time.Now()
	stopErr := app.plugin.Stop(stopCtx)
	elapsed := time.Since(started)
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want context.DeadlineExceeded", stopErr)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Stop() took %v with a 100ms context, want the wait bounded by the context", elapsed)
	}

	// Answer the pending close so the handler and the force-close goroutine
	// unwind before the registry is checked.
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("the client read succeeded after Stop began")
	}
	waitFor(t, 10*time.Second, "the registry to empty after the handlers returned", app.plugin.registryEmpty)
}

// TestServeReturningClosesTheConnection pins the deferred force-close: a serve
// function that returns leaves no open socket behind, even when nobody called
// Drain or Stop yet.
func TestServeReturningClosesTheConnection(t *testing.T) {
	app := startTestApp(t, Options{}, func(context.Context, *Conn) error { return nil }, 0)

	conn, _, err := app.dial(t, "/ws", coderwebsocket.DialOptions{})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("Read() succeeded after the handler returned")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Read() did not observe the closed connection: %v", err)
	}
	waitFor(t, 5*time.Second, "the registry to empty", app.plugin.registryEmpty)
}

// TestRegisterRefusesAfterShutdownBegins pins the single-lock decision that
// makes a late handshake harmless: beginShutdown sets the flag under the same
// mutex register checks, so once the shutdown has begun register cannot
// succeed. The handler's post-Accept branch -- a handshake that completes
// between the pre-upgrade check and register -- is the same decision taken at
// runtime; it is not reproduced here because the window cannot be opened
// without a real handshake.
func TestRegisterRefusesAfterShutdownBegins(t *testing.T) {
	instance := New()
	instance.beginShutdown()
	if _, ok := instance.register(new(Conn)); ok {
		t.Fatal("register() admitted a connection after shutdown began")
	}
	if !instance.registryEmpty() {
		t.Fatal("the refused connection was left in the registry")
	}
}
