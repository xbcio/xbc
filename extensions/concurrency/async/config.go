package async

import (
	"fmt"
	"strings"
	"time"
)

// ExecutorGoroutine is the only executor accepted in this step. A later step
// adds an ants-backed pool; config.go's job is to name every value Prepare
// currently accepts, so adding one there is a one-line, explicit decision.
const ExecutorGoroutine = "goroutine"

// Config is bound from plugins.async.
type Config struct {
	// Executor selects the task-running strategy. Only ExecutorGoroutine is
	// accepted in this step.
	Executor string `yaml:"executor" default:"goroutine"`
	// MaxConcurrency bounds how many tasks run at once. Zero means unlimited.
	MaxConcurrency int `yaml:"max_concurrency" default:"256"`
	// QueueCapacity bounds how many tasks may wait once MaxConcurrency is
	// reached. Zero means no queue: a submission that finds every slot busy
	// waits only for the SubmitTimeout, never for queue space.
	QueueCapacity int `yaml:"queue_capacity" default:"1024"`
	// SubmitTimeout bounds how long Spawn waits for a running slot or queue
	// space once both are full. Zero rejects immediately with ErrSaturated.
	// The effective wait is min(SubmitTimeout, the Spawn ctx's own deadline).
	SubmitTimeout time.Duration  `yaml:"submit_timeout" default:"0s"`
	Shutdown      ShutdownConfig `yaml:"shutdown"`
}

// ShutdownConfig controls Drain's behavior.
type ShutdownConfig struct {
	// AwaitTermination makes Drain wait for running and queued tasks. False
	// makes Drain return immediately; Stop then cancels running tasks and
	// discards the queue.
	AwaitTermination bool `yaml:"await_termination" default:"true"`
	// AwaitTerminationPeriod further bounds Drain's wait on top of the drain
	// ctx (xbc.drain_timeout). Zero means the wait is bounded only by that
	// ctx. The effective wait is min(AwaitTerminationPeriod, the drain ctx's
	// own deadline).
	AwaitTerminationPeriod time.Duration `yaml:"await_termination_period" default:"0s"`
}

// DefaultConfig returns the same production-safe defaults applied by XBC's
// configuration binder.
func DefaultConfig() Config {
	return Config{
		Executor:       ExecutorGoroutine,
		MaxConcurrency: 256,
		QueueCapacity:  1024,
		SubmitTimeout:  0,
		Shutdown: ShutdownConfig{
			AwaitTermination:       true,
			AwaitTerminationPeriod: 0,
		},
	}
}

func prepareConfig(cfg Config) (Config, error) {
	cfg.Executor = strings.ToLower(strings.TrimSpace(cfg.Executor))
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Executor != ExecutorGoroutine {
		return fmt.Errorf("async: unsupported executor %q, valid values: %s", c.Executor, ExecutorGoroutine)
	}
	if c.MaxConcurrency < 0 {
		return fmt.Errorf("async: max_concurrency must not be negative")
	}
	if c.QueueCapacity < 0 {
		return fmt.Errorf("async: queue_capacity must not be negative")
	}
	if c.QueueCapacity > 0 && c.MaxConcurrency == 0 {
		return fmt.Errorf("async: queue_capacity requires a positive max_concurrency; max_concurrency 0 is unlimited and never queues")
	}
	if c.SubmitTimeout < 0 {
		return fmt.Errorf("async: submit_timeout must not be negative")
	}
	if c.Shutdown.AwaitTerminationPeriod < 0 {
		return fmt.Errorf("async: shutdown.await_termination_period must not be negative")
	}
	if !c.Shutdown.AwaitTermination && c.Shutdown.AwaitTerminationPeriod > 0 {
		return fmt.Errorf("async: shutdown.await_termination_period requires shutdown.await_termination to be true")
	}
	return nil
}
