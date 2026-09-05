package elasticsearch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type fakeHost struct {
	ctx context.Context

	mu          sync.Mutex
	rejectTasks bool
	tasks       sync.WaitGroup
}

func newFakeHost() *fakeHost { return &fakeHost{ctx: context.Background()} }

func (host *fakeHost) ExecutionContext() context.Context {
	if host == nil || host.ctx == nil {
		return context.Background()
	}
	return host.ctx
}

func (*fakeHost) Logger() log.Logger { return log.Nop() }

func (*fakeHost) TrafficGate() <-chan struct{} {
	gate := make(chan struct{})
	close(gate)
	return gate
}

func (host *fakeHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	host.mu.Lock()
	if host.rejectTasks {
		host.mu.Unlock()
		return false
	}
	host.tasks.Add(1)
	ctx := host.ExecutionContext()
	host.mu.Unlock()
	go func() {
		defer host.tasks.Done()
		fn(ctx)
	}()
	return true
}

func (*fakeHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (host *fakeHost) waitTasks(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		host.tasks.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("managed bulk worker did not stop")
	}
}

func runtimeContext(host plugin.RuntimeHost, instance string) *plugin.Context {
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key, Instance: instance})
}

type stubBackend struct {
	performFn func(*http.Request) (*http.Response, error)
	closeFn   func(context.Context) error

	mu         sync.Mutex
	closeCalls int
}

func (backend *stubBackend) Perform(request *http.Request) (*http.Response, error) {
	if backend.performFn == nil {
		return response(http.StatusOK, `{}`), nil
	}
	return backend.performFn(request)
}

func (backend *stubBackend) Close(ctx context.Context) error {
	backend.mu.Lock()
	backend.closeCalls++
	backend.mu.Unlock()
	if backend.closeFn != nil {
		return backend.closeFn(ctx)
	}
	return nil
}

func (backend *stubBackend) closes() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.closeCalls
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type fixedFactory struct {
	client *Client
	err    error
	calls  int
}

func (factory *fixedFactory) New(normalizedConfig) (*Client, error) {
	factory.calls++
	return factory.client, factory.err
}

func validConfig(address string) Config {
	config := defaultConfig()
	config.Addresses = []string{address}
	config.HealthProbe = false
	return config
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
