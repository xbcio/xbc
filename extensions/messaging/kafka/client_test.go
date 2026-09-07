package kafka

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestClientProduceMapsMessagesAndErrors(t *testing.T) {
	writer := &fakeWriter{}
	client := newClient(writer)
	message := Message{Topic: "events", Key: []byte("key"), Value: []byte("value"), Headers: []Header{{Key: "trace", Value: []byte("abc")}}, Time: time.Unix(10, 0)}
	if err := client.Produce(context.Background(), message); err != nil {
		t.Fatalf("Produce() error = %v", err)
	}
	writer.mu.Lock()
	if len(writer.writes) != 1 || len(writer.writes[0]) != 1 {
		t.Fatalf("writes = %#v", writer.writes)
	}
	got := writer.writes[0][0]
	writer.mu.Unlock()
	if got.Topic != message.Topic || string(got.Key) != "key" || string(got.Value) != "value" || got.Headers[0].Key != "trace" || !got.Time.Equal(message.Time) {
		t.Fatalf("written message = %+v, want %+v", got, message)
	}
	if err := client.Produce(nil, message); err == nil {
		t.Fatal("Produce(nil) error = nil")
	}
	if err := client.Produce(context.Background()); err != nil {
		t.Fatalf("Produce(empty) error = %v", err)
	}

	sentinel := errors.New("broker unavailable")
	writer.writeErr = sentinel
	if err := client.Produce(context.Background(), message); !errors.Is(err, sentinel) {
		t.Fatalf("Produce() error = %v, want wrapped sentinel", err)
	}
}

func TestClientAllowsConcurrentProduceAndSerializesClose(t *testing.T) {
	const calls = 12
	release := make(chan struct{})
	writer := &fakeWriter{writeBlock: release}
	client := newClient(writer)
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(offset int64) {
			defer wg.Done()
			errs <- client.Produce(context.Background(), Message{Topic: "events", Offset: offset})
		}(int64(i))
	}
	waitFor(t, func() bool {
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return writer.active == calls
	}, "all concurrent producer calls to enter the writer")

	closed := make(chan error, 1)
	go func() { closed <- client.close() }()
	select {
	case err := <-closed:
		t.Fatalf("close returned before admitted writes: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Produce() error = %v", err)
		}
	}
	if err := <-closed; err != nil {
		t.Fatalf("close() error = %v", err)
	}
	writer.mu.Lock()
	maxActive, closeCount := writer.maxActive, writer.closeCount
	writer.mu.Unlock()
	if maxActive != calls || closeCount != 1 {
		t.Fatalf("max active = %d, close count = %d", maxActive, closeCount)
	}
	if err := client.Produce(context.Background(), Message{Topic: "events"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Produce() after close error = %v, want ErrClosed", err)
	}
	if err := client.close(); err != nil {
		t.Fatalf("second close() error = %v", err)
	}
}
