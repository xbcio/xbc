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

	prepared, err := prepareConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, cfg, prepared)
}

func TestPrepareConfigRejectsUnsupportedExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Executor = "ants"
	_, err := prepareConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goroutine")
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
