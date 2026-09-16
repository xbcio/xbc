package kafka

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

func TestHealthChecksContributeOneReadinessCheck(t *testing.T) {
	writer := &fakeWriter{}
	client := newClient(writer)

	checks := client.HealthChecks()
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(checks))
	}
	if checks[0].Name != "" {
		t.Fatalf("the check must be named by the contributing instance, got %q", checks[0].Name)
	}
	if checks[0].Kind != health.Readiness {
		t.Fatalf("got kind %q, want readiness", checks[0].Kind)
	}
	if checks[0].Timeout != 0 {
		t.Fatalf("the check must inherit the plugins.health timeout, got %s", checks[0].Timeout)
	}

	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	if !report.Healthy() {
		t.Fatalf("a reachable cluster must report up, got %+v", report)
	}
	if got := writer.pings(); got != 1 {
		t.Fatalf("got %d pings, want 1", got)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("the probe must not produce a message, got %+v", writer.writes)
	}
}

func TestHealthCheckReportsDownForAnUnreachableCluster(t *testing.T) {
	writer := &fakeWriter{pingErr: errors.New("no broker answered")}
	client := newClient(writer)

	report := health.Check(context.Background(), health.Readiness, client.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("an unreachable cluster must report down, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Error == nil {
		t.Fatalf("expected one failed check, got %+v", report.Checks)
	}
}

// The probe is admitted exactly as Produce is, so a client that shutdown already
// closed reports down instead of touching a writer it no longer owns.
func TestHealthCheckReportsDownForAClosedClient(t *testing.T) {
	client := newClient(&fakeWriter{})
	if err := client.close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	report := health.Check(context.Background(), health.Readiness, client.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a closed client must report down, got %+v", report)
	}
	if err := report.Checks[0].Error; !errors.Is(err, ErrClosed) {
		t.Fatalf("got error %v, want ErrClosed", err)
	}
}

// TestWriterPingReadsClusterMetadata covers the real adapter against a listener
// that accepts a connection but never answers, which is the failure a probe built
// on local writer statistics would miss entirely.
func TestWriterPingReadsClusterMetadata(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		accepted <- connection
	}()

	adapter := &writerAdapter{
		dialer:  &kafkago.Dialer{Timeout: time.Second},
		brokers: []string{listener.Addr().String()},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := adapter.Ping(ctx); err == nil {
		t.Fatal("a broker that never answers metadata must not report reachable")
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(time.Second):
		t.Fatal("the probe never dialed the broker")
	}
}

func TestWriterPingRequiresBrokers(t *testing.T) {
	adapter := &writerAdapter{dialer: &kafkago.Dialer{Timeout: time.Second}}
	if err := adapter.Ping(context.Background()); err == nil {
		t.Fatal("a writer with no brokers must not report reachable")
	}
}
