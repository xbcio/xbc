package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zeroDefinition plugin.Definition
	if Definition() == zeroDefinition {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}

	first := Bundle()
	second := Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestConfiguredClientHealthProbeStartAndPrimaryContracts(t *testing.T) {
	var probes int
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		probes++
		if request.Method != http.MethodGet || request.URL.Path != "/" {
			t.Fatalf("health request = %s %s", request.Method, request.URL.Path)
		}
		return response(http.StatusOK, `{}`), nil
	}}
	config := validConfig("http://unused.example")
	config.HealthProbe = true
	factory := &fixedFactory{client: newClientWithBackend(backend, time.Second)}
	client := constructTestClient(t, config, factory)
	if factory.calls != 1 {
		t.Fatalf("factory calls = %d, want 1", factory.calls)
	}

	host := newFakeHost()
	ctx := runtimeContext(host, "search")
	if err := initClient(client, ctx); err != nil {
		t.Fatalf("initClient() error = %v", err)
	}
	if probes != 1 {
		t.Fatalf("health probes = %d, want 1", probes)
	}
	if err := startClient(client, ctx); err != nil {
		t.Fatalf("startClient() error = %v", err)
	}
	if _, ok := any(client).(BulkIndexer); !ok {
		t.Fatal("primary *Client does not export BulkIndexer")
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("primary BulkIndexer Flush() error = %v", err)
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v", err)
	}
	host.waitTasks(t)
	if backend.closes() != 1 {
		t.Fatalf("backend closes = %d, want 1", backend.closes())
	}
}

func TestConfiguredClientCanDisableHealthProbe(t *testing.T) {
	backend := &stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		t.Fatal("health probe ran while disabled")
		return nil, nil
	}}
	client := constructTestClient(t, validConfig("http://unused.example"), &fixedFactory{
		client: newClientWithBackend(backend, time.Second),
	})
	host := newFakeHost()
	ctx := runtimeContext(host, plugin.DefaultInstance)
	if err := initClient(client, ctx); err != nil {
		t.Fatalf("initClient() error = %v", err)
	}
	if err := startClient(client, ctx); err != nil {
		t.Fatalf("startClient() error = %v", err)
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v", err)
	}
	host.waitTasks(t)
}

func TestConstructionAndInitFailuresCleanUpOwnedTransport(t *testing.T) {
	factoryFailure := errors.New("factory failed")
	backend := &stubBackend{}
	client, err := newConfiguredClient(
		mustNormalizeConfig(t, validConfig("http://unused.example")),
		&fixedFactory{client: newClientWithBackend(backend, time.Second), err: factoryFailure},
		nil,
	)
	if client != nil || !errors.Is(err, factoryFailure) {
		t.Fatalf("newConfiguredClient() = %#v, %v; want nil factory failure", client, err)
	}
	if backend.closes() != 1 {
		t.Fatalf("failed factory backend closes = %d, want 1", backend.closes())
	}

	healthFailure := errors.New("health failed")
	backend = &stubBackend{performFn: func(*http.Request) (*http.Response, error) {
		return nil, healthFailure
	}}
	config := validConfig("http://unused.example")
	config.HealthProbe = true
	client = constructTestClient(t, config, &fixedFactory{client: newClientWithBackend(backend, time.Second)})
	if err := initClient(client, runtimeContext(newFakeHost(), "failed")); !errors.Is(err, healthFailure) {
		t.Fatalf("initClient() error = %v, want health failure", err)
	}
	// A successful factory transfers ownership before Init. Stop must therefore
	// be safe and complete cleanup after the failed lifecycle stage.
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() after failed Init error = %v", err)
	}
	if backend.closes() != 1 {
		t.Fatalf("failed Init backend closes = %d, want 1", backend.closes())
	}
}

func TestClientObserverFreezesWhenStartBegins(t *testing.T) {
	client := constructTestClient(t, validConfig("http://unused.example"), &fixedFactory{
		client: newClientWithBackend(&stubBackend{}, time.Second),
	})
	if err := client.SetBulkObserver(BulkObserverFunc(func(BulkResult) {})); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	ctx := runtimeContext(host, plugin.DefaultInstance)
	if err := startClient(client, ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.SetBulkObserver(nil); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("SetBulkObserver() error = %v, want frozen error", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.waitTasks(t)
}

func TestClientConcurrentStopDrainsAndReturnsSharedError(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	flushErr := errors.New("flush failed")
	var requestOnce, closeOnce sync.Once
	backend := &stubBackend{
		performFn: func(*http.Request) (*http.Response, error) {
			requestOnce.Do(func() { close(requestStarted) })
			<-releaseRequest
			return nil, flushErr
		},
		closeFn: func(context.Context) error {
			closeOnce.Do(func() { close(closeStarted) })
			<-releaseClose
			return nil
		},
	}
	config := validConfig("http://unused.example")
	config.Bulk.FlushActions = 1
	client := constructTestClient(t, config, &fixedFactory{client: newClientWithBackend(backend, time.Second)})
	host := newFakeHost()
	if err := startClient(client, runtimeContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk request did not start")
	}

	results := make(chan error, 2)
	go func() { results <- client.Close(context.Background()) }()
	go func() { results <- client.Close(context.Background()) }()
	select {
	case err := <-results:
		t.Fatalf("Close returned before flush completed: %v", err)
	case <-time.After(15 * time.Millisecond):
	}
	close(releaseRequest)
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("client close did not start after flush")
	}
	select {
	case err := <-results:
		t.Fatalf("Close returned before transport close completed: %v", err)
	case <-time.After(15 * time.Millisecond):
	}
	close(releaseClose)
	for range 2 {
		if err := <-results; !errors.Is(err, flushErr) {
			t.Fatalf("Close() error = %v, want shared flush error", err)
		}
	}
	host.waitTasks(t)
	if backend.closes() != 1 {
		t.Fatalf("backend closes = %d, want 1", backend.closes())
	}
	if err := client.Close(context.Background()); !errors.Is(err, flushErr) {
		t.Fatalf("subsequent Close() error = %v, want shared flush error", err)
	}
}

func TestClientStopHonorsDeadlineWhileDrainContinues(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	backend := &stubBackend{performFn: func(request *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return successfulBulkResponse(request)
	}}
	config := validConfig("http://unused.example")
	config.Bulk.FlushActions = 1
	client := constructTestClient(t, config, &fixedFactory{client: newClientWithBackend(backend, time.Second)})
	host := newFakeHost()
	if err := startClient(client, runtimeContext(host, plugin.DefaultInstance)); err != nil {
		t.Fatal(err)
	}
	if err := client.Add(context.Background(), BulkItem{Index: "events", Document: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk request did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	if err := client.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("Close ignored deadline for %s", elapsed)
	}
	close(release)
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("fresh Close() did not retrieve completed cleanup: %v", err)
	}
	host.waitTasks(t)
}

func TestStopIsSafeBeforeInitAfterPartialStartAndRepeatedly(t *testing.T) {
	if err := stopClient(nil, nil); err != nil {
		t.Fatalf("stopClient(nil) error = %v", err)
	}

	backend := &stubBackend{}
	client := constructTestClient(t, validConfig("http://unused.example"), &fixedFactory{
		client: newClientWithBackend(backend, time.Second),
	})
	if err := client.Close(nil); err != nil {
		t.Fatalf("Close() before Init error = %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
	if backend.closes() != 1 {
		t.Fatalf("backend closes = %d, want 1", backend.closes())
	}

	backend = &stubBackend{}
	client = constructTestClient(t, validConfig("http://unused.example"), &fixedFactory{
		client: newClientWithBackend(backend, time.Second),
	})
	host := newFakeHost()
	host.rejectTasks = true
	if err := startClient(client, runtimeContext(host, plugin.DefaultInstance)); err == nil {
		t.Fatal("startClient() unexpectedly succeeded after task rejection")
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close() after partial Start error = %v", err)
	}
	if backend.closes() != 1 {
		t.Fatalf("partial Start backend closes = %d, want 1", backend.closes())
	}
}

func constructTestClient(t *testing.T, config Config, factory clientFactory) *Client {
	t.Helper()
	client, err := newConfiguredClient(mustNormalizeConfig(t, config), factory, nil)
	if err != nil {
		t.Fatalf("newConfiguredClient() error = %v", err)
	}
	return client
}

func mustNormalizeConfig(t *testing.T, config Config) normalizedConfig {
	t.Helper()
	normalized, err := normalizeConfig(config)
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	return normalized
}
