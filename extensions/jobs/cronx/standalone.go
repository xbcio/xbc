package cronx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/xbcio/xbc/log"
)

// EntryID identifies one entry in a Scheduler, as returned by Add. It is the
// robfig/cron identifier, so a caller migrating from a *robfigcron.Cron keeps
// using the same values and conversions.
type EntryID = robfigcron.EntryID

// Entry is one entry snapshot as reported by Entries. It is robfig/cron's
// entry, so ID, Schedule, Next, Prev, and Job mean exactly what they mean
// there.
type Entry = robfigcron.Entry

// ErrStopped is returned by Add after the Scheduler has been stopped.
var ErrStopped = errors.New("cron: scheduler is stopped")

// Scheduler is the standalone cron scheduler. It owns a robfig/cron instance
// and adds the lifecycle the plugin form keeps private: an exported
// constructor, runtime Add and Remove, and a Stop that waits for the
// invocations already running.
//
// It needs no XBC application, plugin context, or runtime: New interprets
// specs with the configured parser, evaluates them in the configured
// location, and reports diagnostics through the log facade.
//
// A Scheduler is safe for concurrent use.
type Scheduler struct {
	cron *robfigcron.Cron

	// mu orders Add and Start against Stop. It is held across Add's call into
	// robfig/cron, so an accepted Add happens before Stop marks the scheduler
	// closed; an Add that arrives afterwards is refused instead of landing in
	// an entry list nothing will run.
	mu      sync.Mutex
	stopped bool
}

// Option configures New. Options are applied in the order supplied, and a nil
// option is an error rather than a silent skip.
type Option func(*options) error

type options struct {
	location *time.Location
	parser   robfigcron.ScheduleParser
	logger   log.Logger
}

func defaultOptions() options {
	return options{
		location: time.Local,
		parser:   Parser(false),
		logger:   nil,
	}
}

// WithLocation sets the time zone schedules are evaluated in. A nil location
// selects time.Local. The zone is applied as robfig/cron's instance location;
// New never rewrites a spec with a CRON_TZ= prefix, so a five-field spec keeps
// meaning the local wall clock, exactly as it did under a bare
// *robfigcron.Cron. A spec that already carries CRON_TZ= or TZ= keeps its own
// declared zone.
func WithLocation(location *time.Location) Option {
	return func(config *options) error {
		if location == nil {
			location = time.Local
		}
		config.location = location
		return nil
	}
}

// WithSecondsOptional selects whether specs may carry a leading seconds field.
// It is Parser(optional) as an Option. When optional is true the parser accepts
// five or six fields; when false it accepts the standard five fields only.
// Descriptors such as @hourly and @every are accepted in both modes.
func WithSecondsOptional(optional bool) Option {
	return WithParser(Parser(optional))
}

// WithParser overrides how specs are parsed. The parser must be non-nil; build
// one with Parser or robfigcron.NewParser. The parser also decides which
// descriptors are accepted.
func WithParser(parser robfigcron.ScheduleParser) Option {
	return func(config *options) error {
		if isNilInterface(parser) {
			return errors.New("cron: parser must not be nil")
		}
		config.parser = parser
		return nil
	}
}

// WithLogger sets the log facade logger scheduler diagnostics are written to.
// A nil logger restores the default, which is the facade's process logger
// resolved at each log call, so a scheduler built before log.Init still follows
// the initialized backend.
func WithLogger(logger log.Logger) Option {
	return func(config *options) error {
		config.logger = logger
		return nil
	}
}

// Parser returns the spec parser a Scheduler uses. The five standard fields are
// always accepted. secondsOptional adds robfig/cron's SecondOptional field, so
// a spec may then carry a leading seconds field; a five-field spec keeps its
// seconds defaulted to zero. Descriptors such as @hourly and @every are
// accepted either way.
//
// The plugin's Config.Seconds is a different switch: it selects a parser that
// requires six fields, because a configured plugin section states its intent
// up front. This parser is the permissive form for a process-wide scheduler
// that must keep accepting its legacy five-field specs while gaining
// seconds-precision ones.
func Parser(secondsOptional bool) robfigcron.Parser {
	fields := robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor
	if secondsOptional {
		fields |= robfigcron.SecondOptional
	}
	return robfigcron.NewParser(fields)
}

// New constructs a standalone Scheduler. It requires no XBC application or
// runtime; the caller owns the scheduler and releases it with Stop.
//
// The defaults are time.Local, the standard five-field parser with descriptors
// (Parser(false)), and the log facade's process logger. Callers that need the
// legacy five-or-six-field form use WithSecondsOptional(true).
//
// Add may be called before or after Start. Start is idempotent and a no-op
// after Stop. Add after Stop returns ErrStopped. Stop stops scheduling and
// waits for invocations already running.
//
// Unlike the plugin form, a standalone Scheduler has no Job snapshot, no
// distributed lease, no workload quota, no traffic gate, and no
// skip/delay concurrency policy: each occurrence runs in its own goroutine
// (overlapping occurrences of one entry may run concurrently), and a panicking
// job is not recovered. A host that needs those capabilities composes the
// Bundle instead of calling New.
func New(opts ...Option) (*Scheduler, error) {
	config := defaultOptions()
	for index, option := range opts {
		if option == nil {
			return nil, fmt.Errorf("cron: option %d is nil", index)
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if config.location == nil {
		config.location = time.Local
	}
	if isNilInterface(config.parser) {
		return nil, errors.New("cron: parser must not be nil")
	}
	return &Scheduler{
		cron: robfigcron.New(
			robfigcron.WithLocation(config.location),
			robfigcron.WithParser(config.parser),
			robfigcron.WithLogger(LogAdapter{Logger: config.logger}),
		),
	}, nil
}

// Add schedules fn to run on spec. The spec is parsed by the scheduler's
// parser, so units and accepted field counts are the ones described by Parser
// and WithSecondsOptional. Add is safe at runtime, before Start, and while
// stopped it returns ErrStopped.
func (s *Scheduler) Add(spec string, fn func()) (EntryID, error) {
	if fn == nil {
		return 0, errors.New("cron: Add requires a non-nil job function")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return 0, ErrStopped
	}
	return s.cron.AddFunc(spec, fn)
}

// Remove stops the entry with id from being run in the future. It is a no-op
// for an unknown id, for an entry that already ran, and after Stop.
func (s *Scheduler) Remove(id EntryID) {
	s.cron.Remove(id)
}

// Entries returns a snapshot of the scheduler's entries. Next is the zero time
// before Start and for a schedule with no future occurrence; Prev is the last
// activation time.
func (s *Scheduler) Entries() []Entry {
	return s.cron.Entries()
}

// Start starts the scheduler in its own goroutine. It is idempotent and a no-op
// after Stop. Entries added before Start run from the first matching
// occurrence after it.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.cron.Start()
}

// Stop stops scheduling new invocations and waits for the invocations already
// running to finish. A nil context means context.Background(). When the
// context expires first, Stop returns an error wrapping ctx.Err(); the running
// invocations keep running, and a later Stop can wait for them again.
//
// Stop is idempotent and safe before Start, where there is nothing to wait for.
// After Stop, Add returns ErrStopped and Start is a no-op.
func (s *Scheduler) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()

	select {
	case <-s.cron.Stop().Done():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cron: wait for running jobs: %w", ctx.Err())
	}
}

// LogAdapter adapts the XBC log facade to robfig/cron's Logger interface. The
// zero value is usable: it resolves the facade's process logger at each call,
// so it follows log.Init when the scheduler is built first.
type LogAdapter struct {
	Logger log.Logger
}

var _ robfigcron.Logger = LogAdapter{}

// Info implements robfigcron.Logger.
func (a LogAdapter) Info(msg string, keysAndValues ...any) {
	a.logger().Info(msg, keysAndValues...)
}

// Error implements robfigcron.Logger. The error is passed as the leading
// "error" field, matching the facade's structured errors and the plugin's job
// failure records.
func (a LogAdapter) Error(err error, msg string, keysAndValues ...any) {
	kv := make([]any, 0, len(keysAndValues)+2)
	kv = append(kv, "error", err)
	kv = append(kv, keysAndValues...)
	a.logger().Error(msg, kv...)
}

func (a LogAdapter) logger() log.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return log.L()
}
