package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// blockedHandler is the route body a pressure run parks requests in. Holding
// every admitted request inside the handler is what makes "the ceiling is fully
// occupied" an observable state rather than a race: until the run releases
// them, no admitted request can finish and free a slot.
type blockedHandler struct {
	release chan struct{}

	inFlight atomic.Int64
	peak     atomic.Int64
}

func newBlockedHandler() *blockedHandler {
	return &blockedHandler{release: make(chan struct{})}
}

func (h *blockedHandler) handle(_ context.Context, c *web.Ctx) error {
	current := h.inFlight.Add(1)
	h.raisePeak(current)

	<-h.release
	h.inFlight.Add(-1)

	c.Status(http.StatusOK)
	return nil
}

// raisePeak records current as the highest concurrency this handler has seen.
// The peak the ceiling is measured against has to come from the handler, not
// from the gate: the gate's own occupancy cannot exceed its limit by
// construction, so reading it would assert nothing.
func (h *blockedHandler) raisePeak(current int64) {
	for {
		observed := h.peak.Load()
		if current <= observed || h.peak.CompareAndSwap(observed, current) {
			return
		}
	}
}

// blockedServer is one started Server whose /block route parks admitted
// requests, together with the engine that dispatches into it.
type blockedServer struct {
	server *web.Server
	engine *enginetest.Engine
	state  *blockedHandler
}

// startBlockedServer starts a Server over the given configuration and adds the
// blocking route to whatever the caller contributed.
func startBlockedServer(t *testing.T, cfg web.Config, inputs serverInputs) blockedServer {
	t.Helper()

	state := newBlockedHandler()
	inputs.routes = append(inputs.routes, plugin.Entry[web.RouteContributor]{
		Identity: plugin.Identity{Plugin: "inflight-pressure"},
		Value: fakeRouteContributor{register: func(router *web.Router) {
			router.GET("/block", state.handle)
		}},
	})

	server, ctx, _ := newPingServer(t, cfg, inputs)
	require.NoError(t, server.Start(ctx))
	return blockedServer{server: server, engine: testEngineOf(t, server), state: state}
}

// inFlightPressure is what one pressure run observed: how many requests the
// gate admitted, how many it refused, the highest handler concurrency, and every
// response so a test can inspect the refusals individually.
type inFlightPressure struct {
	limit     int
	admitted  int
	refused   int
	peak      int64
	responses []*httptest.ResponseRecorder
}

// runBlockedRequests dispatches that many concurrent requests into a started
// Server and returns once every one of them has been answered. It offers more
// requests than the ceiling admits on purpose: the excess are the refusals.
//
// The run releases the parked handlers only after every refusal has been
// answered, so a request counted as admitted cannot be an artifact of another
// request having finished first.
func runBlockedRequests(t *testing.T, fixture blockedServer, requests int) inFlightPressure {
	t.Helper()

	limit := fixture.server.InFlightStats().Limit
	require.Positive(t, limit, "the run needs an admission ceiling in force")
	require.Greater(t, requests, limit, "the run must offer more requests than the ceiling admits")

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fixture.state.release) }) }
	// A failed require below ends this goroutine before the run's own release,
	// and the parked handlers must not be left waiting on a channel nobody will
	// close.
	defer release()

	var answered atomic.Int64
	responses := make([]*httptest.ResponseRecorder, requests)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/block", nil))
			responses[index] = response
			answered.Add(1)
		}(i)
	}

	// The run's progress signal is deliberately not the gate's own rejection
	// counter: a broken counter would then surface as this timeout instead of
	// as the assertion that reads it. A request answered before the release is
	// one the gate refused, because every admitted one is parked in the handler,
	// so the excess being answered is the ceiling having been held at its limit
	// for the whole window.
	answeredExcess := pollUntil(2*time.Second, time.Millisecond, func() bool {
		return answered.Load() == int64(requests-limit)
	})
	assert.LessOrEqual(t, fixture.state.peak.Load(), int64(limit),
		"concurrency inside the handler exceeded the admission ceiling")
	require.True(t, answeredExcess,
		"every request past the ceiling must be answered while the admitted ones are still being served")

	release()
	wg.Wait()

	pressure := inFlightPressure{limit: limit, peak: fixture.state.peak.Load(), responses: responses}
	for _, response := range responses {
		switch response.Code {
		case http.StatusOK:
			pressure.admitted++
		case http.StatusServiceUnavailable:
			pressure.refused++
		}
	}
	return pressure
}

// TestInFlightGateRefusesBeyondItsCeiling is the pressure test: the observed
// concurrency inside the handler must never exceed web.max_in_flight, and every
// request past it must be refused rather than queued, with the retry hint the
// contract promises.
func TestInFlightGateRefusesBeyondItsCeiling(t *testing.T) {
	const limit = 2
	const requests = 5

	// The counting middleware sits in PhaseRecover, the outermost phase a
	// contributed middleware can claim. A refused request must not reach it:
	// the gate is outside every middleware, not merely outside the business
	// ones, so a refusal is not observed, logged, or metered as a request.
	var reached atomic.Int64
	fixture := startBlockedServer(t, blockedConfig(t, limit), serverInputs{
		middlewares: []plugin.Entry[web.Middleware]{{
			Identity: plugin.Identity{Plugin: "inflight-probe"},
			Value: fakeMiddleware{
				order: web.Order{Phase: web.PhaseRecover},
				handler: func(_ context.Context, c *web.Ctx) error {
					reached.Add(1)
					c.Next()
					return nil
				},
			},
		}},
	})

	pressure := runBlockedRequests(t, fixture, requests)

	assert.Equal(t, limit, pressure.limit)
	assert.Equal(t, int64(limit), pressure.peak,
		"the ceiling must be reached, and reached by no more than the ceiling")
	assert.Equal(t, limit, pressure.admitted, "the ceiling must be fully used, not left idle")
	assert.Equal(t, requests-limit, pressure.refused, "every request past the ceiling must be refused")

	assert.Equal(t, int64(limit), reached.Load(),
		"a refused request must not enter any contributed middleware, PhaseRecover included")

	assert.Equal(t, web.InFlightStats{Limit: limit, Rejections: requests - limit}, fixture.server.InFlightStats(),
		"the gate's own counter must record every refusal and nothing else")

	for _, response := range pressure.responses {
		if response.Code != http.StatusServiceUnavailable {
			continue
		}
		assert.Equal(t, "1", response.Header().Get("Retry-After"),
			"a refusal must tell the client when to come back")
		assert.Equal(t, web.ProblemContentType, response.Header().Get("Content-Type"))
		assert.Contains(t, response.Body.String(), `"code":"server_overloaded"`,
			"a refusal is a Problem Detail like every other Web error")
		assert.Contains(t, response.Body.String(), `"status":503`)
	}
}

// TestZeroMaxInFlightBehavesLikeTheDerivedCeiling pins the default: an unset
// key derives the ceiling from GOMAXPROCS, and deriving it is not a special
// mode -- the gate assembled around the derived value admits and refuses exactly
// as one configured with that value explicitly does.
func TestZeroMaxInFlightBehavesLikeTheDerivedCeiling(t *testing.T) {
	derived := runtime.GOMAXPROCS(0)
	require.Positive(t, derived)

	for _, test := range []struct {
		name       string
		configured int
	}{
		{"derived", 0},
		{"configured to the derived value", derived},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := startBlockedServer(t, blockedConfig(t, test.configured), serverInputs{})
			require.Equal(t, derived, fixture.server.InFlightStats().Limit)

			pressure := runBlockedRequests(t, fixture, derived+1)
			assert.Equal(t, derived, pressure.admitted)
			assert.Equal(t, 1, pressure.refused, "the derived ceiling must refuse the request past it")
			assert.LessOrEqual(t, pressure.peak, int64(derived))
		})
	}
}

// TestInFlightPressureLeavesNoGoroutineBehind pins the other half of the gate's
// resource contract. Admission is a token test, so a refused request costs no
// goroutine and an admitted one spawns none; a gate that parked refused requests
// on a channel would satisfy every other test here while turning a burst into a
// pile of waiting goroutines.
func TestInFlightPressureLeavesNoGoroutineBehind(t *testing.T) {
	const limit = 2

	fixture := startBlockedServer(t, blockedConfig(t, limit), serverInputs{})
	before := settledGoroutines()

	pressure := runBlockedRequests(t, fixture, 6)

	assert.Equal(t, limit, pressure.admitted)
	assert.Equal(t, 6-limit, pressure.refused)
	assert.Zero(t, fixture.server.InFlightStats().InFlight,
		"every admitted request must have returned its slot")
	assert.LessOrEqual(t, settledGoroutines(), before,
		"the gate must leave no goroutine behind after a burst of refusals")
}

// TestInFlightSlotIsReleasedWhenTheHandlerPanics pins release on the panic path.
// The panic boundary is downstream of the gate -- PhaseRecover is a middleware
// -- so the gate is the outermost frame a panic unwinds through, and a slot lost
// there would make the process narrower with every panic until it refused
// everything.
func TestInFlightSlotIsReleasedWhenTheHandlerPanics(t *testing.T) {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxInFlight = 1

	server, ctx, _ := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "inflight-panic"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.GET("/boom", func(context.Context, *web.Ctx) error {
					panic("handler failure")
				})
			}},
		}},
	})
	require.NoError(t, server.Start(ctx))
	engine := testEngineOf(t, server)

	assert.Panics(t, func() {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	}, "the panic must reach the caller: no middleware sits outside the gate to recover it")

	// /ping is admitted only if the panicking request returned its slot. With a
	// ceiling of one there is no other slot it could have used.
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ping", nil))

	assert.Equal(t, http.StatusOK, response.Code,
		"the panicking request must have released its slot")
	assert.Equal(t, web.InFlightStats{Limit: 1}, server.InFlightStats(),
		"a panic is not a refusal, and no slot may remain occupied")
}

// TestStartReportsTheInFlightGateAndRejections pins what an operator can read:
// Start logs the ceiling in force and where it came from, and a saturated
// process reports the episode rather than each refused request, because a
// refused request reaches no middleware that could have recorded it.
func TestStartReportsTheInFlightGateAndRejections(t *testing.T) {
	const limit = 2

	logger := &captureLogger{Logger: log.Nop()}
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxInFlight = limit

	state := newBlockedHandler()
	server, ctx, host := newPingServer(t, cfg, serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "inflight-report"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.GET("/block", state.handle)
			}},
		}},
	})
	host.logger = logger
	require.NoError(t, server.Start(ctx))

	var report string
	for _, line := range logger.infos() {
		if strings.Contains(line, "in-flight gate") {
			report = line
			break
		}
	}
	require.NotEmpty(t, report, "Start must report the in-flight gate it assembled")
	assert.Contains(t, report, "limit=2")
	assert.Contains(t, report, "web.max_in_flight")
	assert.Contains(t, report, "retry_after=1s")
	assert.Contains(t, report, "rejections=0")

	fixture := blockedServer{server: server, engine: testEngineOf(t, server), state: state}
	pressure := runBlockedRequests(t, fixture, 5)
	require.Equal(t, 5-limit, pressure.refused)

	warns := linesContaining(logger.warns(), inFlightSaturatedMessage)
	require.Len(t, warns, 1,
		"a saturated process must report the episode once, not once per refused request")
	assert.Contains(t, warns[0], "limit=2", "the warn line must name the ceiling that refused the request")
	assert.Contains(t, warns[0], "rejections=1", "the warn line must name the refusals it reports")

	// runBlockedRequests releases every parked handler, so the process drains and
	// the episode ends within the run.
	cleared := linesContaining(logger.infos(), inFlightClearedMessage)
	require.Len(t, cleared, 1, "the end of an episode must be reported once")
	assert.Contains(t, cleared[0], fmt.Sprintf("rejections_total=%d", pressure.refused),
		"the closing line must account for every refusal of the episode")
}

// The two messages the gate reports an episode with. A test matches on them
// rather than on "the last warn" so an unrelated warn cannot pass for the
// saturation report.
const (
	inFlightSaturatedMessage = "web: in-flight limit reached"
	inFlightClearedMessage   = "web: in-flight limit cleared"
)

// TestInFlightSaturationIsReportedPerEpisodeNotPerRefusal pins the operational
// bound on the gate's logging. Saturation is the worst possible moment to
// amplify logging -- the gate exists to protect a process that is already out of
// capacity, and a line per refused request spends another round of CPU and IO on
// describing the refusals -- so the volume must not follow the refusal count.
//
// Bounded must not mean late, either: the run reads the log while the process is
// still refusing, because "refusals started" is an event an operator has to see
// when it happens rather than after the burst is over.
func TestInFlightSaturationIsReportedPerEpisodeNotPerRefusal(t *testing.T) {
	const limit = 2
	const refusals = 6

	logger := &captureLogger{Logger: log.Nop()}
	first, second := newBlockedHandler(), newBlockedHandler()
	server, ctx, host := newPingServer(t, blockedConfig(t, limit), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "inflight-episodes"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.GET("/block", first.handle)
				// A second parking route is what lets the run saturate the gate
				// twice: releasing an episode's handlers cannot be undone.
				router.GET("/block-again", second.handle)
			}},
		}},
	})
	host.logger = logger
	require.NoError(t, server.Start(ctx))
	engine := testEngineOf(t, server)

	saturate := func(path string, state *blockedHandler) func() {
		t.Helper()
		var parked sync.WaitGroup
		for range limit {
			parked.Add(1)
			go func() {
				defer parked.Done()
				engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
			}()
		}
		require.True(t, pollUntil(2*time.Second, time.Millisecond, func() bool {
			return state.inFlight.Load() == int64(limit)
		}), "the ceiling must be fully occupied before the run offers a refusable request")
		return func() {
			close(state.release)
			parked.Wait()
		}
	}

	drain := saturate("/block", first)
	for range refusals {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/block-again", nil))
		require.Equal(t, http.StatusServiceUnavailable, response.Code,
			"every request offered past the ceiling must be refused")
	}

	// Read the log with the process still saturated: the opening line is here
	// already, and it is the only one the refusals produced.
	warns := linesContaining(logger.warns(), inFlightSaturatedMessage)
	require.Len(t, warns, 1,
		"refusals must produce one report for the episode, whatever their number")
	assert.Contains(t, warns[0], "limit=2")
	assert.Contains(t, warns[0], "rejections=1",
		"the opening line reports the refusal that opened the episode")
	assert.Contains(t, warns[0], "retry_after_seconds=1",
		"the opening line must say what refused clients were told")
	assert.Empty(t, linesContaining(logger.infos(), inFlightClearedMessage),
		"recovery must not be reported while the process is still refusing requests")

	drain()

	cleared := linesContaining(logger.infos(), inFlightClearedMessage)
	require.Len(t, cleared, 1, "draining must report the end of the episode once")
	assert.Contains(t, cleared[0], "limit=2")
	assert.Contains(t, cleared[0], fmt.Sprintf("rejections=%d", refusals-1),
		"the closing line must carry every refusal the opening line did not")
	assert.Contains(t, cleared[0], fmt.Sprintf("rejections_total=%d", refusals))

	// A second episode must be reported too: an operator who missed the first
	// warn still has to learn that refusals resumed.
	drainAgain := saturate("/block-again", second)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/block", nil))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)

	warns = linesContaining(logger.warns(), inFlightSaturatedMessage)
	require.Len(t, warns, 2, "a new episode must be reported, not suppressed by the first one")
	assert.Contains(t, warns[1], fmt.Sprintf("rejections_total=%d", refusals+1))

	drainAgain()
	assert.Len(t, linesContaining(logger.infos(), inFlightClearedMessage), 2,
		"the second episode must end as visibly as the first")
	assert.Equal(t, web.InFlightStats{Limit: limit, Rejections: refusals + 1}, server.InFlightStats(),
		"reporting an episode must not change what the gate counts")
}

// linesContaining keeps the recorded lines whose message the test is about, so
// an unrelated line at the same level cannot satisfy or break an assertion.
func linesContaining(lines []string, message string) []string {
	var kept []string
	for _, line := range lines {
		if strings.Contains(line, message) {
			kept = append(kept, line)
		}
	}
	return kept
}

func blockedConfig(t *testing.T, maxInFlight int) web.Config {
	t.Helper()
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxInFlight = maxInFlight
	return cfg
}

// settledGoroutines samples the goroutine count once it stops moving, so that a
// goroutine still unwinding from an earlier test does not decide a leak
// assertion. runtime/doctor_test.go carries the same helper for the same reason:
// the count is only meaningful once it has settled.
func settledGoroutines() int {
	previous := runtime.NumGoroutine()
	for attempt := 0; attempt < 50; attempt++ {
		time.Sleep(10 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == previous {
			return current
		}
		previous = current
	}
	return previous
}

// captureLogger records the lines Web logs, by level, so a test can assert on
// what an operator would read without standing up a logging backend.
type captureLogger struct {
	log.Logger

	mu      sync.Mutex
	infoLog []string
	warnLog []string
}

func (l *captureLogger) Info(msg string, kv ...any) { l.record(&l.infoLog, msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any) { l.record(&l.warnLog, msg, kv...) }

func (l *captureLogger) record(destination *[]string, msg string, kv ...any) {
	line := msg
	for i := 0; i+1 < len(kv); i += 2 {
		line += fmt.Sprintf(" %v=%v", kv[i], kv[i+1])
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	*destination = append(*destination, line)
}

func (l *captureLogger) infos() []string { return l.snapshot(&l.infoLog) }
func (l *captureLogger) warns() []string { return l.snapshot(&l.warnLog) }

func (l *captureLogger) snapshot(source *[]string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), *source...)
}
