package management

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// startTestPlugin builds and starts a plugin against a fresh host and returns
// the bound address. The plugin is stopped on cleanup with a bounded context
// before the host shuts down, so a failed assertion can never leave the
// serving task waiting and hang the binary.
func startTestPlugin(t *testing.T, cfg Config, gatherer prometheus.Gatherer) (*Plugin, *bootHost, string) {
	t.Helper()
	host := newBootHost()
	t.Cleanup(host.shutdown)

	plugin, err := New(cfg, gatherer)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := plugin.Start(host.contextFor(Key)); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	addr := plugin.addr()
	if addr == "" {
		t.Fatal("Start() did not bind a listener")
	}
	// Registered after host.shutdown, so LIFO makes this run first.
	t.Cleanup(func() {
		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plugin.Stop(stopContext); err != nil {
			t.Errorf("cleanup Stop() = %v", err)
		}
	})
	return plugin, host, addr
}

// TestTheListenerServesOnlyOnceTheTrafficGateReleases pins the admission rule:
// the socket exists as soon as Start returns, but nothing is answered until
// the runtime opens traffic, so a scrape can never observe a process that did
// not finish starting.
func TestTheListenerServesOnlyOnceTheTrafficGateReleases(t *testing.T) {
	plugin, host, addr := startTestPlugin(t, Config{Addr: "127.0.0.1:0"}, expositionRegistry(t))

	if _, _, err := fetch(addr, "/metrics", 200*time.Millisecond); err == nil {
		t.Fatal("the listener answered a scrape before the traffic gate released")
	}

	host.releaseTraffic()
	waitForServing(t, addr, "/metrics")

	status, body, err := fetch(addr, "/metrics", 2*time.Second)
	if err != nil {
		t.Fatalf("fetch(/metrics) = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("fetch(/metrics) status = %d, want 200", status)
	}
	if !strings.Contains(body, "management_probe_total") {
		t.Errorf("the exposition does not carry the registry's series:\n%s", body)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := plugin.Stop(stopContext); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

// TestEndpointsAppearOnlyWhenTheirSourceIsSelected pins both conditionals: an
// absent gatherer leaves /metrics unregistered -- so a scrape cannot mistake
// it for an empty registry -- and the pprof subtree needs the explicit opt-in.
func TestEndpointsAppearOnlyWhenTheirSourceIsSelected(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cfg         Config
		gatherer    *blockingGatherer
		metricsWant int
		pprofWant   int
	}{
		{"neither selected", Config{Addr: "127.0.0.1:0"}, nil, http.StatusNotFound, http.StatusNotFound},
		{"metrics only", Config{Addr: "127.0.0.1:0"}, newBlockingGatherer(t), http.StatusOK, http.StatusNotFound},
		{"pprof only", Config{Addr: "127.0.0.1:0", Pprof: true}, nil, http.StatusNotFound, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The conversion is guarded: a nil *blockingGatherer assigned to
			// the prometheus.Gatherer parameter would be a non-nil interface
			// holding a nil pointer, which would register /metrics over a
			// gatherer that panics.
			var source prometheus.Gatherer
			if tc.gatherer != nil {
				tc.gatherer.unblock()
				source = tc.gatherer
			}
			plugin, host, addr := startTestPlugin(t, tc.cfg, source)
			host.releaseTraffic()
			waitForServing(t, addr, "/metrics")

			for _, probe := range []struct {
				path string
				want int
			}{
				{"/metrics", tc.metricsWant},
				{"/debug/pprof/", tc.pprofWant},
			} {
				status, _, err := fetch(addr, probe.path, 2*time.Second)
				if err != nil {
					t.Fatalf("fetch(%s) = %v", probe.path, err)
				}
				if status != probe.want {
					t.Errorf("fetch(%s) status = %d, want %d", probe.path, status, probe.want)
				}
			}

			stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := plugin.Stop(stopContext); err != nil {
				t.Fatalf("Stop() = %v", err)
			}
		})
	}
}

// TestThePprofSubtreeDispatchesTheStandardHandlers drives one endpoint of each
// kind the subtree fronts -- the index, a named profile, and the POST-only
// symbol endpoint -- so the single registration is proven to cover the whole
// standard set rather than just its root.
func TestThePprofSubtreeDispatchesTheStandardHandlers(t *testing.T) {
	plugin, host, addr := startTestPlugin(t, Config{Addr: "127.0.0.1:0", Pprof: true}, nil)
	host.releaseTraffic()
	waitForServing(t, addr, "/debug/pprof/")

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline", "/debug/pprof/goroutine"} {
		status, body, err := fetch(addr, path, 2*time.Second)
		if err != nil {
			t.Fatalf("fetch(%s) = %v", path, err)
		}
		if status != http.StatusOK {
			t.Errorf("fetch(%s) status = %d, want 200", path, status)
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("fetch(%s) answered an empty body", path)
		}
	}

	response, err := http.Post("http://"+addr+"/debug/pprof/symbol", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /debug/pprof/symbol = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("POST /debug/pprof/symbol status = %d, want 200", response.StatusCode)
	}

	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := plugin.Stop(stopContext); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

// TestTheListenerWithoutEndpointsSaysSo pins the diagnostic for the
// composition that configured a listener and selected nothing to serve on it.
func TestTheListenerWithoutEndpointsSaysSo(t *testing.T) {
	host := newBootHost()
	t.Cleanup(host.shutdown)
	logger := &recordingLogger{}
	host.logger = logger

	plugin, err := New(Config{Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := plugin.Start(host.contextFor(Key)); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	// Registered after host.shutdown, so LIFO stops the plugin first: a serving
	// task that reached Serve must be unblocked by a closed listener, not left
	// in Accept for host.shutdown's WaitGroup to join forever.
	t.Cleanup(func() {
		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plugin.Stop(stopContext); err != nil {
			t.Errorf("cleanup Stop() = %v", err)
		}
	})
	if !containsLine(logger.warnLines(), "serves no endpoints") {
		t.Errorf("warnings = %v, want one naming the empty listener", logger.warnLines())
	}
}

// TestStopDrainsInFlightScrapesAndRefusesNewOnes pins the drain contract with a
// scrape parked inside the registry: Shutdown closes the listener first -- a
// new scrape is refused while the parked one is still running -- and then
// waits, so the in-flight scrape is answered rather than cut short.
func TestStopDrainsInFlightScrapesAndRefusesNewOnes(t *testing.T) {
	gatherer := newBlockingGatherer(t)
	plugin, host, addr := startTestPlugin(t, Config{Addr: "127.0.0.1:0"}, gatherer)
	host.releaseTraffic()
	// No waitForServing probe here: every scrape parks inside this gatherer,
	// so the first scrape to enter is the one this test parks deliberately.

	// The release is guarded and deferred: an assertion that ends this test
	// early must not leave the scrape parked, because Stop would then wait for
	// it until its context expires.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(gatherer.unblock) }
	defer release()

	type answer struct {
		status int
		body   string
		err    error
	}
	parked := make(chan answer, 1)
	go func() {
		status, body, err := fetch(addr, "/metrics", 5*time.Second)
		parked <- answer{status: status, body: body, err: err}
	}()
	if !pollUntil(2*time.Second, 5*time.Millisecond, func() bool { return gatherer.started.Load() == 1 }) {
		t.Fatal("the scrape never reached the registry")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- plugin.Stop(context.Background()) }()

	// A refused connection is the observable state "the drain is in progress":
	// Shutdown closes its listener before it waits for the request that is
	// still parked inside the handler.
	if !pollUntil(2*time.Second, 10*time.Millisecond, func() bool {
		_, _, err := fetch(addr, "/metrics", 200*time.Millisecond)
		return err != nil
	}) {
		t.Fatal("Stop must close the listener before waiting for the in-flight scrape")
	}
	select {
	case result := <-parked:
		t.Fatalf("the parked scrape returned before the registry was released: %+v", result)
	default:
	}

	release()
	result := <-parked
	if result.err != nil {
		t.Fatalf("the in-flight scrape was cut short by the drain: %v", result.err)
	}
	if result.status != http.StatusOK || !strings.Contains(result.body, "management_probe_total") {
		t.Errorf("the in-flight scrape = (%d, %q), want the full exposition", result.status, result.body)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if _, _, err := fetch(addr, "/metrics", 200*time.Millisecond); err == nil {
		t.Fatal("Stop must leave the listener refusing new connections")
	}
}

// TestStopForceClosesAConnectionItsDeadlineCouldNotDrain pins the forced-close
// branch of Stop. A scrape parked inside the registry outlives a deadline that
// has already expired, so Shutdown cannot drain it; Stop must then close the
// live connection itself and report the failed drain, rather than report the
// failure and leave the connection open. The plugin's "no live connection
// survives Stop under any path" promise holds on this branch exactly as it
// does on the drained one.
func TestStopForceClosesAConnectionItsDeadlineCouldNotDrain(t *testing.T) {
	gatherer := newBlockingGatherer(t)
	plugin, host, addr := startTestPlugin(t, Config{Addr: "127.0.0.1:0"}, gatherer)
	host.releaseTraffic()
	// No waitForServing probe here: every scrape parks inside this gatherer,
	// so the first scrape to enter is the one this test parks deliberately.

	// The release is guarded and deferred: the assertion below must be able to
	// fail without leaving the scrape parked for the cleanup's Stop.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(gatherer.unblock) }
	defer release()

	type answer struct {
		status int
		body   string
		err    error
	}
	parked := make(chan answer, 1)
	go func() {
		// The client timeout deliberately outlives every deadline in this test,
		// so a connection the forced close failed to cut cannot end the request
		// by timing out before the assertion observes it.
		status, body, err := fetch(addr, "/metrics", 30*time.Second)
		parked <- answer{status: status, body: body, err: err}
	}()
	if !pollUntil(2*time.Second, 5*time.Millisecond, func() bool { return gatherer.started.Load() == 1 }) {
		t.Fatal("the scrape never reached the registry")
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	stopErr := plugin.Stop(expired)
	if stopErr == nil || !strings.Contains(stopErr.Error(), "shutdown failed") {
		t.Fatalf("Stop() with an expired deadline = %v, want the shutdown-failed error the un-drained work produces", stopErr)
	}

	// The parked scrape must end promptly -- cut by the forced close, not later
	// by a timeout of its own. Waiting for that error rather than asserting it
	// eventually arrives is what tells the forced close apart from a connection
	// Stop left open.
	var outcome answer
	cut := pollUntil(2*time.Second, 10*time.Millisecond, func() bool {
		select {
		case outcome = <-parked:
			return true
		default:
			return false
		}
	})
	if !cut {
		t.Fatal("the scrape parked inside the registry survived Stop; its connection was not force-closed")
	}
	if outcome.err == nil {
		t.Fatalf("the parked scrape was answered instead of cut: %+v", outcome)
	}
	if _, _, err := fetch(addr, "/metrics", 200*time.Millisecond); err == nil {
		t.Fatal("Stop must leave the listener refusing new connections")
	}
}

// TestStopBeforeTheTrafficGateClosesTheListenerWithoutServing pins the
// shutdown-before-release path: nothing has served, so the socket is the only
// thing to release, and the serving task that is still waiting for a gate that
// never came must exit quietly when the runtime cancels its task context.
func TestStopBeforeTheTrafficGateClosesTheListenerWithoutServing(t *testing.T) {
	host := newBootHost()
	t.Cleanup(host.shutdown)
	logger := &recordingLogger{}
	host.logger = logger

	plugin, err := New(Config{Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := plugin.Start(host.contextFor(Key)); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	addr := plugin.addr()

	if err := plugin.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() before the gate = %v", err)
	}
	if err := plugin.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() twice = %v", err)
	}

	// Releasing the gate after Stop is what a runtime does when admission was
	// rejected or shutdown raced the release: the serving task must observe a
	// closed listener, not panic and not start answering.
	host.releaseTraffic()
	host.shutdown()
	if lines := logger.errorLines(); len(lines) != 0 {
		t.Errorf("a listener closed before the gate logged %v, want no errors", lines)
	}
	if _, _, err := fetch(addr, "/metrics", 200*time.Millisecond); err == nil {
		t.Fatal("a listener stopped before the gate must not answer")
	}
}

// TestStopIsSafeBeforeStart pins the construction-only state: a plugin that
// never started has nothing to release.
func TestStopIsSafeBeforeStart(t *testing.T) {
	plugin, err := New(Config{Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if err := plugin.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() before Start = %v", err)
	}
}

// TestStartRefusesASecondStartAndABusyAddress pins both Start refusals: the
// double start leaves the first listener serving, and a bind failure admits no
// serving task at all.
func TestStartRefusesASecondStartAndABusyAddress(t *testing.T) {
	t.Run("second start", func(t *testing.T) {
		plugin, host, addr := startTestPlugin(t, Config{Addr: "127.0.0.1:0"}, nil)
		err := plugin.Start(host.contextFor(Key))
		if err == nil || !strings.Contains(err.Error(), "already started") {
			t.Fatalf("second Start() = %v, want an already-started refusal", err)
		}
		host.releaseTraffic()
		waitForServing(t, addr, "/metrics")

		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plugin.Stop(stopContext); err != nil {
			t.Fatalf("Stop() = %v", err)
		}
	})

	t.Run("busy address", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		defer occupied.Close()

		host := newBootHost()
		t.Cleanup(host.shutdown)
		plugin, err := New(Config{Addr: occupied.Addr().String()}, nil)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		if err := plugin.Start(host.contextFor(Key)); err == nil || !strings.Contains(err.Error(), "failed to listen on") {
			t.Fatalf("Start() on a busy address = %v, want a listen failure", err)
		}
		if got := host.submissions(); got != 0 {
			t.Errorf("a failed Start admitted %d serving tasks, want 0", got)
		}
		if err := plugin.Stop(context.Background()); err != nil {
			t.Fatalf("Stop() after a failed Start = %v", err)
		}
	})
}

// TestTheListenerTerminatesTLSAndEnforcesItsFloor pins the TLS material end to
// end: the handshake succeeds against the configured certificate at the
// declared floor, and plaintext cannot reach the handlers.
func TestTheListenerTerminatesTLSAndEnforcesItsFloor(t *testing.T) {
	certFile, keyFile := writeCertificatePair(t)
	plugin, host, addr := startTestPlugin(t, Config{
		Addr: "127.0.0.1:0",
		TLS:  TLSConfig{CertFile: certFile, KeyFile: keyFile, MinVersion: "1.2"},
	}, expositionRegistry(t))
	host.releaseTraffic()

	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		t.Fatal("the generated certificate did not parse")
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}},
	}
	var response *http.Response
	if !pollUntil(2*time.Second, 20*time.Millisecond, func() bool {
		response, err = client.Get("https://" + addr + "/metrics")
		if err != nil {
			return false
		}
		return true
	}) {
		t.Fatalf("the TLS listener never answered: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("reading the HTTPS exposition: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS scrape status = %d, want 200", response.StatusCode)
	}
	if response.TLS == nil || response.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("negotiated TLS = %+v, want at least TLS 1.2", response.TLS)
	}
	if !strings.Contains(string(body), "management_probe_total") {
		t.Errorf("the HTTPS exposition does not carry the registry's series:\n%s", body)
	}
	// A cleartext request is refused by the standard library with its own 400,
	// exactly as on the Web transport's management listener: the handlers stay
	// unreachable over a socket that speaks TLS.
	plaintextStatus, plaintextBody, err := fetch(addr, "/metrics", time.Second)
	if err != nil {
		t.Fatalf("the plaintext probe could not complete: %v", err)
	}
	if plaintextStatus != http.StatusBadRequest {
		t.Errorf("plaintext scrape status = %d, want 400", plaintextStatus)
	}
	if strings.Contains(plaintextBody, "management_probe_total") {
		t.Error("a plaintext request read the registry through the TLS listener")
	}

	stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := plugin.Stop(stopContext); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

// containsLine reports whether any recorded line contains the fragment.
func containsLine(lines []string, fragment string) bool {
	for _, line := range lines {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}
