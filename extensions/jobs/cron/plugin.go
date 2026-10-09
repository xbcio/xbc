package cron

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	robfigcron "github.com/robfig/cron/v3"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is cron's stable Definition and configuration identity.
const Key plugin.Key = "cron"

var jobContributors = plugin.Collect[JobContributor]()

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins.cron"),
		Exports:    plugin.Contracts[*Plugin](),
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Init:  (*Plugin).init,
			Start: (*Plugin).start,
			Drain: (*Plugin).drain,
			Stop:  (*Plugin).stop,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns cron's canonical immutable Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns cron's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return bundle }

// plan declares only the inputs selected by the prepared configuration. It is
// pure: Redis clients, lockers, contributors, and jobs are not touched here.
func plan(config Config) (plugin.Plan[*Plugin], error) {
	if !config.Distributed.Enabled {
		return plugin.PlanOf(plugin.Inputs(jobContributors), func(ctx plugin.BuildContext) (*Plugin, error) {
			return newConfiguredPlugin(config, jobContributors.Get(ctx), nil, nil)
		}), nil
	}

	if instance := config.Distributed.RedisInstance; instance != "" {
		redisClient := plugin.RefToInstance[redis.UniversalClient]("redis", instance)
		return plugin.PlanOf(plugin.Inputs(jobContributors, redisClient), func(ctx plugin.BuildContext) (*Plugin, error) {
			client := redisClient.Get(ctx).Value
			if isNilInterface(client) {
				return nil, fmt.Errorf("cron: Redis instance %q exported a nil client", instance)
			}
			return newConfiguredPlugin(config, jobContributors.Get(ctx), nil, client)
		}), nil
	}

	if config.Distributed.Redis.Addr != "" {
		return plugin.PlanOf(plugin.Inputs(jobContributors), func(ctx plugin.BuildContext) (*Plugin, error) {
			return newConfiguredPlugin(config, jobContributors.Get(ctx), nil, nil)
		}), nil
	}

	lockerInput := plugin.OptionalOne[lease.Locker]()
	return plugin.PlanOf(plugin.Inputs(jobContributors, lockerInput), func(ctx plugin.BuildContext) (*Plugin, error) {
		entry, ok := lockerInput.Get(ctx)
		if !ok || isNilInterface(entry.Value) {
			return nil, errors.New("cron: distributed mode requires a lease.Locker, distributed.redis_instance, or distributed.redis.addr")
		}
		return newConfiguredPlugin(config, jobContributors.Get(ctx), entry.Value, nil)
	}), nil
}

// Plugin owns one immutable job snapshot and all runtime resources used to
// schedule it. Dependency resolution is complete before this value is created;
// lifecycle methods never perform service lookup.
type Plugin struct {
	config        Config
	parser        robfigcron.Parser
	location      *time.Location
	renewInterval time.Duration
	locker        lease.Locker
	ownedRedis    redis.UniversalClient
	jobs          []*scheduledJob

	// admissions is the quota an invocation charges, one limiter per workload
	// the job contributors belong to ("" for contributors that belong to none).
	// It is resolved at Init and read by the runners without a lock: nothing
	// writes it afterwards, and Start admits those runners only after Init.
	admissions map[plugin.WorkloadKey]plugin.Admission

	mu              sync.Mutex
	initialized     bool
	startAttempted  bool
	starting        bool
	started         bool
	stopping        bool
	runCancel       context.CancelFunc
	taskCount       int
	submissionsDone bool
	stopCh          chan struct{}
	// drainCh is closed by drain: runners stop scheduling new invocations and
	// return once the one in flight, if any, has finished. Unlike stopCh's
	// companion runCancel, it cancels nothing that is already running.
	drainCh       chan struct{}
	drainOnce     sync.Once
	draining      bool
	tasksDone     chan struct{}
	tasksDoneOnce sync.Once
	finalizeOnce  sync.Once
	finalizeDone  chan struct{}
	stopErr       error

	// process is the identity of the process this replica runs in, learned from
	// the Context at Init and published as the claimant of every job lock. It is
	// written once under mu and read without it: the only readers are tasks that
	// Start admits, and Start takes mu before admitting any of them.
	process string
}

func newConfiguredPlugin(
	config Config,
	contributors []plugin.Entry[JobContributor],
	selectedLocker lease.Locker,
	selectedRedis redis.UniversalClient,
) (_ *Plugin, err error) {
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return nil, fmt.Errorf("cron: load timezone %q: %w", config.Timezone, err)
	}
	parser := standardParser(config.Seconds)
	jobs, err := buildScheduledJobs(config, parser, contributors)
	if err != nil {
		return nil, err
	}

	var locker lease.Locker
	var owned redis.UniversalClient
	if config.Distributed.Enabled {
		switch {
		case !isNilInterface(selectedRedis):
			locker, err = newRedisLocker(selectedRedis)
		case !isNilInterface(selectedLocker):
			locker = selectedLocker
		case config.Distributed.Redis.Addr != "":
			redisConfig := config.Distributed.Redis
			owned = redis.NewClient(&redis.Options{
				Addr:         redisConfig.Addr,
				Username:     redisConfig.Username,
				Password:     redisConfig.Password,
				DB:           redisConfig.DB,
				DialTimeout:  redisConfig.DialTimeout,
				ReadTimeout:  redisConfig.ReadTimeout,
				WriteTimeout: redisConfig.WriteTimeout,
			})
			if pingErr := owned.Ping(context.Background()).Err(); pingErr != nil {
				_ = owned.Close()
				return nil, fmt.Errorf("cron: connect distributed Redis at %q: %w", redisConfig.Addr, pingErr)
			}
			locker, err = newRedisLocker(owned)
		default:
			err = errors.New("cron: distributed mode has no lock backend")
		}
		if err != nil {
			if owned != nil {
				_ = owned.Close()
			}
			return nil, err
		}
	}

	return &Plugin{
		config:        config,
		parser:        parser,
		location:      location,
		renewInterval: config.Distributed.RenewInterval,
		locker:        locker,
		ownedRedis:    owned,
		jobs:          jobs,
		stopCh:        make(chan struct{}),
		drainCh:       make(chan struct{}),
		tasksDone:     make(chan struct{}),
		finalizeDone:  make(chan struct{}),
	}, nil
}

// prepareConfig normalizes the same defaults historically accepted by the
// integration and validates every configuration-only invariant. It performs no
// I/O and creates no runtime resources, so it is safe to call from planning.
func prepareConfig(config Config) (Config, error) {
	if config.Timezone == "" {
		config.Timezone = "Local"
	}
	if _, err := time.LoadLocation(config.Timezone); err != nil {
		return Config{}, fmt.Errorf("cron: load timezone %q: %w", config.Timezone, err)
	}
	if config.Concurrency == "" {
		config.Concurrency = ConcurrencySkip
	}
	if config.Concurrency != ConcurrencySkip && config.Concurrency != ConcurrencyDelay {
		return Config{}, fmt.Errorf("cron: concurrency must be %q or %q, got %q", ConcurrencySkip, ConcurrencyDelay, config.Concurrency)
	}

	distributed := &config.Distributed
	if distributed.KeyPrefix == "" {
		distributed.KeyPrefix = "xbc:cron"
	}
	if strings.TrimSpace(distributed.KeyPrefix) != distributed.KeyPrefix || distributed.KeyPrefix == "" {
		return Config{}, errors.New("cron: distributed.key_prefix must be non-empty and have no surrounding whitespace")
	}
	if distributed.TTL == 0 {
		distributed.TTL = 30 * time.Second
	}
	if distributed.TTL < time.Millisecond {
		return Config{}, fmt.Errorf("cron: distributed.ttl must be at least 1ms, got %s", distributed.TTL)
	}
	if distributed.RenewInterval == 0 {
		distributed.RenewInterval = distributed.TTL / 3
	}
	if distributed.RenewInterval < time.Millisecond {
		return Config{}, fmt.Errorf("cron: distributed.renew_interval must be at least 1ms, got %s", distributed.RenewInterval)
	}
	if distributed.RenewInterval > distributed.TTL/2 {
		return Config{}, fmt.Errorf(
			"cron: distributed.renew_interval (%s) must not exceed half of ttl (%s)",
			distributed.RenewInterval, distributed.TTL,
		)
	}
	if distributed.RedisInstance != "" {
		if err := plugin.ValidateInstanceName(distributed.RedisInstance); err != nil {
			return Config{}, fmt.Errorf("cron: invalid distributed.redis_instance: %w", err)
		}
	}
	if distributed.RedisInstance != "" && distributed.Redis.Addr != "" {
		return Config{}, errors.New("cron: configure only one of distributed.redis_instance and distributed.redis.addr")
	}
	if distributed.Redis.DB < 0 {
		return Config{}, fmt.Errorf("cron: distributed.redis.db cannot be negative, got %d", distributed.Redis.DB)
	}

	var err error
	distributed.Redis.DialTimeout, err = redisTimeout("dial_timeout", distributed.Redis.DialTimeout, 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	distributed.Redis.ReadTimeout, err = redisTimeout("read_timeout", distributed.Redis.ReadTimeout, 3*time.Second)
	if err != nil {
		return Config{}, err
	}
	distributed.Redis.WriteTimeout, err = redisTimeout("write_timeout", distributed.Redis.WriteTimeout, 3*time.Second)
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

func redisTimeout(name string, value, fallback time.Duration) (time.Duration, error) {
	if value < 0 {
		return 0, fmt.Errorf("cron: distributed.redis.%s cannot be negative, got %s", name, value)
	}
	if value == 0 {
		return fallback, nil
	}
	return value, nil
}

func buildScheduledJobs(config Config, parser robfigcron.Parser, contributors []plugin.Entry[JobContributor]) ([]*scheduledJob, error) {
	entries := make([]*scheduledJob, 0)
	seen := make(map[string]struct{})
	for _, contributor := range contributors {
		if isNilInterface(contributor.Value) {
			return nil, fmt.Errorf("cron: JobContributor %s is nil", contributor.Identity)
		}
		jobs, err := contributorJobs(contributor)
		if err != nil {
			return nil, err
		}
		for index, job := range jobs {
			if isNilInterface(job) {
				return nil, fmt.Errorf("cron: JobContributor %s returned nil job at index %d", contributor.Identity, index)
			}
			name, spec, err := jobMetadata(contributor.Identity, job)
			if err != nil {
				return nil, err
			}
			label := contributor.Identity.String() + "/" + name
			if _, duplicate := seen[label]; duplicate {
				return nil, fmt.Errorf("cron: duplicate job %q", label)
			}
			seen[label] = struct{}{}

			schedule, err := parser.Parse(applyTimezone(spec, config.Timezone))
			if err != nil {
				return nil, fmt.Errorf("cron: parse spec %q for job %s: %w", spec, label, err)
			}
			if schedule.Next(time.Now().In(time.Local)).IsZero() {
				return nil, fmt.Errorf("cron: spec %q for job %s has no future occurrence", spec, label)
			}
			entry := &scheduledJob{
				job:        job,
				label:      label,
				workload:   contributor.Workload,
				lockKey:    lockKey(config.Distributed.KeyPrefix, label),
				schedule:   schedule,
				runnerDone: make(chan struct{}),
			}
			if config.Distributed.Enabled {
				entry.renewals = make(chan *leaseSession)
			}
			entries = append(entries, entry)
		}
	}
	// No jobs is not an error. A process whose Plugins contribute none -- the
	// standby half of a role handover, or a deployment where this replica is
	// configured but has nothing to schedule -- starts, schedules nothing, and
	// provides the long-lived task the runtime needs (see start). Refusing
	// construction was what made the takeover model unusable: the process that
	// must stay alive is the one with no work at that moment.
	return entries, nil
}

func contributorJobs(entry plugin.Entry[JobContributor]) (jobs []Job, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cron: JobContributor %s Jobs callback panicked: %v", entry.Identity, recovered)
		}
	}()
	return entry.Value.Jobs(), nil
}

func jobMetadata(identity plugin.Identity, job Job) (name, spec string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("cron: JobContributor %s job metadata callback panicked: %v", identity, recovered)
		}
	}()
	name = job.Name()
	if validationErr := plugin.ValidateName(name); validationErr != nil {
		return "", "", fmt.Errorf("cron: JobContributor %s returned invalid job name: %w", identity, validationErr)
	}
	spec = strings.TrimSpace(job.Spec())
	if spec == "" {
		return "", "", fmt.Errorf("cron: job %s/%s has an empty spec", identity, name)
	}
	return name, spec, nil
}

func applyTimezone(spec, timezone string) string {
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		return spec
	}
	return "CRON_TZ=" + timezone + " " + spec
}

func lockKey(prefix, label string) string {
	return strings.TrimSuffix(prefix, ":") + ":" + label
}

func standardParser(seconds bool) robfigcron.Parser {
	options := robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor
	if seconds {
		options |= robfigcron.Second
	}
	return robfigcron.NewParser(options)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (p *Plugin) init(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("cron: Init requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.stopping:
		return errors.New("cron: cannot initialize after Stop")
	case p.initialized:
		return errors.New("cron: Init called more than once")
	default:
		p.initialized = true
		p.process = ctx.ProcessInstance()
		p.admissions = p.resolveAdmissions(ctx)
		return nil
	}
}

// resolveAdmissions asks the runtime for one limiter per workload its job
// contributors belong to, plus the scheduler's own for the contributors that
// belong to none.
//
// The submission path cannot express this by itself: the scheduler submits
// every runner with its own identity, so a workload's budget would never see an
// invocation run on its behalf. Asking per declared workload is what keeps a
// job's work inside the same quota the workload's managed tasks and queue
// handlers charge.
func (p *Plugin) resolveAdmissions(ctx *plugin.Context) map[plugin.WorkloadKey]plugin.Admission {
	admissions := make(map[plugin.WorkloadKey]plugin.Admission)
	for _, entry := range p.jobs {
		if _, resolved := admissions[entry.workload]; resolved {
			continue
		}
		if entry.workload == "" {
			admissions[entry.workload] = ctx.Admission()
			continue
		}
		admissions[entry.workload] = ctx.AdmissionFor(entry.workload)
	}
	return admissions
}

// drain stops every runner from scheduling another invocation and waits,
// within ctx, for the invocations already running to finish. Their contexts
// stay live -- a running job is neither cancelled nor robbed of its lease
// renewal -- and the owned Redis client stays open, because a distributed
// job in flight still needs it to renew and release its lock. stop is what
// cancels whatever drain could not wait out and closes Redis.
//
// It is safe before Start, where there is nothing to wait for, and repeated
// calls share the same wait.
func (p *Plugin) drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.drainOnce.Do(func() {
		p.mu.Lock()
		p.draining = true
		close(p.drainCh)
		if !p.starting && !p.submissionsDone {
			p.submissionsDone = true
		}
		p.mu.Unlock()
		p.completeTasksIfReady()
	})
	select {
	case <-p.tasksDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cron: drain running jobs: %w", ctx.Err())
	}
}

// stop is safe before Init, after a partially admitted Start, and on repeated
// or concurrent calls. A timed-out call does not freeze cleanup: a later call
// can continue waiting, and the final managed task closes owned resources when
// it eventually quiesces.
func (p *Plugin) stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.requestStop()

	select {
	case <-p.tasksDone:
		p.finalize()
		<-p.finalizeDone
		p.mu.Lock()
		err := p.stopErr
		p.mu.Unlock()
		return err
	case <-ctx.Done():
		return fmt.Errorf("cron: wait for managed tasks: %w", ctx.Err())
	}
}

func (p *Plugin) requestStop() {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	close(p.stopCh)
	cancel := p.runCancel
	if !p.starting && !p.submissionsDone {
		p.submissionsDone = true
	}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.completeTasksIfReady()
}

func (p *Plugin) finalize() {
	p.finalizeOnce.Do(func() {
		var result error
		if p.ownedRedis != nil {
			if err := p.ownedRedis.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
				result = fmt.Errorf("cron: close owned Redis client: %w", err)
			}
		}
		p.mu.Lock()
		p.stopErr = result
		p.mu.Unlock()
		close(p.finalizeDone)
	})
}

func (p *Plugin) completeTasksIfReady() {
	p.mu.Lock()
	ready := p.submissionsDone && p.taskCount == 0
	stopping := p.stopping
	p.mu.Unlock()
	if !ready {
		return
	}
	if stopping {
		p.finalize()
	}
	p.tasksDoneOnce.Do(func() { close(p.tasksDone) })
}

func (p *Plugin) invokeJob(ctx context.Context, logger log.Logger, entry *scheduledJob) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("cron: job panic recovered",
				"job", entry.label,
				"panic", fmt.Sprint(recovered),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("cron: job %s panicked: %v", entry.label, recovered)
		}
	}()
	if err := entry.job.Run(ctx); err != nil {
		logger.Error("cron: job failed", "job", entry.label, "error", err)
		return err
	}
	return nil
}
