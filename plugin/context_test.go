package plugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
)

// fakeHost is a minimal in-memory RuntimeHost. Package plugin cannot import
// the real runtime host (runtime imports plugin), so the narrow port is
// exercised through a local stand-in.
type fakeHost struct {
	mu        sync.Mutex
	execution context.Context
	logger    log.Logger
	gate      chan struct{}
	process   string
	submits   []fakeSubmit
	shutdowns []fakeShutdown
	admit     bool
	stopped   bool
	// admission and admissionFor are what the host reports for the two
	// admission questions; nil is what a host with no budget reports, and what
	// Context has to turn into an unbounded limiter.
	admission    Admission
	admissionFor Admission
}

type fakeSubmit struct {
	id       Identity
	critical bool
}

type fakeShutdown struct {
	id     Identity
	reason string
}

func newFakeHost() *fakeHost { return &fakeHost{admit: true} }

func (h *fakeHost) ExecutionContext() context.Context { return h.execution }

func (h *fakeHost) Logger() log.Logger { return h.logger }

func (h *fakeHost) ProcessInstance() string { return h.process }

func (h *fakeHost) TrafficGate() <-chan struct{} { return h.gate }

func (h *fakeHost) Admission(Identity) Admission { return h.admission }

func (h *fakeHost) AdmissionFor(Identity, WorkloadKey) Admission { return h.admissionFor }

func (h *fakeHost) SubmitTask(id Identity, fn func(context.Context), critical bool) bool {
	h.mu.Lock()
	h.submits = append(h.submits, fakeSubmit{id: id, critical: critical})
	admit := h.admit
	h.mu.Unlock()
	if !admit {
		return false
	}
	fn(context.Background())
	return true
}

func (h *fakeHost) RequestShutdown(id Identity, reason string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shutdowns = append(h.shutdowns, fakeShutdown{id: id, reason: reason})
	if h.stopped {
		return false
	}
	h.stopped = true
	return true
}

func (h *fakeHost) submitted() []fakeSubmit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fakeSubmit(nil), h.submits...)
}

func (h *fakeHost) requested() []fakeShutdown {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fakeShutdown(nil), h.shutdowns...)
}

func TestContextForwardsIdentityAndCriticalityToTheHost(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer", Instance: "readonly"})

	var ran bool
	assert.True(t, ctx.Go(func(context.Context) { ran = true }))
	assert.True(t, ran, "an admitted task actually runs")
	assert.True(t, ctx.GoCritical(func(context.Context) {}))

	submitted := host.submitted()
	require.Len(t, submitted, 2)
	assert.Equal(t, Identity{Plugin: "consumer", Instance: "readonly"}, submitted[0].id)
	assert.False(t, submitted[0].critical, "Go submits a non-critical task")
	assert.True(t, submitted[1].critical, "GoCritical submits a critical task")

	host.mu.Lock()
	host.admit = false
	host.mu.Unlock()
	assert.False(t, ctx.Go(func(context.Context) { t.Fatal("rejected task must not run") }),
		"a closed admission window reports rejection to the caller")
}

func TestContextIdentityIsNormalizedOnceAtConstruction(t *testing.T) {
	t.Parallel()
	named := NewRuntimeContext(newFakeHost(), Identity{Plugin: "gorm", Instance: "readonly"})
	assert.Equal(t, "gorm", named.Name())
	assert.Equal(t, Key("gorm"), named.Key())
	assert.Equal(t, "readonly", named.Instance())
	assert.Equal(t, Identity{Plugin: "gorm", Instance: "readonly"}, named.Identity())

	unnamed := NewRuntimeContext(newFakeHost(), Identity{Plugin: "gorm"})
	assert.Equal(t, DefaultInstance, unnamed.Instance(), "Instance never hands back an empty string")
	assert.Equal(t, "gorm", unnamed.Identity().String())
}

// TestTheProcessIsIdentifiedSeparatelyFromThePluginInstance pins the one axis
// Instance cannot express. Both halves matter: two Contexts in one process
// answer with the same process identity however differently their Definitions
// are instanced, and a Context whose plugin instance name is the default still
// reports the process it runs in. Code that publishes "who holds this" into a
// store shared by replicas is only correct if it reaches for this one.
func TestTheProcessIsIdentifiedSeparatelyFromThePluginInstance(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	host.process = "host-7-1758091200-9f3c1a2b"

	named := NewRuntimeContext(host, Identity{Plugin: "cron", Instance: "reports"})
	unnamed := NewRuntimeContext(host, Identity{Plugin: "cron"})

	assert.Equal(t, host.process, named.ProcessInstance())
	assert.Equal(t, host.process, unnamed.ProcessInstance(),
		"one process answers with one identity no matter how its plugins are instanced")
	assert.NotEqual(t, named.Instance(), named.ProcessInstance(),
		"the plugin instance name is a different question and must not stand in for the process")
}

// TestAContextWithNoRuntimeNamesNoProcess keeps the empty string meaning exactly
// one thing: there is no runtime behind this Context. A real host always has an
// identity to report -- the runtime derives one when the deployment gave none --
// so a caller that sees empty is looking at a zero value, not at an anonymous
// process, and nothing here invents a name to paper over that.
func TestAContextWithNoRuntimeNamesNoProcess(t *testing.T) {
	t.Parallel()
	var missing *Context
	assert.Empty(t, missing.ProcessInstance())
	assert.Empty(t, NewRuntimeContext(nil, Identity{Plugin: "p"}).ProcessInstance())
}

func TestContextLogFallsBackToTheProcessLogger(t *testing.T) {
	t.Parallel()
	ctx := NewRuntimeContext(newFakeHost(), Identity{Plugin: "p"})
	assert.NotNil(t, ctx.Log(), "a host without a logger falls back instead of panicking")

	host := newFakeHost()
	host.logger = log.L().With("plugin", "p")
	assert.Equal(t, host.logger, NewRuntimeContext(host, Identity{Plugin: "p"}).Log())
}

func TestContextImplementsStandardContextAndForwardsExecutionScope(t *testing.T) {
	t.Parallel()
	type keyType struct{}
	key := keyType{}
	deadline := time.Now().Add(time.Minute)
	parent, cancel := context.WithDeadline(context.WithValue(context.Background(), key, "execution-value"), deadline)
	defer cancel()

	host := newFakeHost()
	host.execution = parent
	var standard context.Context = NewRuntimeContext(host, Identity{Plugin: "context-aware"})

	gotDeadline, ok := standard.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, deadline, gotDeadline, time.Millisecond)
	assert.Equal(t, "execution-value", standard.Value(key))
	require.NoError(t, standard.Err())

	cancel()
	select {
	case <-standard.Done():
	case <-time.After(time.Second):
		t.Fatal("Context.Done is not closed after the execution context is canceled")
	}
	assert.ErrorIs(t, standard.Err(), context.Canceled)
}

func TestContextsDoNotShareCancellationOrGateState(t *testing.T) {
	t.Parallel()
	canceledHost := newFakeHost()
	parent, cancel := context.WithCancel(context.Background())
	canceledHost.execution = parent
	canceled := NewRuntimeContext(canceledHost, Identity{Plugin: "first"})
	cancel()
	<-canceled.Done()
	require.ErrorIs(t, canceled.Err(), context.Canceled)

	fresh := NewRuntimeContext(newFakeHost(), Identity{Plugin: "second"})
	assert.NoError(t, fresh.Err(), "a new Context cannot inherit another Context's cancellation")
	assert.Nil(t, fresh.Done(), "a host without an execution context keeps Background semantics")
}

func TestContextTrafficGateAndShutdownRequestPassThroughTheHost(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	host.gate = make(chan struct{})
	ctx := NewRuntimeContext(host, Identity{Plugin: "web"})

	gate := ctx.TrafficGate()
	require.NotNil(t, gate)
	select {
	case <-gate:
		t.Fatal("the traffic gate must stay closed until the runtime releases it")
	default:
	}
	close(host.gate)
	<-gate

	assert.True(t, ctx.RequestShutdown("listener failed"))
	assert.False(t, ctx.RequestShutdown("second"), "the host decides that only the first request wins")
	requested := host.requested()
	require.Len(t, requested, 2)
	assert.Equal(t, Identity{Plugin: "web", Instance: DefaultInstance}, requested[0].id)
	assert.Equal(t, "listener failed", requested[0].reason)
}

func TestContextWithoutHostDegradesInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]*Context{
		"nil":       nil,
		"zero host": {id: Identity{Plugin: "p", Instance: DefaultInstance}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotNil(t, ctx.Log())
			assert.NoError(t, ctx.Err())
			assert.Nil(t, ctx.Done())
			assert.Nil(t, ctx.TrafficGate())
			_, ok := ctx.Deadline()
			assert.False(t, ok)
			assert.Nil(t, ctx.Value("unused"))
			assert.False(t, ctx.Go(func(context.Context) { t.Fatal("must not run") }))
			assert.False(t, ctx.GoCritical(func(context.Context) { t.Fatal("must not run") }))
			assert.False(t, ctx.RequestShutdown("ignored"))

			// Admission degrades the other way round: with nothing to bound,
			// the limiter admits immediately rather than blocking forever on a
			// quota no host will ever give back.
			for name, admission := range map[string]Admission{
				"own workload": ctx.Admission(),
				"named":        ctx.AdmissionFor("sast"),
			} {
				release, err := admission.Acquire(context.Background())
				assert.NoError(t, err, "%s: a missing host never fails the acquisition", name)
				assert.NotNil(t, release, "%s: and still hands back a release", name)
				release()
			}
			_, err := ctx.AdmissionFor("").Acquire(context.TODO())
			assert.NoError(t, err)
		})
	}
}

// TestContextAdmissionReportsWhatTheHostSaysAboutTheWorkload pins the two
// answers the facade must keep apart: the Plugin's own workload and a named
// one. A host that reports no limiter -- a composition whose workloads declare
// no budget -- must read as unbounded, never as an error.
func TestContextAdmissionReportsWhatTheHostSaysAboutTheWorkload(t *testing.T) {
	t.Parallel()
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "worker"})

	assert.IsType(t, noAdmission{}, ctx.Admission(), "a host with no budget reports an unbounded limiter")
	assert.IsType(t, noAdmission{}, ctx.AdmissionFor("sast"))
	assert.IsType(t, noAdmission{}, ctx.AdmissionFor(""),
		"the empty key names no workload, so the host is not even consulted")

	own := &recordingAdmission{}
	named := &recordingAdmission{}
	host.admission = own
	host.admissionFor = named
	assert.Same(t, own, ctx.Admission(), "the own-workload limiter comes from the host")
	assert.Same(t, named, ctx.AdmissionFor("sast"), "and the named one is asked for by key")
}

// recordingAdmission is a limiter that stands in for the runtime's own.
type recordingAdmission struct{}

func (*recordingAdmission) Acquire(context.Context) (func(), error) { return func() {}, nil }
