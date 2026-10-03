package async

import (
	"fmt"
	"strings"
	"time"
)

// ExecutorGoroutine runs every task on its own goroutine. It is the default
// and requires no further configuration.
const ExecutorGoroutine = "goroutine"

// ExecutorAnts runs tasks on a fixed-size, goroutine-reusing pool backed by
// github.com/panjf2000/ants/v2, sized to MaxConcurrency. See doc.go's
// "# Executors" section for when to prefer it over ExecutorGoroutine.
const ExecutorAnts = "ants"

// Config is bound from plugins.async.
type Config struct {
	// Executor selects the task-running strategy: ExecutorGoroutine (default)
	// or ExecutorAnts.
	Executor string `yaml:"executor" default:"goroutine"`
	// MaxConcurrency bounds how many tasks run at once. Zero means unlimited
	// for ExecutorGoroutine. ExecutorAnts has no unlimited mode: it requires
	// MaxConcurrency > 0, and that value is the ants pool's fixed size.
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
	// Ants configures the ants pool. It is only meaningful when Executor is
	// ExecutorAnts; see validate and this field's own doc comment for why a
	// non-default value is rejected under ExecutorGoroutine instead of being
	// silently ignored.
	Ants AntsConfig `yaml:"ants"`
}

// AntsConfig configures the ants pool used by ExecutorAnts. Every field here
// is read only when Config.Executor is ExecutorAnts.
//
// The configuration layer binds this struct from YAML/env/defaults without
// recording which fields the deployment actually wrote versus which were
// left at their zero value, so prepareConfig cannot distinguish "explicitly
// set to the default" from "never set" the way runtime/settings.go's
// env.Exists check does for xbc.drain_timeout (that check runs against the
// raw Environment before Bind; a plugin's ConfigSpec.Prepare only ever sees
// the already-bound Config value, never the Environment it came from).
// prepareConfig therefore accepts a non-default AntsConfig field only when
// Executor is ExecutorAnts, and rejects one under ExecutorGoroutine -- it
// cannot tell a deployment that wrote ants.pre_alloc: false on purpose apart
// from one that never mentioned the ants section at all, so it never treats
// setting an ants field while on the goroutine executor as silently ignorable.
type AntsConfig struct {
	// ExpiryDuration is how often ants' scavenger scans for idle workers to
	// retire. It matches ants' own DefaultCleanIntervalTime (1s): see
	// ants.go's DefaultCleanIntervalTime constant. Must not be negative.
	ExpiryDuration time.Duration `yaml:"expiry_duration" default:"1s"`
	// PreAlloc preallocates the pool's worker storage at creation time
	// instead of growing it lazily. Matches ants' own default (false).
	PreAlloc bool `yaml:"pre_alloc" default:"false"`
	// DisablePurge turns off the scavenger entirely, keeping every worker
	// resident instead of retiring idle ones after ExpiryDuration. Matches
	// ants' own default (false).
	DisablePurge bool `yaml:"disable_purge" default:"false"`
}

// defaultAntsConfig returns AntsConfig's zero-deployment value: ants' own
// defaults, matched field for field against DefaultCleanIntervalTime,
// PreAlloc, and DisablePurge in the vendored ants source.
func defaultAntsConfig() AntsConfig {
	return AntsConfig{
		ExpiryDuration: time.Second,
		PreAlloc:       false,
		DisablePurge:   false,
	}
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
		Ants: defaultAntsConfig(),
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
	switch c.Executor {
	case ExecutorGoroutine, ExecutorAnts:
	default:
		return fmt.Errorf("async: unsupported executor %q, valid values: %s, %s", c.Executor, ExecutorGoroutine, ExecutorAnts)
	}
	if c.MaxConcurrency < 0 {
		return fmt.Errorf("async: max_concurrency must not be negative")
	}
	if c.Executor == ExecutorAnts && c.MaxConcurrency == 0 {
		return fmt.Errorf("async: executor %q requires a positive max_concurrency; the ants pool size equals max_concurrency and ants has no unlimited mode", ExecutorAnts)
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
	if c.Ants.ExpiryDuration < 0 {
		return fmt.Errorf("async: ants.expiry_duration must not be negative")
	}
	if c.Executor != ExecutorAnts && c.Ants != defaultAntsConfig() {
		return fmt.Errorf("async: ants settings (ants.expiry_duration, ants.pre_alloc, ants.disable_purge) are only meaningful when executor is %q; set executor: %s or remove the ants section", ExecutorAnts, ExecutorAnts)
	}
	return nil
}
