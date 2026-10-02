package kafka

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDrainFinishesTheFetchedMessageAndStopsFetching pins the graceful half of
// shutdown: the message already fetched is handled under a live context and
// committed, no further message is fetched, and production stays open until
// stop.
func TestDrainFinishesTheFetchedMessageAndStopsFetching(t *testing.T) {
	reader := newFakeReader()
	writer := &fakeWriter{}
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, &fakeFactory{writer: writer, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{}})
	started := make(chan struct{})
	release := make(chan struct{})
	handlerErr := make(chan error, 1)
	if err := client.RegisterHandler("jobs", HandlerFunc(func(ctx context.Context, _ Message) error {
		close(started)
		<-release
		handlerErr <- ctx.Err()
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
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called")
	}

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain() error = %v, want deadline while the handler runs", err)
	}
	reader.enqueue(Message{Offset: 2})
	close(release)
	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if err := <-handlerErr; err != nil {
		t.Fatalf("drain cancelled the handler's context: %v", err)
	}
	reader.mu.Lock()
	commits, fetches := len(reader.commits), reader.fetchCalls
	reader.mu.Unlock()
	if commits != 1 || fetches != 1 {
		t.Fatalf("commits = %d fetches = %d, want the held message committed and nothing more fetched", commits, fetches)
	}
	if err := client.Produce(context.Background(), Message{Topic: "jobs"}); err != nil {
		t.Fatalf("Produce after drain error = %v, want production open until Stop", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
}

// TestDrainBeforeTrafficOrStartReturns covers the abort path: a consumer still
// waiting for the traffic gate exits on drain, and a client drained before
// Start refuses to start.
func TestDrainBeforeTrafficOrStartReturns(t *testing.T) {
	reader := newFakeReader()
	cfg := validConfig()
	cfg.Consumers = map[string]ConsumerConfig{"jobs": consumerConfig(ErrorPolicyStop)}
	client := managedClient(t, cfg, &fakeFactory{writer: &fakeWriter{}, readers: map[string]messageReader{"jobs": reader}, readerErr: map[string]error{}})
	if err := client.RegisterHandler("jobs", HandlerFunc(func(context.Context, Message) error { return nil })); err != nil {
		t.Fatal(err)
	}
	host := newFakeHost()
	defer host.close()
	if err := client.start(runtimeContext(host, "default")); err != nil {
		t.Fatal(err)
	}
	if err := client.drain(context.Background()); err != nil {
		t.Fatalf("drain() with the gate still closed error = %v", err)
	}
	if err := client.stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	unstarted := managedClient(t, validConfig(), &fakeFactory{writer: &fakeWriter{}})
	if err := unstarted.drain(context.Background()); err != nil {
		t.Fatalf("drain() before Start error = %v", err)
	}
	if err := unstarted.start(runtimeContext(host, "default")); err == nil {
		t.Fatal("start() after drain error = nil")
	}
	if err := unstarted.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
