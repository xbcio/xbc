package elasticx

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	proxyChildModeEnv    = "ELASTICX_TEST_PROXY_CHILD"
	proxyChildAddressEnv = "ELASTICX_TEST_PROXY_ADDRESS"
	// proxyChildAddress is a reserved, unresolvable host: without a proxy the
	// request must fail, which is exactly the direct-connection signal the
	// disable_proxy case needs.
	proxyChildAddress = "http://elasticsearch.invalid:9200"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type recordingProxy struct {
	mu       sync.Mutex
	requests []string
}

func (proxy *recordingProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	proxy.mu.Lock()
	proxy.requests = append(proxy.requests, request.Method+" "+request.URL.String())
	proxy.mu.Unlock()
	writer.Header().Set("X-Elastic-Product", "Elasticsearch")
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, `{}`)
}

func (proxy *recordingProxy) count() int {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return len(proxy.requests)
}

// proxyChildEnvironment builds a child environment with a deterministic proxy
// configuration. http.ProxyFromEnvironment caches the process environment on
// first use, so a subprocess is the only way to make proxy assertions
// independent of which test ran first in this binary.
func proxyChildEnvironment(proxyURL, mode string) []string {
	environment := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			switch strings.ToUpper(name) {
			case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY":
				continue
			}
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY=",
		"NO_PROXY=",
		"ALL_PROXY=",
		proxyChildModeEnv+"="+mode,
		proxyChildAddressEnv+"="+proxyChildAddress,
	)
}

// TestNewProxyEnvironmentControl verifies the two halves of the proxy contract
// end to end: the default transport follows HTTP_PROXY, and disable_proxy
// connects directly. The proxy is an httptest server; no real proxy or
// Elasticsearch cluster is involved.
func TestNewProxyEnvironmentControl(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		wantProxied bool
	}{
		{name: "default honors proxy environment", mode: "proxy", wantProxied: true},
		{name: "disable_proxy connects directly", mode: "direct", wantProxied: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &recordingProxy{}
			proxy := httptest.NewServer(recorder)
			t.Cleanup(proxy.Close)
			command := exec.Command(os.Args[0], "-test.run=^TestNewProxyEnvironmentChild$", "-test.timeout=60s")
			command.Env = proxyChildEnvironment(proxy.URL, test.mode)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("proxy child failed: %v\n%s", err, output)
			}
			proxied := recorder.count() > 0
			if proxied != test.wantProxied {
				t.Fatalf("proxied = %v, want %v", proxied, test.wantProxied)
			}
		})
	}
}

// TestNewProxyEnvironmentChild is the subprocess half of
// TestNewProxyEnvironmentControl. It skips unless the parent supplies a mode.
func TestNewProxyEnvironmentChild(t *testing.T) {
	mode := os.Getenv(proxyChildModeEnv)
	if mode == "" {
		t.Skip("subprocess-only test; see TestNewProxyEnvironmentControl")
	}
	address := os.Getenv(proxyChildAddressEnv)
	if address == "" {
		t.Fatalf("%s is missing", proxyChildAddressEnv)
	}
	config := validConfig(address)
	config.Timeout = 5 * time.Second
	config.DisableProxy = mode == "direct"

	client, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	err = client.Health(context.Background())
	switch mode {
	case "proxy":
		if err != nil {
			t.Fatalf("Health() through the proxy error = %v", err)
		}
	case "direct":
		if err == nil {
			t.Fatal("Health() unexpectedly reached the reserved host without a proxy")
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

// TestNewCustomTransportOwnsRequests verifies a caller-supplied RoundTripper
// serves every request, including the Typed client's, so an otherwise
// unreachable address never touches the network.
func TestNewCustomTransportOwnsRequests(t *testing.T) {
	var calls atomic.Int64
	custom := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		result := response(http.StatusOK, `{}`)
		result.Header.Set("X-Elastic-Product", "Elasticsearch")
		return result, nil
	})

	client, err := New(validConfig("http://unreachable.invalid:9200"), WithTransport(custom))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("custom transport calls = %d, want 1", got)
	}
	if _, err := client.Typed().Core.Info().Do(context.Background()); err != nil {
		t.Fatalf("typed Info() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("custom transport calls after typed request = %d, want 2", got)
	}
}

// TestNewRejectsCustomTransportConfigurationConflicts pins the chosen conflict
// semantics: a caller-supplied transport owns TLS and proxy behavior, so New
// refuses configuration it would otherwise silently discard.
func TestNewRejectsCustomTransportConfigurationConflicts(t *testing.T) {
	custom := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{}`), nil
	})
	tests := []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{name: "ca file", modify: func(config *Config) {
			config.TLS.CAFile = filepath.Join(t.TempDir(), "ca.pem")
		}, want: "tls.ca_file"},
		{name: "insecure skip verify", modify: func(config *Config) {
			config.TLS.InsecureSkipVerify = true
		}, want: "tls.insecure_skip_verify"},
		{name: "disable proxy", modify: func(config *Config) {
			config.DisableProxy = true
		}, want: "disable_proxy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig("http://unused.example")
			test.modify(&config)
			client, err := New(config, WithTransport(custom))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want a conflict naming %q", err, test.want)
			}
			if client != nil {
				_ = client.Close(context.Background())
			}
		})
	}
}

type transportLogEntry struct {
	method string
	path   string
	status int
	err    error
}

type recordingTransportLogger struct {
	mu      sync.Mutex
	entries []transportLogEntry
}

func (logger *recordingTransportLogger) LogRoundTrip(request *http.Request, response *http.Response, err error, _ time.Time, _ time.Duration) error {
	entry := transportLogEntry{err: err}
	if request != nil {
		entry.method = request.Method
		entry.path = request.URL.Path
	}
	if response != nil {
		entry.status = response.StatusCode
	}
	logger.mu.Lock()
	logger.entries = append(logger.entries, entry)
	logger.mu.Unlock()
	return nil
}

func (*recordingTransportLogger) RequestBodyEnabled() bool  { return false }
func (*recordingTransportLogger) ResponseBodyEnabled() bool { return false }

func (logger *recordingTransportLogger) snapshot() []transportLogEntry {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return append([]transportLogEntry(nil), logger.entries...)
}

// TestNewTransportLoggerReceivesRoundTrips verifies the vendor logger is
// installed and called with the request and response of a real round trip.
func TestNewTransportLoggerReceivesRoundTrips(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Elastic-Product", "Elasticsearch")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{}`)
	}))
	t.Cleanup(server.Close)

	logger := &recordingTransportLogger{}
	client, err := New(validConfig(server.URL), WithTransportLogger(logger))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	entries := logger.snapshot()
	if len(entries) != 1 {
		t.Fatalf("logger entries = %d, want 1: %+v", len(entries), entries)
	}
	entry := entries[0]
	if entry.method != http.MethodGet || entry.path != "/" || entry.status != http.StatusOK || entry.err != nil {
		t.Fatalf("logger entry = %+v, want a successful GET /", entry)
	}
}

// TestTransportOptionsMapToVendorConfig pins the default-off mapping and the
// option plumbing without enabling the vendor's process-wide stdout logger in
// this test process.
func TestTransportOptionsMapToVendorConfig(t *testing.T) {
	normalized := mustNormalizeConfig(t, validConfig("http://unused.example"))
	built, err := buildElasticConfig(normalized, transportOptions{})
	if err != nil {
		t.Fatalf("buildElasticConfig() error = %v", err)
	}
	if built.Logger != nil || built.EnableDebugLogger {
		t.Fatalf("default vendor config = logger %v, debug %v; want both off", built.Logger, built.EnableDebugLogger)
	}
	if built.Transport != nil {
		t.Fatalf("default vendor config transport = %T, want the vendor default", built.Transport)
	}

	logger := &recordingTransportLogger{}
	built, err = buildElasticConfig(normalized, transportOptions{logger: logger, debugLogger: true})
	if err != nil {
		t.Fatalf("buildElasticConfig() error = %v", err)
	}
	if built.Logger != logger {
		t.Fatalf("vendor config logger was not installed")
	}
	if !built.EnableDebugLogger {
		t.Fatal("vendor config EnableDebugLogger = false, want true")
	}

	settings := clientOptions{}
	WithDebugLogger()(&settings)
	WithTransportLogger(logger)(&settings)
	custom := roundTripFunc(func(*http.Request) (*http.Response, error) { return response(http.StatusOK, `{}`), nil })
	WithTransport(custom)(&settings)
	if !settings.debugLogger || settings.transportLogger != logger || settings.transport == nil {
		t.Fatalf("option plumbing = %+v", settings)
	}
}

// TestExplicitTransportAppliesProxyAndTLSOverrides covers the combinations the
// request-level tests do not: the package-built transport is only created when
// a setting requires it, and proxy and certificate settings compose.
func TestExplicitTransportAppliesProxyAndTLSOverrides(t *testing.T) {
	if transport, err := explicitTransport(nil, false, false); err != nil || transport != nil {
		t.Fatalf("explicitTransport(default) = %v, %v; want the vendor default", transport, err)
	}

	transport, err := explicitTransport(nil, false, true)
	if err != nil {
		t.Fatalf("explicitTransport(disable proxy) error = %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("disable_proxy transport still consults the proxy environment")
	}

	transport, err = explicitTransport(nil, true, true)
	if err != nil {
		t.Fatalf("explicitTransport(insecure) error = %v", err)
	}
	if transport.Proxy != nil || transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("insecure transport = %+v", transport)
	}

	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	transport, err = explicitTransport(certificate, false, true)
	if err != nil {
		t.Fatalf("explicitTransport(ca, disable proxy) error = %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("disable_proxy with a CA still consults the proxy environment")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("CA transport = %+v", transport)
	}
}
