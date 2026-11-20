package mailx

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

func TestHealthChecksAreGatedOnVerify(t *testing.T) {
	server := newFakeSMTPServer(t)
	disabled := bindConfig(t, serverValues(t, server))
	client, err := buildClient(disabled, "default")
	if err != nil {
		t.Fatalf("buildClient(verify=false) error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })
	if checks := client.HealthChecks(); len(checks) != 0 {
		t.Fatalf("verify=false contributed %d checks, want none", len(checks))
	}

	values := serverValues(t, server)
	values["verify"] = true
	enabled := bindConfig(t, values)
	probed, err := buildClient(enabled, "default")
	if err != nil {
		t.Fatalf("buildClient(verify=true) error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(probed, context.Background()) })
	checks := probed.HealthChecks()
	if len(checks) != 1 {
		t.Fatalf("verify=true contributed %d checks, want 1", len(checks))
	}
	if checks[0].Kind != health.Readiness {
		t.Fatalf("check kind = %q, want readiness", checks[0].Kind)
	}
	if checks[0].Name != "" {
		t.Fatalf("check name = %q, want an unqualified name the health owner qualifies", checks[0].Name)
	}
}

func TestHealthCheckReportsAReachableServerUp(t *testing.T) {
	server := newFakeSMTPServer(t)
	values := serverValues(t, server)
	values["verify"] = true
	config := bindConfig(t, values)
	client, err := buildClient(config, "alerts")
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	t.Cleanup(func() { _ = stopClient(client, context.Background()) })

	report := health.Check(context.Background(), health.Readiness, client.HealthChecks(), time.Second)
	if !report.Healthy() {
		t.Fatalf("reachable server reported down: %+v", report)
	}
}

func TestHealthCheckReportsAStoppedClientDown(t *testing.T) {
	server := newFakeSMTPServer(t)
	values := serverValues(t, server)
	values["verify"] = true
	config := bindConfig(t, values)
	client, err := buildClient(config, "alerts")
	if err != nil {
		t.Fatalf("buildClient() error = %v", err)
	}
	if err := stopClient(client, context.Background()); err != nil {
		t.Fatalf("stopClient() error = %v", err)
	}

	checks := client.HealthChecks()
	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	if report.Healthy() {
		t.Fatalf("stopped client reported up: %+v", report)
	}
	if !errors.Is(report.Checks[0].Error, ErrClosed) {
		t.Fatalf("probe error = %v, want ErrClosed", report.Checks[0].Error)
	}
}

func TestHealthCheckReportsAnUnreachableServerDown(t *testing.T) {
	host, port := closedAddress(t)
	client := &Client{verify: true, address: net.JoinHostPort(host, strconv.Itoa(port))}
	report := health.Check(context.Background(), health.Readiness, client.HealthChecks(), time.Second)
	if report.Healthy() {
		t.Fatalf("unreachable server reported up: %+v", report)
	}
}
