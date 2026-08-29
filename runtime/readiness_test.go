package runtime

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
)

// This file pins the protocol-agnostic two-phase readiness contract from
// package-layout design §5.1: every Runner.Start in the application must
// complete before any TrafficOpener.OpenTraffic runs, Start itself is
// serial and follows dependency-topological order (not concurrency, not
// name), and a failure in either phase unwinds the whole application without
// ever letting a later plugin open traffic.
//
// Every test builds a small dependency chain out of the node types below,
// wired together with explicit plugin keys so the assembly graph has something real to
// topologically sort -- exactly the mechanism package assembly's own order
// tests (and shutdown_test.go's shutdownUpstream/shutdownDownstream) use.

// --- shared recording plumbing ---------------------------------------------

// readinessRecorder is a shared, lock-protected event log every node in a
// test writes into. A single global order across every plugin instance is
// the only way to observe the cross-plugin barrier in §5.1's first clause
// ("no OpenTraffic before every Start") -- a per-plugin log could not show
// that plugin A's own OpenTraffic happened before plugin C's Start.
type readinessRecorder struct {
	mu     sync.Mutex
	events []string
}

func newReadinessRecorder() *readinessRecorder { return &readinessRecorder{} }

func (r *readinessRecorder) record(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *readinessRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// readinessWithPrefix returns, in recorded order, every event with prefix
// stripped off -- e.g. "start:" -> the Definition keys, in the order Start was
// actually called.
func readinessWithPrefix(events []string, prefix string) []string {
	var out []string
	for _, e := range events {
		if rest, ok := strings.CutPrefix(e, prefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

// readinessIndexOfFirst/readinessIndexOfLast locate the first/last event
// carrying prefix, or -1. Used to compare "when did the last Start finish"
// against "when did the first OpenTraffic start" without caring about the
// exact interleaving of events that do not carry either prefix.
func readinessIndexOfFirst(events []string, prefix string) int {
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

func readinessIndexOfLast(events []string, prefix string) int {
	last := -1
	for i, e := range events {
		if strings.HasPrefix(e, prefix) {
			last = i
		}
	}
	return last
}

// readinessSpec is the behaviour shared by every node type below: record a
// "start:"/"open:" event tagged with the plugin's own name, optionally fail,
// and -- when wired to a shared counter -- prove no two Starts ever overlap.
type readinessSpec struct {
	rec        *readinessRecorder
	concurrent *int32 // shared across every node in the same chain, or nil
	startErr   error
	openErr    error
}

// doStart is readinessSpec's Start body. When concurrent is set, it bumps a
// shared counter on entry and drops it on exit via defer; runtime.Gosched
// (never time.Sleep -- there is nothing to wait for, only a scheduling point
// to yield at) widens the window in which a wrongly-concurrent
// implementation would be caught, without making the test's success depend
// on any wall-clock duration. A correct, serial implementation can never
// observe the counter above 1, no matter how many times this spins.
func (s *readinessSpec) doStart(ctx *plugin.Context) error {
	s.rec.record("start:" + ctx.Name())
	if s.concurrent != nil {
		cur := atomic.AddInt32(s.concurrent, 1)
		defer atomic.AddInt32(s.concurrent, -1)
		for range 200 {
			runtime.Gosched()
		}
		if cur > 1 {
			s.rec.record("violation:concurrent-start:" + ctx.Name())
		}
	}
	return s.startErr
}

// doOpen is readinessSpec's OpenTraffic body.
func (s *readinessSpec) doOpen(ctx *plugin.Context) error {
	s.rec.record("open:" + ctx.Name())
	return s.openErr
}

// --- dependency chain node types --------------------------------------------
//
// The fixtures use distinct types to keep each lifecycle role easy to inspect,
// but dependency identity itself is the stable plugin key. Each dependent node
// therefore receives its predecessor key explicitly, allowing the same Go type
// to be reused under different keys without changing graph semantics.

type readinessNodeA struct {
	plugin.Base
	spec *readinessSpec
}

var (
	_ plugin.Runner        = (*readinessNodeA)(nil)
	_ plugin.TrafficOpener = (*readinessNodeA)(nil)
)

func (n *readinessNodeA) Start(ctx *plugin.Context) error       { return n.spec.doStart(ctx) }
func (n *readinessNodeA) OpenTraffic(ctx *plugin.Context) error { return n.spec.doOpen(ctx) }

type readinessNodeB struct {
	plugin.Base
	spec       *readinessSpec
	dependency plugin.Key
}

var (
	_ plugin.Runner        = (*readinessNodeB)(nil)
	_ plugin.TrafficOpener = (*readinessNodeB)(nil)
	_ plugin.Declarer      = (*readinessNodeB)(nil)
)

func (n *readinessNodeB) Start(ctx *plugin.Context) error       { return n.spec.doStart(ctx) }
func (n *readinessNodeB) OpenTraffic(ctx *plugin.Context) error { return n.spec.doOpen(ctx) }
func (n *readinessNodeB) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{n.dependency.Ref()}}
}

type readinessNodeC struct {
	plugin.Base
	spec       *readinessSpec
	dependency plugin.Key
}

var (
	_ plugin.Runner        = (*readinessNodeC)(nil)
	_ plugin.TrafficOpener = (*readinessNodeC)(nil)
	_ plugin.Declarer      = (*readinessNodeC)(nil)
)

func (n *readinessNodeC) Start(ctx *plugin.Context) error       { return n.spec.doStart(ctx) }
func (n *readinessNodeC) OpenTraffic(ctx *plugin.Context) error { return n.spec.doOpen(ctx) }
func (n *readinessNodeC) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{n.dependency.Ref()}}
}

type readinessNodeD struct {
	plugin.Base
	spec       *readinessSpec
	dependency plugin.Key
}

var (
	_ plugin.Runner        = (*readinessNodeD)(nil)
	_ plugin.TrafficOpener = (*readinessNodeD)(nil)
	_ plugin.Declarer      = (*readinessNodeD)(nil)
)

func (n *readinessNodeD) Start(ctx *plugin.Context) error       { return n.spec.doStart(ctx) }
func (n *readinessNodeD) OpenTraffic(ctx *plugin.Context) error { return n.spec.doOpen(ctx) }
func (n *readinessNodeD) Dependencies() plugin.Deps {
	return plugin.Deps{Plugins: []plugin.Ref{n.dependency.Ref()}}
}

// --- 1. global barrier: every Start before any OpenTraffic ----------------

// TestReadiness_GlobalBarrier_AllStartsCompleteBeforeAnyOpenTraffic pins
// §5.1's defining guarantee. Three plugins, each implementing both Runner and
// TrafficOpener like a real protocol module would, wired A <- B <- C by
// dependency. A broken single-phase implementation that opens each plugin's
// traffic immediately after that same plugin's own Start would still produce
// *a* valid-looking log -- start,open,start,open,start,open -- but it would
// violate the one property this test checks: the last Start in the whole
// application must precede the first OpenTraffic in the whole application.
// That ordering is exactly what "A serves before C has even bound its
// listener" looks like from outside, which is the failure mode §5.1 exists
// to rule out entirely.
func TestReadiness_GlobalBarrier_AllStartsCompleteBeforeAnyOpenTraffic(t *testing.T) {
	rec := newReadinessRecorder()
	var concurrent int32

	specA := &readinessSpec{rec: rec, concurrent: &concurrent}
	specB := &readinessSpec{rec: rec, concurrent: &concurrent}
	specC := &readinessSpec{rec: rec, concurrent: &concurrent}

	app := newTestApp(t,
		def("barrier-a", func() plugin.Plugin { return &readinessNodeA{spec: specA} }),
		def("barrier-b", func() plugin.Plugin { return &readinessNodeB{spec: specB, dependency: "barrier-a"} }),
		def("barrier-c", func() plugin.Plugin { return &readinessNodeC{spec: specC, dependency: "barrier-b"} }),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)

	events := rec.snapshot()
	lastStart := readinessIndexOfLast(events, "start:")
	firstOpen := readinessIndexOfFirst(events, "open:")
	require.NotEqual(t, -1, lastStart, "At least one start event is expected to be observed")
	require.NotEqual(t, -1, firstOpen, "At least one open event is expected to be observed")
	assert.Less(t, lastStart, firstOpen,
		"All Runner.Start must complete before any TrafficOpener.OpenTraffic—"+
			"This is the entire purpose of two-phase readiness; it catches the erroneous implementation of 'immediately OpenTraffic after Start'")
	assert.Empty(t, readinessWithPrefix(events, "violation:"))
}

// --- 2. Start is serial and follows topological order ----------------------

// TestReadiness_StartRunsSeriallyInTopologicalOrder pins §5.1's explicit
// "Serial, not concurrent" clause on a four-node chain. The sequence assertion is
// exact equality against the one legal order for a total chain (not merely
// "b before c"), so a bug that reverses direction or interleaves unrelated
// nodes fails immediately; the concurrency counter inside readinessSpec.doStart
// independently pins "serial" itself, which a merely-correct-by-luck ordering
// assertion could not catch on its own (an implementation that fires every
// Start concurrently but happens to have the fast one finish last could still
// produce the right final order while never actually being serial).
func TestReadiness_StartRunsSeriallyInTopologicalOrder(t *testing.T) {
	rec := newReadinessRecorder()
	var concurrent int32

	app := newTestApp(t,
		def("chain-a", func() plugin.Plugin { return &readinessNodeA{spec: &readinessSpec{rec: rec, concurrent: &concurrent}} }),
		def("chain-b", func() plugin.Plugin {
			return &readinessNodeB{spec: &readinessSpec{rec: rec, concurrent: &concurrent}, dependency: "chain-a"}
		}),
		def("chain-c", func() plugin.Plugin {
			return &readinessNodeC{spec: &readinessSpec{rec: rec, concurrent: &concurrent}, dependency: "chain-b"}
		}),
		def("chain-d", func() plugin.Plugin {
			return &readinessNodeD{spec: &readinessSpec{rec: rec, concurrent: &concurrent}, dependency: "chain-c"}
		}),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)

	events := rec.snapshot()
	assert.Equal(t, []string{"chain-a", "chain-b", "chain-c", "chain-d"},
		readinessWithPrefix(events, "start:"),
		"Start call sequence must strictly equal the dependency topological order, not just any permutation satisfying dependencies")
	assert.Empty(t, readinessWithPrefix(events, "violation:"),
		"At most one Start can be executing at any moment; this event indicates concurrency level exceeds 1, meaning the implementation parallelized the serial Start phase")
}

// --- 3. a failed Start prevents every OpenTraffic ---------------------------

// TestReadiness_StartFailure_PreventsAnyOpenTraffic pins §5.1's whole reason
// for existing: if any Runner.Start fails, no OpenTraffic must ever run, for
// any plugin -- not just the failing one and not just the ones after it.
// b fails; c is never reached (Start is sequential and stops at the first
// error); and critically, a's own Start succeeded but its OpenTraffic must
// still never be called, because phase 2 as a whole never begins once phase
// 1 has failed. This is the property that rules out "a is already answering
// health checks while b never even bound its port".
func TestReadiness_StartFailure_PreventsAnyOpenTraffic(t *testing.T) {
	rec := newReadinessRecorder()
	sentinel := errors.New("boom: bind failed")

	app := newTestApp(t,
		def("fail-a", func() plugin.Plugin { return &readinessNodeA{spec: &readinessSpec{rec: rec}} }),
		def("fail-b", func() plugin.Plugin {
			return &readinessNodeB{spec: &readinessSpec{rec: rec, startErr: sentinel}, dependency: "fail-a"}
		}),
		def("fail-c", func() plugin.Plugin { return &readinessNodeC{spec: &readinessSpec{rec: rec}, dependency: "fail-b"} }),
	)

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fail-b")

	events := rec.snapshot()
	assert.Equal(t, []string{"fail-a", "fail-b"}, readinessWithPrefix(events, "start:"),
		"c comes after b; after b fails, the Start phase must immediately stop, c's Start will never be called")
	assert.Empty(t, readinessWithPrefix(events, "open:"),
		"After the Start phase fails, no plugin's OpenTraffic should be called—even if a has successfully started")
}

// --- 4. a failed OpenTraffic still unwinds and names the plugin ------------

// TestReadiness_OpenTrafficFailure_UnwindsAndNamesThePlugin pins the second
// failure mode: every Start succeeds, but a later OpenTraffic fails. The
// application must still fail startup (exit code 1) and the error must name
// the failing plugin, and OpenTraffic must stop being called for anything
// after it in topological order -- open is sequential exactly like start is.
func TestReadiness_OpenTrafficFailure_UnwindsAndNamesThePlugin(t *testing.T) {
	rec := newReadinessRecorder()
	sentinel := errors.New("boom: port not ready")

	app := newTestApp(t,
		def("open-fail-a", func() plugin.Plugin { return &readinessNodeA{spec: &readinessSpec{rec: rec}} }),
		def("open-fail-b", func() plugin.Plugin {
			return &readinessNodeB{spec: &readinessSpec{rec: rec, openErr: sentinel}, dependency: "open-fail-a"}
		}),
		def("open-fail-c", func() plugin.Plugin {
			return &readinessNodeC{spec: &readinessSpec{rec: rec}, dependency: "open-fail-b"}
		}),
	)

	code, err := app.Execute(context.Background(), quietConfig(t, ""))
	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open-fail-b",
		"The error must specify which plugin's OpenTraffic failed")

	events := rec.snapshot()
	assert.Equal(t, []string{"open-fail-a", "open-fail-b", "open-fail-c"},
		readinessWithPrefix(events, "start:"),
		"The Start phase must have completed for all plugins before OpenTraffic fails")
	assert.Equal(t, []string{"open-fail-a", "open-fail-b"}, readinessWithPrefix(events, "open:"),
		"c comes after b; after b's OpenTraffic fails, c's OpenTraffic must never be called")
}

// --- 5. dependency order overrides Definition-key order --------------------

// TestReadiness_DependencyOverridesDefinitionKeyOrder pins that the
// catalog's deterministic Key order is only a tie-break for unrelated nodes.
// Here the lexically first key ("aaa-second") depends on the lexically last
// key ("zzz-first"), so the real dependency edge must win and Start must run
// zzz-first strictly before aaa-second.
func TestReadiness_DependencyOverridesDefinitionKeyOrder(t *testing.T) {
	rec := newReadinessRecorder()

	app := newTestApp(t,
		def("zzz-first", func() plugin.Plugin { return &readinessNodeA{spec: &readinessSpec{rec: rec}} }),
		def("aaa-second", func() plugin.Plugin { return &readinessNodeB{spec: &readinessSpec{rec: rec}, dependency: "zzz-first"} }),
	)

	done := runAsync(app, quietConfig(t, "")...)
	awaitReady(t, app)
	app.requestStop(stopReasonSignal)
	res := awaitResult(t, done)
	require.NoError(t, res.err)
	assert.Equal(t, 0, res.code)

	events := rec.snapshot()
	assert.Equal(t, []string{"zzz-first", "aaa-second"}, readinessWithPrefix(events, "start:"),
		"The order must follow dependency relationships: aaa-second depends on zzz-first,"+
			"even if aaa-second is sorted earlier by Definition key, it must start after zzz-first")
}
