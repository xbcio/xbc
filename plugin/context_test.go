package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextGoForwardsIdentityAndNonCriticalFlag(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer", Instance: "readonly"}, nil, nil)

	var ran bool
	ctx.Go(func(context.Context) { ran = true })

	require.True(t, ran, "Go must actually call fn")
	calls := host.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, Identity{Plugin: "consumer", Instance: "readonly"}, calls[0].id)
	assert.False(t, calls[0].critical, "Go submitted tasks must have critical=false")
}

func TestContextGoCriticalForwardsCriticalTrue(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "consumer"}, nil, nil)

	ctx.GoCritical(func(context.Context) {})

	calls := host.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, Key("consumer"), calls[0].id.Plugin)
	assert.True(t, calls[0].critical, "GoCritical submitted tasks must have critical=true")
}

// TestContextTasksAcceptedTrueWhenHostHasNoReporter pins the fallback
// branch: a bare *fakeHost does not implement taskAdmissionReporter at all,
// and TasksAccepted must not panic or otherwise misbehave -- it must simply
// assume tasks are still accepted.
func TestContextTasksAcceptedTrueWhenHostHasNoReporter(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "p"}, nil, nil)
	assert.True(t, ctx.TasksAccepted())
}

func TestContextTasksAcceptedReflectsReporterFalse(t *testing.T) {
	host := &admissionHost{fakeHost: newFakeHost(), accepted: false}
	ctx := NewRuntimeContext(host, Identity{Plugin: "p"}, nil, nil)
	assert.False(t, ctx.TasksAccepted(), "when the host explicitly reports that admission is closed, it must reflect as false")
}

func TestContextTasksAcceptedReflectsReporterTrue(t *testing.T) {
	host := &admissionHost{fakeHost: newFakeHost(), accepted: true}
	ctx := NewRuntimeContext(host, Identity{Plugin: "p"}, nil, nil)
	assert.True(t, ctx.TasksAccepted())
}

func TestContextIdentityNameInstanceAgree(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "gorm", Instance: "readonly"}, nil, nil)
	assert.Equal(t, "gorm", ctx.Name())
	assert.Equal(t, "readonly", ctx.Instance())
	assert.Equal(t, Identity{Plugin: "gorm", Instance: "readonly"}, ctx.Identity())
}

// TestContextInstanceIsNeverEmpty pins that NewRuntimeContext normalizes "" to
// "default" once, at construction, so Context.Instance() never hands back
// an empty string even when the caller built the Identity with one.
func TestContextInstanceIsNeverEmpty(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "gorm"}, nil, nil)
	assert.Equal(t, "default", ctx.Instance())
}

func TestContextLogFallsBackToGlobalLoggerWhenNil(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "p"}, nil, nil)
	assert.NotPanics(t, func() { ctx.Log() })
}

func TestContextConfigReturnsWhatWasPassedIn(t *testing.T) {
	host := newFakeHost()
	ctx := NewRuntimeContext(host, Identity{Plugin: "p"}, nil, nil)
	assert.Nil(t, ctx.Config(), "Config() returns nil as-is when env is not provided, without constructing an empty environment")
}

func TestContextImplementsStandardContextAndForwardsExecutionScope(t *testing.T) {
	type keyType struct{}
	key := keyType{}
	deadline := time.Now().Add(time.Minute)
	parent, parentCancel := context.WithDeadline(
		context.WithValue(context.Background(), key, "execution-value"),
		deadline,
	)
	ctx := NewRuntimeContext(newFakeHost(), Identity{Plugin: "context-aware"}, nil, nil)
	BindLifecycleContext(ctx, parent)

	var standard context.Context = ctx
	gotDeadline, ok := standard.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, deadline, gotDeadline, time.Millisecond)
	assert.Equal(t, "execution-value", standard.Value(key))
	assert.NoError(t, standard.Err())

	parentCancel()
	select {
	case <-standard.Done():
	case <-time.After(time.Second):
		t.Fatal("plugin.Context.Done is not closed after the parent execution context is canceled")
	}
	assert.ErrorIs(t, standard.Err(), context.Canceled)
}

func TestNewRuntimeContextDoesNotReuseCancelledLifecycleState(t *testing.T) {
	host := newFakeHost()
	cancelled := NewRuntimeContext(host, Identity{Plugin: "first"}, nil, nil)
	parent, cancel := context.WithCancel(context.Background())
	BindLifecycleContext(cancelled, parent)
	cancel()
	<-cancelled.Done()
	require.ErrorIs(t, cancelled.Err(), context.Canceled)

	fresh := NewRuntimeContext(host, Identity{Plugin: "second"}, nil, nil)
	assert.NoError(t, fresh.Err(), "new Context cannot inherit the cancellation state of another Context")
	assert.Nil(t, fresh.Done(), "new Context not bound to execution context should maintain Background semantics")
}
