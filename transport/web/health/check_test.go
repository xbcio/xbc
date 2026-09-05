package health

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckRunsConcurrentlyAndSortsResults(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 2)
	var active atomic.Int32
	checker := func(name string) NamedChecker {
		return NamedChecker{Name: name, Kind: Readiness, Checker: CheckFunc(func(context.Context) error {
			active.Add(1)
			defer active.Add(-1)
			entered <- name
			<-release
			return nil
		})}
	}

	done := make(chan Report, 1)
	go func() {
		done <- Check(context.Background(), Readiness, []NamedChecker{checker("zeta"), checker("alpha")}, time.Second)
	}()

	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("checks did not enter concurrently")
		}
	}
	assert.Equal(t, int32(2), active.Load())
	close(release)

	report := <-done
	assert.Equal(t, Up, report.Status)
	require.Len(t, report.Checks, 2)
	assert.Equal(t, []string{"alpha", "zeta"}, []string{report.Checks[0].Name, report.Checks[1].Name})
	assert.Equal(t, Up, report.Checks[0].Status)
}

func TestCheckAppliesPerItemTimeoutWithoutWaitingForStuckChecker(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	started := time.Now()
	report := Check(context.Background(), Readiness, []NamedChecker{
		{Name: "stuck", Kind: Readiness, Timeout: 20 * time.Millisecond, Checker: CheckFunc(func(context.Context) error {
			<-blocked
			return nil
		})},
		{Name: "fast", Kind: Readiness, Checker: CheckFunc(func(context.Context) error { return nil })},
	}, time.Second)

	assert.Less(t, time.Since(started), 500*time.Millisecond)
	assert.Equal(t, Down, report.Status)
	require.Len(t, report.Checks, 2)
	assert.Equal(t, "fast", report.Checks[0].Name)
	assert.Equal(t, Up, report.Checks[0].Status)
	assert.Equal(t, "stuck", report.Checks[1].Name)
	assert.Equal(t, Down, report.Checks[1].Status)
	assert.ErrorIs(t, report.Checks[1].Error, context.DeadlineExceeded)
}

func TestCheckRecoversPanicsAndAggregatesDown(t *testing.T) {
	report := Check(context.Background(), Liveness, []NamedChecker{
		{Name: "panic", Kind: Liveness, Checker: CheckFunc(func(context.Context) error { panic("boom") })},
		{Name: "up", Kind: Liveness, Checker: CheckFunc(func(context.Context) error { return nil })},
	}, time.Second)

	assert.Equal(t, Down, report.Status)
	require.Len(t, report.Checks, 2)
	assert.Contains(t, report.Checks[0].Error.Error(), "panicked")
	assert.Contains(t, report.Checks[0].Error.Error(), "boom")
	assert.Equal(t, Up, report.Checks[1].Status)
}

func TestCheckSeparatesProbeKindsAndDefaultsUnspecifiedToReadiness(t *testing.T) {
	boom := errors.New("not ready")
	checkers := []NamedChecker{
		{Name: "live", Kind: Liveness, Checker: CheckFunc(func(context.Context) error { return nil })},
		{Name: "ready", Checker: CheckFunc(func(context.Context) error { return boom })},
	}

	live := Check(context.Background(), Liveness, checkers, time.Second)
	assert.Equal(t, Up, live.Status)
	require.Len(t, live.Checks, 1)
	assert.Equal(t, "live", live.Checks[0].Name)

	ready := Check(context.Background(), Readiness, checkers, time.Second)
	assert.Equal(t, Down, ready.Status)
	require.Len(t, ready.Checks, 1)
	assert.Equal(t, Readiness, ready.Checks[0].Kind)
	assert.ErrorIs(t, ready.Checks[0].Error, boom)
}

func TestCheckHandlesInvalidInputsAsDownReports(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		kind    Kind
		timeout time.Duration
		checker NamedChecker
		want    string
	}{
		{name: "nil context", ctx: nil, kind: Readiness, timeout: time.Second, want: "context"},
		{name: "invalid kind", ctx: context.Background(), kind: "startup", timeout: time.Second, want: "unsupported"},
		{name: "invalid default timeout", ctx: context.Background(), kind: Readiness, timeout: 0, want: "default timeout"},
		{name: "nil checker", ctx: context.Background(), kind: Readiness, timeout: time.Second, checker: NamedChecker{Name: "nil", Kind: Readiness}, want: "nil"},
		{name: "negative item timeout", ctx: context.Background(), kind: Readiness, timeout: time.Second, checker: NamedChecker{Name: "bad", Kind: Readiness, Timeout: -1, Checker: CheckFunc(func(context.Context) error { return nil })}, want: "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var checks []NamedChecker
			if tt.checker.Name != "" {
				checks = []NamedChecker{tt.checker}
			}
			report := Check(tt.ctx, tt.kind, checks, tt.timeout)
			assert.Equal(t, Down, report.Status)
			require.NotEmpty(t, report.Checks)
			require.Error(t, report.Checks[0].Error)
			assert.Contains(t, report.Checks[0].Error.Error(), tt.want)
		})
	}
}

func TestCheckFuncNilIsSafe(t *testing.T) {
	var check CheckFunc
	err := check.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil CheckFunc")
}
