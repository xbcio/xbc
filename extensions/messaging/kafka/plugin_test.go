package kafka

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestPrepareConfigPreservesDefaultsAndValidates(t *testing.T) {
	defaults := defaultConfig()
	defaults.Brokers = []string{"127.0.0.1:9092"}
	prepared, err := prepareConfig(defaults)
	if err != nil {
		t.Fatalf("prepareConfig(defaults) error = %v", err)
	}
	if !reflect.DeepEqual(prepared, defaults) {
		t.Fatalf("prepared config = %#v, want %#v", prepared, defaults)
	}
	if _, err := prepareConfig(Config{}); err == nil {
		t.Fatal("prepareConfig(empty) error = nil")
	}
}

func TestNewManagedClientMapsConfigAndRollsBackWriterFailure(t *testing.T) {
	writer := &fakeWriter{}
	factory := &fakeFactory{writer: writer}
	client, err := newManagedClient(validConfig(), factory)
	if err != nil {
		t.Fatalf("newManagedClient() error = %v", err)
	}
	factory.mu.Lock()
	brokers := append([]string(nil), factory.writerConfig.Brokers...)
	factory.mu.Unlock()
	if !reflect.DeepEqual(brokers, []string{"127.0.0.1:9092"}) {
		t.Fatalf("writer brokers = %v", brokers)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}

	sentinel := errors.New("dial failed")
	failedWriter := &fakeWriter{}
	_, err = newManagedClient(validConfig(), &fakeFactory{writer: failedWriter, writerErr: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("newManagedClient() error = %v, want sentinel", err)
	}
	failedWriter.mu.Lock()
	closes := failedWriter.closeCount
	failedWriter.mu.Unlock()
	if closes != 1 {
		t.Fatalf("failed writer close count = %d, want 1", closes)
	}
}

func TestRegisterHandlerValidatesAndFreezesAtStart(t *testing.T) {
	client := managedClient(t, validConfig(), &fakeFactory{writer: &fakeWriter{}})
	if err := client.RegisterHandler("", HandlerFunc(func(context.Context, Message) error { return nil })); err == nil {
		t.Fatal("RegisterHandler(empty) error = nil")
	}
	var nilHandler *pointerHandler
	if err := client.RegisterHandler("nil", nilHandler); err == nil {
		t.Fatal("RegisterHandler(typed nil) error = nil")
	}
	if err := client.RegisterHandler("unused", HandlerFunc(func(context.Context, Message) error { return nil })); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "default")); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if err := client.RegisterHandler("late", HandlerFunc(func(context.Context, Message) error { return nil })); !errors.Is(err, ErrHandlersFrozen) {
		t.Fatalf("late RegisterHandler error = %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type pointerHandler struct{}

func (*pointerHandler) Handle(context.Context, Message) error { return nil }

func TestStartRollsBackReadersAndUnfreezesHandlers(t *testing.T) {
	firstReader := newFakeReader()
	factory := &fakeFactory{
		writer:    &fakeWriter{},
		readers:   map[string]messageReader{"a": firstReader},
		readerErr: map[string]error{"b": errors.New("join failed")},
	}
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"b": consumerConfig(ErrorPolicyStop), "a": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, factory)
	for _, name := range []string{"a", "b"} {
		if err := client.RegisterHandler(name, HandlerFunc(func(context.Context, Message) error { return nil })); err != nil {
			t.Fatal(err)
		}
	}
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "default")); err == nil || !errors.Is(err, factory.readerErr["b"]) {
		t.Fatalf("start() error = %v", err)
	}
	firstReader.mu.Lock()
	closeCount := firstReader.closeCount
	firstReader.mu.Unlock()
	if closeCount != 1 || host.taskCount() != 0 {
		t.Fatalf("rollback close count = %d, tasks = %d", closeCount, host.taskCount())
	}
	factory.mu.Lock()
	order := append([]string(nil), factory.readerOrder...)
	factory.mu.Unlock()
	if !reflect.DeepEqual(order, []string{"a", "b"}) {
		t.Fatalf("reader order = %v", order)
	}
	if err := client.RegisterHandler("after-failure", HandlerFunc(func(context.Context, Message) error { return nil })); err != nil {
		t.Fatalf("handlers remained frozen after failed start: %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartRejectsMissingHandlerBeforeSubmittingTasks(t *testing.T) {
	reader := newFakeReader()
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, &fakeFactory{
		writer: &fakeWriter{}, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{},
	})
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "default")); err == nil {
		t.Fatal("start without handler error = nil")
	}
	if host.taskCount() != 0 {
		t.Fatalf("start submitted %d tasks before validation", host.taskCount())
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartHandlesFirstAndPartialTaskAdmissionRejection(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		want  int
	}{{"first", 0, 0}, {"partial", 1, 1}} {
		t.Run(test.name, func(t *testing.T) {
			readers := map[string]messageReader{"a": newFakeReader(), "b": newFakeReader()}
			cfg := validConfig()
			cfg.Consumers = map[string]ConsumerConfig{"a": consumerConfig(ErrorPolicyStop), "b": consumerConfig(ErrorPolicyStop)}
			client := managedClient(t, cfg, &fakeFactory{writer: &fakeWriter{}, readers: readers, readerErr: map[string]error{}})
			for _, name := range []string{"a", "b"} {
				if err := client.RegisterHandler(name, HandlerFunc(func(context.Context, Message) error { return nil })); err != nil {
					t.Fatal(err)
				}
			}
			host := newFakeHost()
			host.setAdmissionLimit(test.limit)
			if err := client.start(runtimeContext(host, "default")); err == nil {
				t.Fatal("start() error = nil")
			}
			count, critical := host.taskCounts()
			if count != test.want || critical != test.want {
				t.Fatalf("accepted tasks = %d/%d, want %d/%d", count, critical, test.want, test.want)
			}
			for name, raw := range readers {
				r := raw.(*fakeReader)
				r.mu.Lock()
				closes := r.closeCount
				r.mu.Unlock()
				if closes != 1 {
					t.Fatalf("reader %s close count = %d", name, closes)
				}
			}
			if err := client.stop(context.Background()); err != nil {
				t.Fatalf("stop after rejected start error = %v", err)
			}
			if err := client.stop(context.Background()); err != nil {
				t.Fatalf("repeated stop after rejected start error = %v", err)
			}
			host.close()
		})
	}
}

func TestConsumerWaitsForTrafficGateThenCommitsAfterHandler(t *testing.T) {
	reader := newFakeReader()
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, &fakeFactory{writer: &fakeWriter{}, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{}})
	handled := make(chan Message, 1)
	if err := client.RegisterHandler("jobs", HandlerFunc(func(_ context.Context, message Message) error {
		handled <- message
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "analytics")); err != nil {
		t.Fatal(err)
	}
	if count, critical := host.taskCounts(); count != 1 || critical != 1 {
		t.Fatalf("managed tasks = %d/%d, want 1/1", count, critical)
	}
	reader.enqueue(Message{Topic: "jobs", Offset: 7, Value: []byte("work")})
	select {
	case <-handled:
		t.Fatal("handler ran before traffic gate")
	case <-time.After(50 * time.Millisecond):
	}
	host.openTraffic()
	select {
	case message := <-handled:
		if message.Offset != 7 {
			t.Fatalf("handled message = %+v", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not run after traffic gate")
	}
	waitFor(t, func() bool {
		reader.mu.Lock()
		defer reader.mu.Unlock()
		return reader.commitCalls == 1
	}, "offset commit")
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerAndCommitRetries(t *testing.T) {
	reader := newFakeReader()
	reader.commitFailures = 1
	reader.commitErr = errors.New("temporary commit")
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, &fakeFactory{writer: &fakeWriter{}, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{}})
	var calls atomic.Int32
	if err := client.RegisterHandler("jobs", HandlerFunc(func(context.Context, Message) error {
		if calls.Add(1) == 1 {
			return errors.New("retry")
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "default")); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	reader.enqueue(Message{Offset: 1})
	waitFor(t, func() bool {
		reader.mu.Lock()
		defer reader.mu.Unlock()
		return reader.commitCalls == 2
	}, "handler and commit retries")
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d, want 2", calls.Load())
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestErrorPoliciesControlCommitAndConsumerReturn(t *testing.T) {
	for _, test := range []struct {
		policy      string
		wantCommits int
	}{
		{ErrorPolicySkip, 1},
		{ErrorPolicyStop, 0},
	} {
		t.Run(test.policy, func(t *testing.T) {
			reader := newFakeReader()
			cfg := validConfig()
			consumer := consumerConfig(test.policy)
			consumer.HandlerMaxAttempts = 2
			cfg.Consumers = map[string]ConsumerConfig{"jobs": consumer}
			client := managedClient(t, cfg, &fakeFactory{writer: &fakeWriter{}, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{}})
			var calls atomic.Int32
			if err := client.RegisterHandler("jobs", HandlerFunc(func(context.Context, Message) error {
				calls.Add(1)
				return errors.New("failed")
			})); err != nil {
				t.Fatal(err)
			}
			host := newFakeHost()
			if err := client.start(runtimeContext(host, "default")); err != nil {
				t.Fatal(err)
			}
			host.openTraffic()
			reader.enqueue(Message{Offset: 3})
			waitFor(t, func() bool { return calls.Load() == 2 }, "handler attempts")
			if test.wantCommits > 0 {
				waitFor(t, func() bool {
					reader.mu.Lock()
					defer reader.mu.Unlock()
					return reader.commitCalls == test.wantCommits
				}, "skip-policy commit")
			} else {
				time.Sleep(20 * time.Millisecond)
				reader.mu.Lock()
				commits := reader.commitCalls
				reader.mu.Unlock()
				if commits != 0 {
					t.Fatalf("commit calls = %d, want 0", commits)
				}
			}
			if err := client.stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			host.close()
		})
	}
}

func TestConcurrentStopCancelsHandlerAndSharesCloseError(t *testing.T) {
	reader := newFakeReader()
	closeErr := errors.New("flush failed")
	closeRelease := make(chan struct{})
	closeStarted := make(chan struct{})
	writer := &fakeWriter{closeErr: closeErr, closeBlock: closeRelease, closeStarted: closeStarted}
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, &fakeFactory{writer: writer, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{}})
	handlerStarted := make(chan struct{})
	var once sync.Once
	if err := client.RegisterHandler("jobs", HandlerFunc(func(ctx context.Context, _ Message) error {
		once.Do(func() { close(handlerStarted) })
		<-ctx.Done()
		return ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "default")); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	reader.enqueue(Message{Offset: 1})
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called")
	}
	results := make(chan error, 2)
	go func() { results <- client.stop(context.Background()) }()
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("producer close did not start")
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.stop(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting stop error = %v, want deadline", err)
	}
	go func() { results <- client.stop(context.Background()) }()
	close(closeRelease)
	for range 2 {
		if err := <-results; !errors.Is(err, closeErr) {
			t.Fatalf("stop() error = %v, want shared close error", err)
		}
	}
	if err := client.stop(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("repeated stop error = %v", err)
	}
	reader.mu.Lock()
	readerCloses := reader.closeCount
	reader.mu.Unlock()
	writer.mu.Lock()
	writerCloses := writer.closeCount
	writer.mu.Unlock()
	if readerCloses != 1 || writerCloses != 1 {
		t.Fatalf("reader closes = %d, writer closes = %d", readerCloses, writerCloses)
	}
}

func TestStopBeforeStartIsSafeAndIdempotent(t *testing.T) {
	writer := &fakeWriter{}
	client := managedClient(t, validConfig(), &fakeFactory{writer: writer})
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop before start error = %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("repeated stop error = %v", err)
	}
	writer.mu.Lock()
	closes := writer.closeCount
	writer.mu.Unlock()
	if closes != 1 {
		t.Fatalf("writer close calls = %d, want 1", closes)
	}
}
