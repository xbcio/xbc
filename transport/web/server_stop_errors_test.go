package web_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// errServingShutdown and errManagementShutdown are the scripted failures the
// two planes report. They are distinct values so errors.Is can attribute the
// joined result to the plane that produced it.
var (
	errServingShutdown    = errors.New("serving plane shutdown failed")
	errManagementShutdown = errors.New("management plane shutdown failed")
)

// stopEngine is an Engine double whose only observable behavior is shutdown:
// Serve blocks until Shutdown runs or the test cleanup aborts it, and Shutdown
// records the context it received and returns a scripted error. It owns no HTTP
// behavior because these tests assert on the Stop path, never on a dispatched
// request.
type stopEngine struct {
	shutdownErr error

	serving   chan struct{}
	servingOn sync.Once
	stopped   chan struct{}
	stopOnce  sync.Once
	aborted   chan struct{}
	abortOnce sync.Once

	mu          sync.Mutex
	listener    net.Listener
	shutdownCtx context.Context
	shutdown    atomic.Int32
}

func newStopEngine(shutdownErr error) *stopEngine {
	return &stopEngine{
		shutdownErr: shutdownErr,
		serving:     make(chan struct{}),
		stopped:     make(chan struct{}),
		aborted:     make(chan struct{}),
	}
}

func (*stopEngine) Handle(string, string, []web.Handler) {}
func (*stopEngine) Mount(string, string, []web.Handler)  {}
func (*stopEngine) NoRoute([]web.Handler)                {}
func (*stopEngine) NoMethod([]web.Handler)               {}

func (e *stopEngine) Serve(ln net.Listener) error {
	e.mu.Lock()
	e.listener = ln
	e.mu.Unlock()
	e.servingOn.Do(func() { close(e.serving) })
	select {
	case <-e.stopped:
	case <-e.aborted:
	}
	return http.ErrServerClosed
}

func (e *stopEngine) Shutdown(ctx context.Context) error {
	calls := e.shutdown.Add(1)
	e.mu.Lock()
	e.shutdownCtx = ctx
	e.mu.Unlock()
	e.stopOnce.Do(func() {
		e.mu.Lock()
		ln := e.listener
		e.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		close(e.stopped)
	})
	// A second Shutdown is a no-op success, matching http.Server.Shutdown.
	// The test helper's cleanup calls Stop again; only the first call carries
	// the scripted failure.
	if calls > 1 {
		return nil
	}
	return e.shutdownErr
}

func (e *stopEngine) shutdownCalls() int { return int(e.shutdown.Load()) }

// shutdownContext reports the context of the most recent Shutdown call.
func (e *stopEngine) shutdownContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCtx
}

// abortServe releases a Serve call that no Shutdown will ever reach, so a
// regression that skips a plane's shutdown fails the test instead of leaving
// the host's task wait blocked until the test timeout.
func (e *stopEngine) abortServe() {
	e.abortOnce.Do(func() { close(e.aborted) })
}

// scriptedEngineFactory hands out one stopEngine per NewEngine call, in
// construction order with the scripted error for that position. The Server
// builds the serving engine first and the management engine second.
type scriptedEngineFactory struct {
	errs []error

	mu      sync.Mutex
	engines []*stopEngine
}

func (f *scriptedEngineFactory) NewEngine(web.Options) (web.Engine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.engines)
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	engine := newStopEngine(err)
	f.engines = append(f.engines, engine)
	return engine, nil
}

func (f *scriptedEngineFactory) built() []*stopEngine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*stopEngine(nil), f.engines...)
}

// TestStopSettlesBothPlanesWhenShutdownFails pins the two-plane settlement: a
// serving-plane failure must not skip the management shutdown, a management
// failure must still be reported alongside a serving failure, and each failure
// must remain attributable through errors.Is.
//
// The management shutdown counter is the discriminating assertion for the
// baseline defect: today Stop returns right after the serving error, so the
// management engine is never shut down and its error is never observed.
func TestStopSettlesBothPlanesWhenShutdownFails(t *testing.T) {
	tests := []struct {
		name           string
		servingErr     error
		managementErr  error
		wantServing    bool
		wantManagement bool
		repeatStop     bool
	}{
		{
			name:           "serving failure still settles management",
			servingErr:     errServingShutdown,
			wantServing:    true,
			wantManagement: false,
			repeatStop:     true,
		},
		{
			name:           "management failure is reported",
			managementErr:  errManagementShutdown,
			wantServing:    false,
			wantManagement: true,
		},
		{
			name:           "both failures are joined",
			servingErr:     errServingShutdown,
			managementErr:  errManagementShutdown,
			wantServing:    true,
			wantManagement: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := &scriptedEngineFactory{errs: []error{test.servingErr, test.managementErr}}
			cfg := web.DefaultConfig()
			cfg.Addr = "127.0.0.1:0"
			cfg.Management.Addr = "127.0.0.1:0"
			server, ctx, host := newPingServer(t, cfg, serverInputs{factory: factory})
			require.NoError(t, server.Start(ctx))
			require.NoError(t, server.OpenTraffic(ctx))

			engines := factory.built()
			require.Len(t, engines, 2, "the composition must build a serving and a management engine")
			serving, management := engines[0], engines[1]

			// Cleanups run LIFO, so these release a Serve call that the Stop
			// path failed to reach before the server/host cleanup waits on it.
			t.Cleanup(serving.abortServe)
			t.Cleanup(management.abortServe)

			host.releaseTraffic()
			waitForEngineServe(t, serving)
			waitForEngineServe(t, management)

			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := server.Stop(stopCtx)

			assert.Equal(t, 1, serving.shutdownCalls(),
				"the serving engine must be shut down exactly once")
			assert.Equal(t, 1, management.shutdownCalls(),
				"the management engine must be shut down even when the serving plane fails")

			assert.Same(t, stopCtx, serving.shutdownContext(),
				"the serving engine must receive the runtime's remaining deadline")
			assert.Same(t, stopCtx, management.shutdownContext(),
				"the management engine must share the same remaining deadline")

			if test.repeatStop {
				// No once-guard hides the engines on a repeated Stop: each call
				// still reaches both planes, and the doubles' later shutdowns
				// succeed, so the second call reports nil.
				require.NoError(t, server.Stop(stopCtx),
					"a repeated Stop must return nil once both planes already settled")
				assert.Equal(t, 2, serving.shutdownCalls(),
					"a repeated Stop must reach the serving engine again")
				assert.Equal(t, 2, management.shutdownCalls(),
					"a repeated Stop must reach the management engine again")
			}

			assert.Equal(t, test.wantServing, errors.Is(err, errServingShutdown),
				"the serving failure must be attributable in the returned error")
			assert.Equal(t, test.wantManagement, errors.Is(err, errManagementShutdown),
				"the management failure must be attributable in the returned error")

			require.Error(t, err)
			message := err.Error()
			assert.Equal(t, test.wantServing, strings.Contains(message, "xbc: graceful shutdown failed"),
				"the serving plane's wrapper text must appear exactly when the serving plane failed")
			assert.Equal(t, test.wantManagement, strings.Contains(message, "xbc: management HTTP shutdown failed"),
				"the management plane's wrapper text must appear exactly when the management plane failed")
		})
	}
}

// waitForServing blocks until the engine's Serve call has been entered, which
// is what tells the test that Stop will take the served branch.
func waitForEngineServe(t *testing.T, engine *stopEngine) {
	t.Helper()
	select {
	case <-engine.serving:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine never began serving after the traffic gate was released")
	}
}
