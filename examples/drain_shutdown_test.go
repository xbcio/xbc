package examples_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/extensions/jobs/cron"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
)

// drainProbeJob is one cron job that reports when it starts and then holds its
// invocation until the test releases it, or until its context is cancelled.
type drainProbeJob struct {
	started  chan struct{}
	release  chan struct{}
	finished chan error
}

func newDrainProbeJob() *drainProbeJob {
	return &drainProbeJob{
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		finished: make(chan error, 1),
	}
}

func (*drainProbeJob) Name() string { return "drain_probe" }
func (*drainProbeJob) Spec() string { return "@every 1h" }

func (job *drainProbeJob) Run(ctx context.Context) error {
	select {
	case job.started <- struct{}{}:
	default:
	}
	var err error
	select {
	case <-job.release:
	case <-ctx.Done():
		err = ctx.Err()
	}
	select {
	case job.finished <- err:
	default:
	}
	return err
}

func (job *drainProbeJob) Jobs() []cron.Job { return []cron.Job{job} }

func drainProbeBundle(job *drainProbeJob) plugin.Bundle {
	return plugin.BundleOf(plugin.Define("drain-probe-jobs",
		func(plugin.BuildContext) (*drainProbeJob, error) { return job, nil },
		plugin.Options[*drainProbeJob]{Exports: plugin.Contracts(
			plugin.ExportAs[cron.JobContributor](func(job *drainProbeJob) cron.JobContributor { return job }),
		)},
	))
}

type drainExecuteResult struct {
	code int
	err  error
}

// startDrainApp runs a Web server beside cron through the public Execute path,
// waits until the probe job is running, and returns the Web address, the
// cancel that begins shutdown, and Execute's result channel.
func startDrainApp(t *testing.T, job *drainProbeJob, drain, shutdown time.Duration) (string, context.CancelFunc, <-chan drainExecuteResult) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	configPath := filepath.Join(t.TempDir(), "application.yml")
	config := fmt.Sprintf(`xbc:
  shutdown_timeout: %s
  drain_timeout: %s
  pre_stop_timeout: 0s
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
plugins:
  cron:
    run_immediately: true
`, shutdown, drain, addr)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	app, err := xbc.New(xbc.WithBundles(web.Bundle(), ginengine.Bundle(), cron.Bundle(), drainProbeBundle(job)))
	require.NoError(t, err)

	parent, cancel := context.WithCancel(context.Background())
	done := make(chan drainExecuteResult, 1)
	go func() {
		code, executeErr := app.Execute(parent, []string{"--config", configPath})
		done <- drainExecuteResult{code: code, err: executeErr}
	}()
	t.Cleanup(cancel)

	select {
	case <-job.started:
	case result := <-done:
		t.Fatalf("Execute returned before the job started: code=%d error=%v", result.code, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the cron job to start")
	}
	return addr, cancel, done
}

// webListenerClosed reports whether nothing accepts connections at addr any
// more, polling briefly because the ingress stop is asynchronous to the cancel.
func webListenerClosed(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestShutdownDrainsAcceptedJobAfterIngressStops proves the shutdown order from
// the public composition path: the Web listener is already closed while a cron
// invocation accepted before shutdown is still running under a live context,
// and that invocation finishes normally before Execute returns.
func TestShutdownDrainsAcceptedJobAfterIngressStops(t *testing.T) {
	job := newDrainProbeJob()
	addr, cancel, done := startDrainApp(t, job, 3*time.Second, 5*time.Second)

	client := &http.Client{Timeout: 250 * time.Millisecond}
	response, err := client.Get("http://" + addr + "/")
	require.NoError(t, err, "Web server must be serving before shutdown")
	require.NoError(t, response.Body.Close())

	cancel()
	require.True(t, webListenerClosed(addr, 2*time.Second), "ingress must be stopped before the drain phase waits on cron")
	select {
	case err := <-job.finished:
		t.Fatalf("job ended during drain with %v; drain must not cancel accepted work", err)
	case result := <-done:
		t.Fatalf("Execute returned while the job was still running: code=%d error=%v", result.code, result.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(job.release)
	select {
	case err := <-job.finished:
		assert.NoError(t, err, "job must finish normally under a live context")
	case <-time.After(2 * time.Second):
		t.Fatal("job did not finish after release")
	}
	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Equal(t, 0, result.code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Execute after the job finished")
	}
}

// TestShutdownStopCancelsJobThatOutlivesTheDrainBudget proves the other half:
// once xbc.drain_timeout expires, Stop cancels the job that is still running,
// and the process still exits within the shutdown budget.
func TestShutdownStopCancelsJobThatOutlivesTheDrainBudget(t *testing.T) {
	const (
		drainTimeout    = 200 * time.Millisecond
		shutdownTimeout = 3 * time.Second
	)
	job := newDrainProbeJob()
	_, cancel, done := startDrainApp(t, job, drainTimeout, shutdownTimeout)

	cancelledAt := time.Now()
	cancel()
	select {
	case err := <-job.finished:
		assert.ErrorIs(t, err, context.Canceled, "Stop must cancel the job the drain left running")
		assert.GreaterOrEqual(t, time.Since(cancelledAt), drainTimeout, "the job must not be cancelled before the drain budget expires")
	case <-time.After(shutdownTimeout):
		t.Fatal("job was not cancelled after the drain budget expired")
	}
	select {
	case <-done:
		// The expired drain is reported, not fatal; only the exit is asserted.
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("timed out waiting for Execute after Stop cancelled the job")
	}
}
