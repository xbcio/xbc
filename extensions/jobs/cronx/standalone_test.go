package cronx

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	corelog "github.com/xbcio/xbc/log"
)

// TestParserAcceptsFiveAndSixFieldSpecs covers the standalone parser contract:
// the optional-seconds form accepts both five and six fields, the standard form
// accepts five only, descriptors work in both, and a five-field spec defaults
// its seconds to zero.
func TestParserAcceptsFiveAndSixFieldSpecs(t *testing.T) {
	optional := Parser(true)
	standard := Parser(false)

	for name, test := range map[string]struct {
		parser robfigcron.Parser
		spec   string
	}{
		"six fields with optional seconds":     {optional, "*/15 * * * * *"},
		"five fields with optional seconds":    {optional, "0 * * * *"},
		"five fields with the standard parser": {standard, "0 * * * *"},
		"descriptor with optional seconds":     {optional, "@every 1s"},
		"descriptor with the standard parser":  {standard, "@hourly"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := test.parser.Parse(test.spec); err != nil {
				t.Fatalf("Parse(%q) error = %v", test.spec, err)
			}
		})
	}

	if _, err := standard.Parse("*/15 * * * * *"); err == nil {
		t.Fatal("standard parser accepted a six-field spec")
	}

	base := time.Date(2026, time.January, 2, 10, 30, 45, 500_000_000, time.UTC)

	fiveField, err := optional.Parse("0 * * * *")
	if err != nil {
		t.Fatalf("Parse(five fields) error = %v", err)
	}
	if got, want := fiveField.Next(base), time.Date(2026, time.January, 2, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("five-field next = %s, want %s (seconds defaulted to zero)", got, want)
	}

	sixField, err := optional.Parse("*/15 * * * * *")
	if err != nil {
		t.Fatalf("Parse(six fields) error = %v", err)
	}
	sixBase := time.Date(2026, time.January, 2, 10, 30, 31, 0, time.UTC)
	if got, want := sixField.Next(sixBase), time.Date(2026, time.January, 2, 10, 30, 45, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("six-field next = %s, want %s", got, want)
	}
}

// TestSchedulerAddsAndRemovesAtRuntime covers the standalone lifecycle: Add
// before and after Start, Remove, Entries, the error paths, and the closed
// state after Stop.
func TestSchedulerAddsAndRemovesAtRuntime(t *testing.T) {
	scheduler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = scheduler.Stop(context.Background()) })

	id, err := scheduler.Add("@hourly", func() {})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	entries := scheduler.Entries()
	if len(entries) != 1 || entries[0].ID != id {
		t.Fatalf("entries = %#v, want one entry with id %d", entries, id)
	}
	if !entries[0].Next.IsZero() {
		t.Fatalf("Next = %s before Start, want the zero time", entries[0].Next)
	}

	scheduler.Start()
	scheduler.Start()
	entries = scheduler.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries after Start = %d, want 1", len(entries))
	}
	if entries[0].Next.IsZero() {
		t.Fatal("Next is the zero time after Start, want the first occurrence")
	}

	scheduler.Remove(id)
	if entries = scheduler.Entries(); len(entries) != 0 {
		t.Fatalf("entries after Remove = %#v, want none", entries)
	}

	if _, err := scheduler.Add("not-a-spec", func() {}); err == nil {
		t.Fatal("Add(invalid spec) error = nil")
	}
	if _, err := scheduler.Add("@hourly", nil); err == nil {
		t.Fatal("Add(nil func) error = nil")
	}

	runtimeID, err := scheduler.Add("@daily", func() {})
	if err != nil {
		t.Fatalf("Add() at runtime error = %v", err)
	}
	if entries = scheduler.Entries(); len(entries) != 1 || entries[0].ID != runtimeID {
		t.Fatalf("entries after runtime Add = %#v, want the new entry", entries)
	}
	scheduler.Remove(runtimeID)
	if entries = scheduler.Entries(); len(entries) != 0 {
		t.Fatalf("entries after runtime Remove = %#v, want none", entries)
	}

	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := scheduler.Add("@hourly", func() {}); !errors.Is(err, ErrStopped) {
		t.Fatalf("Add() after Stop error = %v, want ErrStopped", err)
	}
	scheduler.Start()
	if entries = scheduler.Entries(); len(entries) != 0 {
		t.Fatalf("Start after Stop restarted the scheduler: entries = %#v", entries)
	}
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
}

// TestStopBeforeStartAndWithNilContext pins the shutdown states that have
// nothing to wait for.
func TestStopBeforeStartAndWithNilContext(t *testing.T) {
	scheduler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := scheduler.Stop(nil); err != nil {
		t.Fatalf("Stop(nil) before Start error = %v", err)
	}
	if _, err := scheduler.Add("@hourly", func() {}); !errors.Is(err, ErrStopped) {
		t.Fatalf("Add() after Stop error = %v, want ErrStopped", err)
	}
	scheduler.Start()
	if entries := scheduler.Entries(); len(entries) != 0 {
		t.Fatalf("Start after Stop scheduled %#v, want nothing", entries)
	}
}

// TestStopWaitsForRunningJob proves Stop blocks until the invocation that was
// already running returns, and that a nil context is accepted by that path.
func TestStopWaitsForRunningJob(t *testing.T) {
	scheduler, err := New(WithSecondsOptional(true))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if _, err := scheduler.Add("* * * * * *", func() {
		once.Do(func() { close(started) })
		<-release
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	scheduler.Start()
	awaitSignal(t, started, "the scheduled job to start")

	stopped := make(chan error, 1)
	go func() { stopped <- scheduler.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before the running job finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the running job finished")
	}
}

// TestStopDeadlineCanBeRetriedAfterJobQuiesces mirrors the plugin's shutdown
// contract: an expired context stops waiting without stopping the job, and a
// later call finishes the wait.
func TestStopDeadlineCanBeRetriedAfterJobQuiesces(t *testing.T) {
	scheduler, err := New(WithSecondsOptional(true))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if _, err := scheduler.Add("* * * * * *", func() {
		once.Do(func() { close(started) })
		<-release
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	scheduler.Start()
	awaitSignal(t, started, "the scheduled job to start")

	stopContext, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = scheduler.Stop(stopContext)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want context.DeadlineExceeded", err)
	}

	close(release)
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("retried Stop() error = %v", err)
	}
}

// TestSchedulerUsesTheConfiguredLocationWithoutCRONPrefix proves the
// standalone scheduler evaluates a bare five-field spec in the configured
// location without rewriting it into a CRON_TZ form.
func TestSchedulerUsesTheConfiguredLocationWithoutCRONPrefix(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}

	scheduler, err := New(WithLocation(location))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = scheduler.Stop(context.Background()) })

	if _, err := scheduler.Add("0 9 * * *", func() {}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	scheduler.Start()

	entries := scheduler.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %#v, want one", entries)
	}
	spec, ok := entries[0].Schedule.(*robfigcron.SpecSchedule)
	if !ok {
		t.Fatalf("schedule type = %T, want *robfigcron.SpecSchedule", entries[0].Schedule)
	}
	if spec.Location != time.Local {
		t.Fatalf("spec location = %s, want time.Local: the standalone scheduler rewrote the spec with a time zone", spec.Location)
	}
	next := entries[0].Next
	if next.IsZero() {
		t.Fatal("Next is the zero time, want the first occurrence")
	}
	if next.Location() != location {
		t.Fatalf("Next location = %s, want %s", next.Location(), location)
	}
	if next.Hour() != 9 || next.Minute() != 0 || next.Second() != 0 {
		t.Fatalf("Next = %s, want 09:00:00 in %s", next, location)
	}
}

// TestSchedulerDefaultsToLocalTime covers the zero-value location rule both
// through the default and through an explicit nil option.
func TestSchedulerDefaultsToLocalTime(t *testing.T) {
	for name, options := range map[string][]Option{
		"default":        nil,
		"explicit nil":   {WithLocation(nil)},
		"explicit local": {WithLocation(time.Local)},
	} {
		t.Run(name, func(t *testing.T) {
			scheduler, err := New(options...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := scheduler.cron.Location(); got != time.Local {
				t.Fatalf("location = %s, want time.Local", got)
			}
		})
	}
}

// TestNewRejectsNilOptionAndParser pins the constructor's error paths rather
// than letting a nil option be skipped silently.
func TestNewRejectsNilOptionAndParser(t *testing.T) {
	if scheduler, err := New(nil); err == nil || scheduler != nil {
		t.Fatalf("New(nil) = (%v, %v), want a nil scheduler and an error", scheduler, err)
	}
	if scheduler, err := New(WithParser(nil)); err == nil || scheduler != nil {
		t.Fatalf("New(WithParser(nil)) = (%v, %v), want a nil scheduler and an error", scheduler, err)
	}
}

// recordingLogger is a log.Logger double that captures the records it is
// handed, so the adapter and the scheduler wiring can be asserted without a
// live logging backend.
type recordingLogger struct {
	mu      sync.Mutex
	records []loggedRecord
}

type loggedRecord struct {
	level string
	msg   string
	kv    []any
}

func (l *recordingLogger) record(level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, loggedRecord{level: level, msg: msg, kv: append([]any(nil), kv...)})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.record("debug", msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.record("info", msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.record("warn", msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.record("error", msg, kv) }
func (l *recordingLogger) Fatal(string, ...any)        {}

func (l *recordingLogger) With(...any) corelog.Logger { return l }
func (l *recordingLogger) Enabled(corelog.Level) bool { return true }

func (l *recordingLogger) recorded() []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]loggedRecord(nil), l.records...)
}

var _ corelog.Logger = (*recordingLogger)(nil)

// TestLogAdapterForwardsCronRecordsToTheFacade pins the adapter's field
// mapping: Info keeps the message and key/value pairs, and Error folds the
// error into a leading "error" field.
func TestLogAdapterForwardsCronRecordsToTheFacade(t *testing.T) {
	recorder := &recordingLogger{}
	adapter := LogAdapter{Logger: recorder}
	failure := errors.New("boom")

	adapter.Info("start", "entry", 7)
	adapter.Error(failure, "job failed", "job", "cleanup")

	records := recorder.recorded()
	if len(records) != 2 {
		t.Fatalf("records = %#v, want two", records)
	}
	if records[0].level != "info" || records[0].msg != "start" || !reflect.DeepEqual(records[0].kv, []any{"entry", 7}) {
		t.Fatalf("info record = %#v", records[0])
	}
	wantError := []any{"error", failure, "job", "cleanup"}
	if records[1].level != "error" || records[1].msg != "job failed" || !reflect.DeepEqual(records[1].kv, wantError) {
		t.Fatalf("error record = %#v, want key/value %#v", records[1], wantError)
	}
}

// TestZeroLogAdapterUsesTheFacadeLogger proves the zero adapter is usable
// without a backend configured: before log.Init the facade discards records,
// and neither method may panic.
func TestZeroLogAdapterUsesTheFacadeLogger(t *testing.T) {
	LogAdapter{}.Info("start", "entry", 1)
	LogAdapter{}.Error(errors.New("boom"), "failed")
}

// TestSchedulerWritesThroughTheConfiguredLogger proves WithLogger reaches the
// robfig/cron instance rather than only being stored.
func TestSchedulerWritesThroughTheConfiguredLogger(t *testing.T) {
	recorder := &recordingLogger{}
	scheduler, err := New(WithLogger(recorder))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scheduler.Start()
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if records := recorder.recorded(); len(records) == 0 {
		t.Fatal("the scheduler wrote no records through the configured logger")
	}
}

// recordingCronLogger is a robfig/cron Logger double. It records both methods
// and signals the first "run" record, so a test can wait for the scheduler to
// invoke an entry rather than sleeping.
type recordingCronLogger struct {
	mu    sync.Mutex
	infos []cronLogRecord
	errs  []cronLogRecord
	run   chan struct{}
	once  sync.Once
}

type cronLogRecord struct {
	msg string
	kv  []any
}

func newRecordingCronLogger() *recordingCronLogger {
	return &recordingCronLogger{run: make(chan struct{})}
}

func (l *recordingCronLogger) Info(msg string, kv ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, cronLogRecord{msg: msg, kv: append([]any(nil), kv...)})
	l.mu.Unlock()
	if msg == "run" {
		l.once.Do(func() { close(l.run) })
	}
}

func (l *recordingCronLogger) Error(err error, msg string, kv ...any) {
	record := cronLogRecord{msg: msg, kv: append([]any{"error", err}, kv...)}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, record)
}

func (l *recordingCronLogger) infoMessages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	messages := make([]string, 0, len(l.infos))
	for _, record := range l.infos {
		messages = append(messages, record.msg)
	}
	return messages
}

func (l *recordingCronLogger) errors() []cronLogRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]cronLogRecord(nil), l.errs...)
}

var _ robfigcron.Logger = (*recordingCronLogger)(nil)

// TestSchedulerWritesThroughTheConfiguredCronLogger proves WithCronLogger
// reaches robfig/cron verbatim: the two-method logger sees the start, schedule,
// and run records of a real scheduling cycle without implementing the facade
// interface.
func TestSchedulerWritesThroughTheConfiguredCronLogger(t *testing.T) {
	logger := newRecordingCronLogger()
	scheduler, err := New(WithSecondsOptional(true), WithCronLogger(logger))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = scheduler.Stop(context.Background()) })
	if _, err := scheduler.Add("* * * * * *", func() {}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	scheduler.Start()
	awaitSignal(t, logger.run, "the cron run record")
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	seen := make(map[string]bool)
	for _, message := range logger.infoMessages() {
		seen[message] = true
	}
	for _, want := range []string{"start", "schedule", "run"} {
		if !seen[want] {
			t.Fatalf("cron info records = %v, want a %q record", logger.infoMessages(), want)
		}
	}
}

// TestCronLoggerAndLoggerShareOneSlot pins the override rule: WithLogger and
// WithCronLogger configure the same injection point, so only the option
// supplied last receives the scheduler's records.
func TestCronLoggerAndLoggerShareOneSlot(t *testing.T) {
	for name, test := range map[string]struct {
		options    func(*recordingLogger, *recordingCronLogger) []Option
		wantFacade bool
		wantCron   bool
	}{
		"cron logger supplied last": {
			options: func(facade *recordingLogger, cronLogger *recordingCronLogger) []Option {
				return []Option{WithLogger(facade), WithCronLogger(cronLogger)}
			},
			wantCron: true,
		},
		"facade logger supplied last": {
			options: func(facade *recordingLogger, cronLogger *recordingCronLogger) []Option {
				return []Option{WithCronLogger(cronLogger), WithLogger(facade)}
			},
			wantFacade: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			facade := &recordingLogger{}
			cronLogger := newRecordingCronLogger()
			scheduler, err := New(test.options(facade, cronLogger)...)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			scheduler.Start()
			if err := scheduler.Stop(context.Background()); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			if got := len(facade.recorded()) > 0; got != test.wantFacade {
				t.Fatalf("facade logger received records = %v, want %v", got, test.wantFacade)
			}
			if got := len(cronLogger.infoMessages()) > 0; got != test.wantCron {
				t.Fatalf("cron logger received records = %v, want %v", got, test.wantCron)
			}
		})
	}
}

// TestCronLoggerResolution pins nil semantics and the exact object handed to
// robfig/cron. A nil selection resolves the facade's process logger at each
// call through the zero LogAdapter, exactly as WithLogger(nil), and a supplied
// robfig/cron Logger is passed through unchanged, so its Error method is the
// one robfig/cron reports job failures through.
func TestCronLoggerResolution(t *testing.T) {
	facade := &recordingLogger{}
	cronLogger := newRecordingCronLogger()
	var nilCronLogger *recordingCronLogger

	for name, test := range map[string]struct {
		options []Option
		want    robfigcron.Logger
	}{
		"default":                             {want: LogAdapter{}},
		"nil cron logger":                     {options: []Option{WithCronLogger(nil)}, want: LogAdapter{}},
		"typed nil cron logger":               {options: []Option{WithCronLogger(nilCronLogger)}, want: LogAdapter{}},
		"nil facade logger":                   {options: []Option{WithLogger(nil)}, want: LogAdapter{}},
		"facade logger":                       {options: []Option{WithLogger(facade)}, want: LogAdapter{Logger: facade}},
		"cron logger":                         {options: []Option{WithCronLogger(cronLogger)}, want: cronLogger},
		"cron logger after the facade logger": {options: []Option{WithLogger(facade), WithCronLogger(cronLogger)}, want: cronLogger},
		"facade logger after the cron logger": {options: []Option{WithCronLogger(cronLogger), WithLogger(facade)}, want: LogAdapter{Logger: facade}},
		"nil cron logger after a cron logger": {options: []Option{WithCronLogger(cronLogger), WithCronLogger(nil)}, want: LogAdapter{}},
	} {
		t.Run(name, func(t *testing.T) {
			config := defaultOptions()
			for index, option := range test.options {
				if err := option(&config); err != nil {
					t.Fatalf("option %d error = %v", index, err)
				}
			}
			if got := config.resolvedCronLogger(); got != test.want {
				t.Fatalf("resolved logger = %#v, want %#v", got, test.want)
			}
		})
	}

	config := defaultOptions()
	if err := WithCronLogger(cronLogger)(&config); err != nil {
		t.Fatalf("WithCronLogger() error = %v", err)
	}
	failure := errors.New("boom")
	config.resolvedCronLogger().Error(failure, "job failed", "job", "cleanup")
	records := cronLogger.errors()
	wantError := []any{"error", failure, "job", "cleanup"}
	if len(records) != 1 || records[0].msg != "job failed" || !reflect.DeepEqual(records[0].kv, wantError) {
		t.Fatalf("cron logger errors = %#v, want the job failure record", records)
	}
}
