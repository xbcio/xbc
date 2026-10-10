package async

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetGlobal clears any process-global binding left over from another test
// in this package, restoring it afterward if one happened to be present.
func resetGlobal(t *testing.T) {
	t.Helper()
	previous := globalPool.Load()
	globalPool.Store(nil)
	t.Cleanup(func() { globalPool.Store(previous) })
}

func TestGlobalSpawnBeforeInitReturnsErrNotInstalled(t *testing.T) {
	resetGlobal(t)
	err := Spawn(context.Background(), "x", func(context.Context) {})
	assert.ErrorIs(t, err, ErrNotInstalled)
}

func TestGlobalSpawnIsBoundAfterInit(t *testing.T) {
	resetGlobal(t)
	pool := mustNewPool(t, DefaultConfig(), nil)
	require.NoError(t, initPool(pool, runtimeContext(newFakeHost(), "")))
	t.Cleanup(func() { _ = pool.stop(context.Background()) })

	done := make(chan struct{})
	err := Spawn(context.Background(), "global-task", func(context.Context) { close(done) })
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("globally spawned task did not run")
	}
}

func TestGlobalSpawnReturnsErrNotInstalledAfterStop(t *testing.T) {
	resetGlobal(t)
	pool := mustNewPool(t, DefaultConfig(), nil)
	require.NoError(t, initPool(pool, runtimeContext(newFakeHost(), "")))
	require.NoError(t, pool.stop(context.Background()))

	err := Spawn(context.Background(), "x", func(context.Context) {})
	assert.ErrorIs(t, err, ErrNotInstalled)
}

func TestGlobalSpawnReturnsErrShuttingDownDuringDrain(t *testing.T) {
	resetGlobal(t)
	pool := mustNewPool(t, DefaultConfig(), nil)
	require.NoError(t, initPool(pool, runtimeContext(newFakeHost(), "")))
	t.Cleanup(func() { _ = pool.stop(context.Background()) })

	require.NoError(t, pool.drain(context.Background()))
	err := Spawn(context.Background(), "x", func(context.Context) {})
	assert.ErrorIs(t, err, ErrShuttingDown)
}

func TestSecondPoolDoesNotOverwriteTheGlobalBinding(t *testing.T) {
	resetGlobal(t)
	first := mustNewPool(t, DefaultConfig(), nil)
	require.NoError(t, initPool(first, runtimeContext(newFakeHost(), "")))
	t.Cleanup(func() { _ = first.stop(context.Background()) })

	second := mustNewPool(t, DefaultConfig(), nil)
	require.NoError(t, initPool(second, runtimeContext(newFakeHost(), "")))
	t.Cleanup(func() { _ = second.stop(context.Background()) })

	assert.Same(t, first, globalPool.Load())

	// Stopping the second pool (never bound) must not unbind the first.
	require.NoError(t, second.stop(context.Background()))
	assert.Same(t, first, globalPool.Load())
}

func TestUnbindGlobalOnlyClearsItsOwnBinding(t *testing.T) {
	resetGlobal(t)
	first := mustNewPool(t, DefaultConfig(), nil)
	require.NoError(t, initPool(first, runtimeContext(newFakeHost(), "")))
	// Init also installs the process-wide local task executor, which only
	// stop uninstalls; unbinding the Spawner by hand must not leave it behind.
	t.Cleanup(func() { _ = first.stop(context.Background()) })

	second := mustNewPool(t, DefaultConfig(), nil)
	// second never bound (first already holds the slot); stopping it must be
	// a no-op for the global binding.
	unbindGlobal(second)
	assert.Same(t, first, globalPool.Load())

	unbindGlobal(first)
	assert.Nil(t, globalPool.Load())
}
