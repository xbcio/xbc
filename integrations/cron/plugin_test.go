package cron

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	robfigcron "github.com/robfig/cron/v3"

	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned unstable composition")
	}
}

func TestPrepareConfigPreservesDefaultsAndNormalizesZeroValues(t *testing.T) {
	defaults := defaultConfig()
	prepared, err := prepareConfig(defaults)
	if err != nil {
		t.Fatalf("prepareConfig(defaults) error = %v", err)
	}
	want := defaults
	want.Distributed.RenewInterval = want.Distributed.TTL / 3
	if !reflect.DeepEqual(prepared, want) {
		t.Fatalf("prepared defaults = %#v, want %#v", prepared, want)
	}

	zero, err := prepareConfig(Config{})
	if err != nil {
		t.Fatalf("prepareConfig(zero) error = %v", err)
	}
	if zero.Timezone != "Local" || zero.Concurrency != ConcurrencySkip || zero.Distributed.KeyPrefix != "xbc:cron" {
		t.Fatalf("normalized zero config = %#v", zero)
	}
	if zero.Distributed.TTL != 30*time.Second || zero.Distributed.RenewInterval != 10*time.Second {
		t.Fatalf("normalized lease defaults = %#v", zero.Distributed)
	}
	if zero.Distributed.Redis.DialTimeout != 5*time.Second || zero.Distributed.Redis.ReadTimeout != 3*time.Second || zero.Distributed.Redis.WriteTimeout != 3*time.Second {
		t.Fatalf("normalized Redis defaults = %#v", zero.Distributed.Redis)
	}
}

func TestPrepareConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"timezone", func(config *Config) { config.Timezone = "Not/A_Real_Zone" }},
		{"concurrency", func(config *Config) { config.Concurrency = "parallel" }},
		{"key prefix whitespace", func(config *Config) { config.Distributed.KeyPrefix = " locks " }},
		{"short TTL", func(config *Config) { config.Distributed.TTL = time.Nanosecond }},
		{"slow renewal", func(config *Config) {
			config.Distributed.TTL = 10 * time.Millisecond
			config.Distributed.RenewInterval = 6 * time.Millisecond
		}},
		{"two Redis sources", func(config *Config) {
			config.Distributed.RedisInstance = "locks"
			config.Distributed.Redis.Addr = "127.0.0.1:6379"
		}},
		{"invalid Redis instance", func(config *Config) { config.Distributed.RedisInstance = "Bad" }},
		{"negative Redis DB", func(config *Config) { config.Distributed.Redis.DB = -1 }},
		{"negative Redis timeout", func(config *Config) { config.Distributed.Redis.ReadTimeout = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := defaultConfig()
			test.edit(&config)
			if _, err := prepareConfig(config); err == nil {
				t.Fatal("prepareConfig() error = nil")
			}
		})
	}
}

func TestFactoryValidatesContributorSnapshotAndSchedules(t *testing.T) {
	validConfig, err := prepareConfig(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	validJob := &funcJob{name: "cleanup", spec: "@hourly", run: func(context.Context) error { return nil }}
	p, err := newConfiguredPlugin(validConfig, []plugin.Entry[JobContributor]{contributorEntry("jobs", validJob)}, nil, nil)
	if err != nil {
		t.Fatalf("newConfiguredPlugin() error = %v", err)
	}
	if len(p.jobs) != 1 || p.jobs[0].label != "worker[jobs]/cleanup" {
		t.Fatalf("jobs = %#v", p.jobs)
	}

	cases := []struct {
		name         string
		contributors []plugin.Entry[JobContributor]
	}{
		{"none", nil},
		{"nil contributor", []plugin.Entry[JobContributor]{{Identity: plugin.Identity{Plugin: "worker"}, Value: (*nilContributor)(nil)}}},
		{"nil job", []plugin.Entry[JobContributor]{contributorEntry("jobs", nil)}},
		{"invalid name", []plugin.Entry[JobContributor]{contributorEntry("jobs", &funcJob{name: "Bad", spec: "@hourly", run: func(context.Context) error { return nil }})}},
		{"empty spec", []plugin.Entry[JobContributor]{contributorEntry("jobs", &funcJob{name: "empty", spec: " ", run: func(context.Context) error { return nil }})}},
		{"invalid spec", []plugin.Entry[JobContributor]{contributorEntry("jobs", &funcJob{name: "broken", spec: "not cron", run: func(context.Context) error { return nil }})}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newConfiguredPlugin(validConfig, test.contributors, nil, nil); err == nil {
				t.Fatal("newConfiguredPlugin() error = nil")
			}
		})
	}
}

type nilContributor struct{}

func (*nilContributor) Jobs() []Job { return nil }

func TestConfiguredTimezoneControlsSchedule(t *testing.T) {
	job := &funcJob{name: "morning", spec: "0 9 * * *", run: func(context.Context) error { return nil }}
	p := newTestPlugin(t, func(config *Config) { config.Timezone = "Asia/Shanghai" }, nil, job)
	next := p.jobs[0].schedule.Next(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	want := time.Date(2026, time.January, 1, 9, 0, 0, 0, p.location)
	if !next.Equal(want) {
		t.Fatalf("next occurrence = %s, want %s", next, want)
	}
}

func TestLifecycleRejectsInvalidOrderingAndNilContext(t *testing.T) {
	job := &funcJob{name: "ordering", spec: "@hourly", run: func(context.Context) error { return nil }}
	p := newTestPlugin(t, nil, nil, job)
	if err := p.init(nil); err == nil {
		t.Fatal("init(nil) error = nil")
	}
	if err := p.start(nil); err == nil {
		t.Fatal("start(nil) error = nil")
	}
	host := newTestHost()
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("start before init error = nil")
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.close()
}

func TestRunnerWaitsForTrafficGateAndStopIsIdempotent(t *testing.T) {
	ran := make(chan struct{}, 1)
	job := &funcJob{name: "gate", spec: "@hourly", run: func(context.Context) error {
		ran <- struct{}{}
		return nil
	}}
	host := newTestHost()
	p, ctx := initTestPlugin(t, host, func(config *Config) { config.RunImmediately = true }, nil, job)
	if err := p.start(ctx); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if submitted, critical := host.counts(); submitted != 1 || critical != 1 {
		t.Fatalf("managed tasks = %d/%d, want 1/1", submitted, critical)
	}
	select {
	case <-ran:
		t.Fatal("job ran before traffic gate opened")
	case <-time.After(50 * time.Millisecond):
	}
	host.openTraffic()
	awaitSignal(t, ran, "immediate job")
	if err := p.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("repeated stop error = %v", err)
	}
	host.close()
}

func TestSecondsScheduleExecutes(t *testing.T) {
	ran := make(chan struct{}, 1)
	job := &funcJob{name: "heartbeat", spec: "*/1 * * * * *", run: func(context.Context) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	}}
	host := newTestHost()
	p, ctx := initTestPlugin(t, host, func(config *Config) { config.Seconds = true }, nil, job)
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, ran, "seconds-based execution")
	stopTestPlugin(t, p, host)
}

func TestStartHandlesFirstAndPartialAdmissionRejection(t *testing.T) {
	jobs := []Job{
		&funcJob{name: "first", spec: "@hourly", run: func(context.Context) error { return nil }},
		&funcJob{name: "second", spec: "@hourly", run: func(context.Context) error { return nil }},
	}
	for _, test := range []struct {
		name  string
		limit int
		want  int
	}{{"first", 0, 0}, {"partial", 1, 1}} {
		t.Run(test.name, func(t *testing.T) {
			host := newTestHost()
			host.setAdmissionLimit(test.limit)
			p, ctx := initTestPlugin(t, host, nil, nil, jobs...)
			if err := p.start(ctx); err == nil {
				t.Fatal("start() error = nil")
			}
			if submitted, critical := host.counts(); submitted != test.want || critical != test.want {
				t.Fatalf("managed tasks = %d/%d, want %d/%d", submitted, critical, test.want, test.want)
			}
			if err := p.stop(context.Background()); err != nil {
				t.Fatalf("stop after rejected Start error = %v", err)
			}
			if err := p.stop(context.Background()); err != nil {
				t.Fatalf("repeated stop error = %v", err)
			}
			host.close()
		})
	}
}

func TestStopTimeoutCanBeRetriedAfterJobQuiesces(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	job := &funcJob{name: "stubborn", spec: "@hourly", run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	host := newTestHost()
	p, runtimeContext := initTestPlugin(t, host, func(config *Config) { config.RunImmediately = true }, nil, job)
	if err := p.start(runtimeContext); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, started, "stubborn job start")

	stopContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.stop(stopContext); err == nil {
		t.Fatal("first stop error = nil, want deadline")
	}
	close(release)
	if err := p.stop(context.Background()); err != nil {
		t.Fatalf("retry stop error = %v", err)
	}
	host.close()
}

func TestJobsRunInParallelButEachRunnerIsSerial(t *testing.T) {
	var running atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	newJob := func(name string) Job {
		return &funcJob{name: name, spec: "@hourly", run: func(ctx context.Context) error {
			current := running.Add(1)
			updateMax(&maximum, current)
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			running.Add(-1)
			return nil
		}}
	}
	host := newTestHost()
	p, ctx := initTestPlugin(t, host, func(config *Config) { config.RunImmediately = true }, nil, newJob("one"), newJob("two"))
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, started, "first parallel job")
	awaitSignal(t, started, "second parallel job")
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent jobs = %d, want 2", maximum.Load())
	}
	close(release)
	stopTestPlugin(t, p, host)
}

func TestPanicIsIsolatedAndLaterOccurrenceRuns(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	job := &funcJob{name: "panic_once", spec: "*/1 * * * * *", run: func(context.Context) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		close(second)
		return nil
	}}
	host := newTestHost()
	p, ctx := initTestPlugin(t, host, func(config *Config) {
		config.Seconds = true
		config.RunImmediately = true
	}, nil, job)
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	host.openTraffic()
	awaitSignal(t, second, "execution after recovered panic")
	stopTestPlugin(t, p, host)
}

func TestNextOccurrenceImplementsSkipAndDelay(t *testing.T) {
	schedule := fixedIntervalSchedule{interval: time.Minute}
	scheduled := time.Unix(0, 0)
	finished := scheduled.Add(5*time.Minute + 30*time.Second)
	if got, want := nextOccurrence(ConcurrencyDelay, schedule, scheduled, finished), scheduled.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("delay next = %s, want %s", got, want)
	}
	if got, want := nextOccurrence(ConcurrencySkip, schedule, scheduled, finished), finished.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("skip next = %s, want %s", got, want)
	}
}

type fixedIntervalSchedule struct{ interval time.Duration }

func (schedule fixedIntervalSchedule) Next(after time.Time) time.Time {
	return after.Add(schedule.interval)
}

var _ robfigcron.Schedule = fixedIntervalSchedule{}
