package composition_test

// This file holds the rolling-restart case: two replicas of one service run
// side by side behind a round-robin proxy, one replica is asked to stop, and
// the caller's own client policy decides what happens to a request that meets
// the replica the ingress has already withdrawn. The GET is retried and
// succeeds on the other replica; the POST is handed back to the caller
// untouched. Both applications are real, booted through the public facade.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/httpclient"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	webhealth "github.com/xbcio/xbc/transport/web/extensions/reliability/health"
)

const (
	// rollingService is the identity both replicas present: they are one
	// service, which is what makes the caller indifferent to which one
	// answers.
	rollingService = "composition-rolling"

	// rollingPreDrain is the window a stopping replica keeps its listener
	// open while readiness is already down -- the window a real ingress
	// notices the instance is leaving rotation.
	rollingPreDrain = 5 * time.Second
)

// ── the replica ────────────────────────────────────────────────────────

// rollingReplica is one B instance's route. It answers with its own name, so
// the test can tell which replica served a request.
type rollingReplica struct{ name string }

var _ web.RouteContributor = (*rollingReplica)(nil)

func rollingReplicaBundle(name string) plugin.Bundle {
	replica := &rollingReplica{name: name}
	return plugin.BundleOf(plugin.Define(
		"composition-rolling-replica",
		func(plugin.BuildContext) (*rollingReplica, error) { return replica, nil },
		plugin.Options[*rollingReplica]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *rollingReplica) web.RouteContributor { return value }),
			),
		},
	))
}

func (p *rollingReplica) RegisterRoutes(router *web.Router) {
	router.GET("/work", p.handleWork).Name("composition.rolling.work").Auth(web.Public())
	router.POST("/work", p.handleWork).Name("composition.rolling.work-post").Auth(web.Public())
}

func (p *rollingReplica) handleWork(_ context.Context, c *web.Ctx) error {
	c.JSON(http.StatusOK, map[string]string{"replica": p.name})
	return nil
}

// rollingReplicaConfig renders one replica's configuration: the Web listener
// and the pre-drain window its stop holds. Both replicas write no logs --
// the test reads responses, and a file sink shared by two live processes
// would only record the first one's shutdown.
func rollingReplicaConfig(addr string) string {
	return fmt.Sprintf(`xbc:
  shutdown_timeout: 20s
  service:
    name: %s
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
  shutdown:
    pre_drain_delay: %s
`, rollingService, addr, rollingPreDrain)
}

// ── the ingress ────────────────────────────────────────────────────────

// proxyAttempt is one decision the proxy recorded, in call order: which
// replica a request was picked for, and whether the proxy forwarded it or
// answered 503 because the replica had left rotation.
type proxyAttempt struct {
	method  string
	replica string
	outcome string
}

// rollingBackend is one replica as the ingress knows it: the name it answers
// under and the address to reach it on.
type rollingBackend struct {
	name string
	addr string
}

// rollingProxy is the ingress in front of the replicas: it round-robins every
// request and consults the picked replica's readiness first. A replica whose
// stop has begun is withdrawn -- readiness down, listener still finishing --
// and the proxy answers 503 for it instead of forwarding, which is what turns
// a request into the retryable failure the caller's client policy decides on.
// The 503 belongs to the ingress, not to the replica: the replica would still
// have answered, which is exactly why the ingress, not the service, is what
// says "gone".
type rollingProxy struct {
	replicas []rollingBackend
	forward  *http.Client

	mu       sync.Mutex
	next     int
	attempts []proxyAttempt
}

func newRollingProxy(t *testing.T, replicas ...rollingBackend) (*rollingProxy, string) {
	t.Helper()
	proxy := &rollingProxy{
		replicas: replicas,
		forward:  &http.Client{Timeout: 5 * time.Second},
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return proxy, server.URL
}

func (p *rollingProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	replica := p.pick()
	if err := rollingReady(replica.addr); err != nil {
		p.record(request.Method, replica.name, "withdrawn")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(writer, "replica %s is withdrawn: %v", replica.name, err)
		return
	}
	p.record(request.Method, replica.name, "forwarded")

	forwarded, err := http.NewRequestWithContext(request.Context(), request.Method,
		"http://"+replica.addr+request.URL.Path, request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	response, err := p.forward.Do(forwarded)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

// pick advances the rotation and returns the next replica.
func (p *rollingProxy) pick() rollingBackend {
	p.mu.Lock()
	defer p.mu.Unlock()
	replica := p.replicas[p.next%len(p.replicas)]
	p.next++
	return replica
}

func (p *rollingProxy) record(method, replica, outcome string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts = append(p.attempts, proxyAttempt{method: method, replica: replica, outcome: outcome})
}

func (p *rollingProxy) snapshot() []proxyAttempt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxyAttempt(nil), p.attempts...)
}

// rollingReady asks one replica whether it is still ready. Any answer other
// than 200 -- and any failure to answer at all -- means it has left rotation.
func rollingReady(addr string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz answered %d", response.StatusCode)
	}
	return nil
}

// ── the caller ─────────────────────────────────────────────────────────

// rollingCaller is A's route: it relays whatever method and body it received
// to the proxy through the configured http-client instance, so the caller's
// own retry policy -- not the test's -- decides what the caller sees.
type rollingCaller struct {
	client *httpclient.Client
	target string
}

var _ web.RouteContributor = (*rollingCaller)(nil)

func rollingCallerBundle(target string) plugin.Bundle {
	calleeClient := plugin.RefToInstance[*httpclient.Client](httpclient.Key, callInstance)
	return plugin.BundleOf(plugin.Define(
		"composition-rolling-caller",
		func(ctx plugin.BuildContext) (*rollingCaller, error) {
			return &rollingCaller{client: calleeClient.Get(ctx).Value, target: target}, nil
		},
		plugin.Options[*rollingCaller]{
			Inputs: plugin.Inputs(calleeClient),
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *rollingCaller) web.RouteContributor { return value }),
			),
		},
	))
}

func (p *rollingCaller) RegisterRoutes(router *web.Router) {
	router.GET("/call", p.handleCall).Name("composition.rolling.call-get").Auth(web.Public())
	router.POST("/call", p.handleCall).Name("composition.rolling.call-post").Auth(web.Public())
}

func (p *rollingCaller) handleCall(_ context.Context, c *web.Ctx) error {
	// The downstream request is built from the received payload, buffered in
	// full: a bodyless method forwards nothing, and a payload the client may
	// have to send again is a payload it can produce again. Forwarding the
	// inbound stream itself would work too, but only while the body stays
	// inside the cap -- a buffer has no such edge.
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	if c.Request().Method == http.MethodGet || c.Request().Method == http.MethodHead {
		body = nil
	}
	request, err := http.NewRequestWithContext(c.Request().Context(), c.Request().Method, p.target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	response, err := p.client.Client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	c.Data(response.StatusCode, "application/json", answer)
	return nil
}

// rollingCallerConfig renders the caller's configuration: one http-client
// instance whose retry policy is the subject of the case -- a GET that meets
// the ingress's 503 is retried, and the total budget covers both attempts.
func rollingCallerConfig(addr, proxyURL string) string {
	return fmt.Sprintf(`xbc:
  shutdown_timeout: 20s
  service:
    name: composition-rolling-caller
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
plugins:
  http-client:
    %s:
      base_url: %q
      timeout: 2s
      total_timeout: 10s
      retry:
        max_attempts: 3
        initial_backoff: 50ms
        max_backoff: 200ms
`, addr, callInstance, proxyURL)
}

// dualPost is dualGet's method-carrying sibling: one POST with a body over a
// fresh connection, so a keep-alive from an earlier request cannot decide
// whether the next one is the attempt under test.
func dualPost(t *testing.T, addr, path, body string, timeout time.Duration) dualHTTPResult {
	t.Helper()
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		return dualHTTPResult{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return dualHTTPResult{err: err}
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return dualHTTPResult{err: err}
	}
	return dualHTTPResult{status: response.StatusCode, body: string(content)}
}

// ── the case ───────────────────────────────────────────────────────────

// TestARollingRestartRetriesTheGetAndHandsBackThePost drives one restart of a
// two-replica service. The first replica is asked to stop, its readiness
// withdraws, and the round-robin ingress answers 503 for the picks that land
// on it. The caller's GET is retried and served by the second replica; the
// caller's POST, which the client must not replay, comes back to the caller
// as the 503 itself.
func TestARollingRestartRetriesTheGetAndHandsBackThePost(t *testing.T) {
	callerAddr := dualReserveAddr(t)
	firstAddr := dualReserveAddr(t)
	secondAddr := dualReserveAddr(t)
	proxy, proxyURL := newRollingProxy(t,
		rollingBackend{name: "b1", addr: firstAddr},
		rollingBackend{name: "b2", addr: secondAddr},
	)

	runFirst := dualBootApp(t, rollingReplicaConfig(firstAddr),
		web.Bundle(), ginengine.Bundle(), webhealth.Bundle(), rollingReplicaBundle("b1"),
	)
	dualWaitFor(t, 10*time.Second, "the first replica to serve", func() error { return dualHTTPReady(firstAddr) })
	runSecond := dualBootApp(t, rollingReplicaConfig(secondAddr),
		web.Bundle(), ginengine.Bundle(), webhealth.Bundle(), rollingReplicaBundle("b2"),
	)
	dualWaitFor(t, 10*time.Second, "the second replica to serve", func() error { return dualHTTPReady(secondAddr) })

	runCaller := dualBootApp(t, rollingCallerConfig(callerAddr, proxyURL),
		web.Bundle(), ginengine.Bundle(), webhealth.Bundle(), httpclient.Bundle(),
		rollingCallerBundle(proxyURL+"/work"),
	)
	dualWaitFor(t, 10*time.Second, "the caller to serve", func() error { return dualHTTPReady(callerAddr) })

	// The rollout begins: the first replica's readiness must withdraw while
	// its listener is still finishing the requests it already accepted, which
	// is the window the ingress acts on.
	runFirst.cancel()
	dualWaitFor(t, 5*time.Second, "the first replica's readiness to withdraw", func() error {
		result := dualGet(firstAddr, "/readyz", "", 500*time.Millisecond)
		if result.err != nil {
			return result.err
		}
		if result.status != http.StatusServiceUnavailable {
			return fmt.Errorf("readyz status = %d, want 503", result.status)
		}
		return nil
	})

	getResult := dualGet(callerAddr, "/call", "", 5*time.Second)
	require.NoError(t, getResult.err)
	require.Equal(t, http.StatusOK, getResult.status,
		"the GET must be retried past the withdrawn replica and answered by the other one")
	require.Contains(t, getResult.body, "b2", "the retry must land on the second replica")

	postResult := dualPost(t, callerAddr, "/call", `{"order":"1"}`, 5*time.Second)
	require.NoError(t, postResult.err)
	require.Equal(t, http.StatusServiceUnavailable, postResult.status,
		"a POST the peer may have acted on must come back as the 503, not be replayed")
	require.Contains(t, postResult.body, "b1", "the caller must receive the ingress's own refusal")

	require.Equal(t, []proxyAttempt{
		{method: http.MethodGet, replica: "b1", outcome: "withdrawn"},
		{method: http.MethodGet, replica: "b2", outcome: "forwarded"},
		{method: http.MethodPost, replica: "b1", outcome: "withdrawn"},
	}, proxy.snapshot(),
		"the round robin must have produced exactly the one retry the GET policy allows and no retry for the POST")

	runCaller.cancel()
	require.NoError(t, runCaller.wait(t, 20*time.Second).err)
	runSecond.cancel()
	require.NoError(t, runSecond.wait(t, 20*time.Second).err)
	require.NoError(t, runFirst.wait(t, 20*time.Second).err,
		"the stopping replica must finish its pre-drain window and shut down")
}
