package async

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigMatchesDocumentedDefaults(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, ExecutorGoroutine, cfg.Executor)
	assert.Equal(t, 256, cfg.MaxConcurrency)
	assert.Equal(t, 1024, cfg.QueueCapacity)
	assert.Equal(t, time.Duration(0), cfg.SubmitTimeout)
	assert.True(t, cfg.Shutdown.AwaitTermination)
	assert.Equal(t, time.Duration(0), cfg.Shutdown.AwaitTerminationPeriod)
	assert.Equal(t, defaultAntsConfig(), cfg.Ants)

	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, cfg, prepared)
}

func TestPrepareConfigRejectsUnsupportedExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = "bogus"
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goroutine")
	assert.Contains(t, err.Error(), "ants")
}

func TestPrepareConfigNormalizesExecutorCase(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = "  Goroutine  "
	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, ExecutorGoroutine, prepared.Executor)
}

func TestPrepareConfigRejectsNegativeMaxConcurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = -1
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_concurrency")
}

func TestPrepareConfigRejectsNegativeQueueCapacity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.QueueCapacity = -1
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "queue_capacity")
}

func TestPrepareConfigRejectsQueueCapacityWithoutBoundedConcurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 0
	cfg.QueueCapacity = 10
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "queue_capacity")
}

func TestPrepareConfigAcceptsZeroQueueCapacityWithUnlimitedConcurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrency = 0
	cfg.QueueCapacity = 0
	_, err := prepareConfig(cfg)
	require.NoError(t, err)
}

func TestPrepareConfigRejectsNegativeSubmitTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SubmitTimeout = -time.Second
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "submit_timeout")
}

func TestPrepareConfigRejectsNegativeAwaitTerminationPeriod(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Shutdown.AwaitTerminationPeriod = -time.Second
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "await_termination_period")
}

func TestPrepareConfigRejectsAwaitTerminationPeriodWithoutAwaitTermination(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Shutdown.AwaitTermination = false
	cfg.Shutdown.AwaitTerminationPeriod = time.Second
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "await_termination_period")
}

func TestPrepareConfigAcceptsAwaitTerminationFalseWithZeroPeriod(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Shutdown.AwaitTermination = false
	cfg.Shutdown.AwaitTerminationPeriod = 0
	_, err := prepareConfig(cfg)
	require.NoError(t, err)
}

func TestPrepareConfigAcceptsAntsExecutorWithPositiveMaxConcurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 64
	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, ExecutorAnts, prepared.Executor)
}

func TestPrepareConfigNormalizesAntsExecutorCase(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = "  Ants  "
	cfg.MaxConcurrency = 1
	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, ExecutorAnts, prepared.Executor)
}

func TestPrepareConfigRejectsAntsExecutorWithZeroMaxConcurrency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 0
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_concurrency")
}

func TestPrepareConfigRejectsNegativeAntsExpiryDuration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorAnts
	cfg.MaxConcurrency = 1
	cfg.Ants.ExpiryDuration = -time.Second
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ants.expiry_duration")
}

func TestPrepareConfigRejectsNonDefaultAntsFieldUnderGoroutineExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorGoroutine
	cfg.Ants.PreAlloc = true
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ants")
	assert.Contains(t, err.Error(), ExecutorAnts)
}

func TestPrepareConfigRejectsNonDefaultAntsExpiryUnderGoroutineExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorGoroutine
	cfg.Ants.ExpiryDuration = 5 * time.Second
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ants")
}

func TestPrepareConfigRejectsNonDefaultAntsDisablePurgeUnderGoroutineExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorGoroutine
	cfg.Ants.DisablePurge = true
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ants")
}

func TestPrepareConfigAcceptsDefaultAntsConfigUnderGoroutineExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = ExecutorGoroutine
	// Ants left at its default (zero-deployment) value: must not be rejected
	// even though the Ants section is "present" in the sense that it was
	// bound at all -- the whole point is that prepareConfig cannot tell the
	// two apart, so it only rejects a value that actually differs.
	_, err := prepareConfig(cfg)
	require.NoError(t, err)
}

func TestDefaultAntsConfigMatchesAntsOwnDefaults(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, time.Second, cfg.Ants.ExpiryDuration, "must match ants.DefaultCleanIntervalTime")
	assert.False(t, cfg.Ants.PreAlloc)
	assert.False(t, cfg.Ants.DisablePurge)
}
