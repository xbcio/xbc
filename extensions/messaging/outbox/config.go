package outbox

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xbcio/xbc/plugin"
)

// Config configures the transactional service and optional dispatcher worker.
type Config struct {
	DBInstance      string          `yaml:"db_instance" default:"default"`
	Table           string          `yaml:"table" default:"xbc_outbox_events"`
	Migrate         bool            `yaml:"migrate" default:"false"`
	MaxPayloadBytes int             `yaml:"max_payload_bytes" default:"1048576" validate:"gt=0"`
	Worker          WorkerConfig    `yaml:"worker"`
	Retention       RetentionConfig `yaml:"retention"`
}

// RetentionConfig bounds how long finished rows stay in the table. Published
// and Dead are measured from the moment a row entered that state (its
// updated_at). Zero disables cleanup for that state, so its rows are kept
// forever.
//
// Automatic cleanup runs inside the dispatcher worker, so a replica without
// worker.enabled never purges on its own; a host can drive the same pass with
// Manager.Purge instead.
//
// Zero is a meaningful value for Published and Dead, so normalized does not
// replace it with a default the way it does for the other numeric fields. The
// 168h default for Published comes from defaultConfig, which the framework
// pre-fills before binding: an absent key keeps it, while an explicit
// "published: 0" overwrites it and survives. A Config built by hand starts
// from the zero value and therefore has cleanup disabled.
type RetentionConfig struct {
	Published time.Duration `yaml:"published" default:"168h"`
	Dead      time.Duration `yaml:"dead" default:"0s"`
	Interval  time.Duration `yaml:"interval" default:"1h"`
	BatchSize int           `yaml:"batch_size" default:"1000"`
}

// enabled reports whether any finished state has a retention to enforce.
func (r RetentionConfig) enabled() bool { return r.Published > 0 || r.Dead > 0 }

// WorkerConfig controls lease-based publishing. Enabled defaults to false so
// importing/configuring the persistence API cannot emit external traffic until
// a Publisher is deliberately wired.
type WorkerConfig struct {
	Enabled         bool          `yaml:"enabled" default:"false"`
	PollInterval    time.Duration `yaml:"poll_interval" default:"1s"`
	BatchSize       int           `yaml:"batch_size" default:"100"`
	Concurrency     int           `yaml:"concurrency" default:"4"`
	LeaseDuration   time.Duration `yaml:"lease_duration" default:"30s"`
	RenewInterval   time.Duration `yaml:"renew_interval" default:"10s"`
	PublishTimeout  time.Duration `yaml:"publish_timeout" default:"10s"`
	DatabaseTimeout time.Duration `yaml:"database_timeout" default:"5s"`
	MaxAttempts     int           `yaml:"max_attempts" default:"10"`
	InitialBackoff  time.Duration `yaml:"initial_backoff" default:"1s"`
	MaxBackoff      time.Duration `yaml:"max_backoff" default:"5m"`
	Jitter          float64       `yaml:"jitter" default:"0.2"`
}

func defaultConfig() Config {
	return Config{
		DBInstance: "default", Table: "xbc_outbox_events", MaxPayloadBytes: 1 << 20,
		Worker: WorkerConfig{
			PollInterval: time.Second, BatchSize: 100, Concurrency: 4,
			LeaseDuration: 30 * time.Second, RenewInterval: 10 * time.Second,
			PublishTimeout: 10 * time.Second, DatabaseTimeout: 5 * time.Second,
			MaxAttempts: 10, InitialBackoff: time.Second, MaxBackoff: 5 * time.Minute, Jitter: .2,
		},
		Retention: RetentionConfig{Published: 168 * time.Hour, Interval: time.Hour, BatchSize: 1000},
	}
}

func (c Config) normalized() (Config, error) {
	d := defaultConfig()
	if strings.TrimSpace(c.DBInstance) == "" {
		c.DBInstance = d.DBInstance
	}
	if strings.TrimSpace(c.Table) == "" {
		c.Table = d.Table
	}
	if c.MaxPayloadBytes == 0 {
		c.MaxPayloadBytes = d.MaxPayloadBytes
	}
	w := &c.Worker
	if w.PollInterval == 0 {
		w.PollInterval = d.Worker.PollInterval
	}
	if w.BatchSize == 0 {
		w.BatchSize = d.Worker.BatchSize
	}
	if w.Concurrency == 0 {
		w.Concurrency = d.Worker.Concurrency
	}
	if w.LeaseDuration == 0 {
		w.LeaseDuration = d.Worker.LeaseDuration
	}
	if w.RenewInterval == 0 {
		w.RenewInterval = w.LeaseDuration / 3
	}
	if w.PublishTimeout == 0 {
		w.PublishTimeout = d.Worker.PublishTimeout
	}
	if w.DatabaseTimeout == 0 {
		w.DatabaseTimeout = d.Worker.DatabaseTimeout
	}
	if w.MaxAttempts == 0 {
		w.MaxAttempts = d.Worker.MaxAttempts
	}
	if w.InitialBackoff == 0 {
		w.InitialBackoff = d.Worker.InitialBackoff
	}
	if w.MaxBackoff == 0 {
		w.MaxBackoff = d.Worker.MaxBackoff
	}
	if w.Jitter == 0 {
		w.Jitter = d.Worker.Jitter
	}
	r := &c.Retention
	if r.Interval == 0 {
		r.Interval = d.Retention.Interval
	}
	if r.BatchSize == 0 {
		r.BatchSize = d.Retention.BatchSize
	}
	// Published and Dead deliberately retain explicit zero values: zero
	// disables cleanup of that state. See RetentionConfig.
	c.DBInstance = plugin.NormalizeInstance(strings.TrimSpace(c.DBInstance))
	if err := plugin.ValidateInstanceName(c.DBInstance); err != nil {
		return Config{}, fmt.Errorf("outbox: invalid db_instance: %w", err)
	}
	if !tablePattern.MatchString(c.Table) {
		return Config{}, fmt.Errorf("outbox: invalid table name %q", c.Table)
	}
	if c.MaxPayloadBytes <= 0 {
		return Config{}, errors.New("outbox: max_payload_bytes must be positive")
	}
	if w.PollInterval <= 0 || w.LeaseDuration <= 0 || w.RenewInterval <= 0 || w.PublishTimeout <= 0 || w.DatabaseTimeout <= 0 || w.InitialBackoff <= 0 || w.MaxBackoff <= 0 {
		return Config{}, errors.New("outbox: worker durations must be positive")
	}
	if w.RenewInterval > w.LeaseDuration/2 {
		return Config{}, errors.New("outbox: renew_interval must not exceed half lease_duration")
	}
	if w.BatchSize <= 0 || w.Concurrency <= 0 || w.MaxAttempts <= 0 {
		return Config{}, errors.New("outbox: batch_size, concurrency, and max_attempts must be positive")
	}
	if w.MaxBackoff < w.InitialBackoff {
		return Config{}, errors.New("outbox: max_backoff must not be less than initial_backoff")
	}
	if w.Jitter < 0 || w.Jitter > 1 {
		return Config{}, errors.New("outbox: jitter must be between 0 and 1")
	}
	if r.Published < 0 || r.Dead < 0 {
		return Config{}, errors.New("outbox: retention published and dead must not be negative")
	}
	if r.Interval <= 0 || r.BatchSize <= 0 {
		return Config{}, errors.New("outbox: retention interval and batch_size must be positive")
	}
	return c, nil
}

// Validate checks configuration without acquiring resources.
func (c Config) Validate() error { _, err := c.normalized(); return err }
