package asynq

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of the Asynq plugin.
const Key plugin.Key = "asynq"

var handlerContributors = plugin.Collect[HandlerContributor]()

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	buildPlugin,
	plugin.Options[*Plugin]{
		Instances:  plugin.SingleInstance,
		Activation: plugin.WhenConfigured("plugins.asynq"),
		Inputs:     plugin.Inputs(handlerContributors),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(value *Plugin) Enqueuer { return value }),
			plugin.ExportAs(func(value *Plugin) health.Contributor { return value }),
		),
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Init:  (*Plugin).init,
			Start: (*Plugin).start,
			Drain: (*Plugin).drain,
			Stop:  (*Plugin).stop,
		},
	},
)

// Definition returns this integration's canonical Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this integration's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return plugin.BundleOf(definition) }

type workerServer interface {
	Start(hibiken.Handler) error
	Stop()
	Shutdown()
}

type backendFactory struct {
	newRedis  func(*goredis.Options) *goredis.Client
	newClient func(goredis.UniversalClient) enqueueBackend
	newServer func(goredis.UniversalClient, hibiken.Config) workerServer
}

func defaultBackendFactory() backendFactory {
	return backendFactory{
		newRedis: goredis.NewClient,
		newClient: func(client goredis.UniversalClient) enqueueBackend {
			return hibiken.NewClientFromRedisClient(client)
		},
		newServer: func(client goredis.UniversalClient, cfg hibiken.Config) workerServer {
			return hibiken.NewServerFromRedisClient(client, cfg)
		},
	}
}

// workerGroup is one worker's share of the integration: the handler set of the
// Plugins that share a workload, the queues it consumes, and its own lifecycle
// state. Its server is created in Init, started by the managed task that group
// owns, and stopped by drain and stop.
type workerGroup struct {
	// workload is the workload this group serves, empty for the group of
	// contributors that belong to none.
	workload plugin.WorkloadKey
	// queues is the queue set this group's server consumes, with hibiken's
	// relative-weight semantics. It is disjoint from every other group's set.
	queues map[string]int
	// concurrency is how many tasks this group's server runs at once.
	concurrency int
	// dispatcher routes a dequeued task to the handler that registered its type.
	dispatcher *dispatcher
	// handlers counts the handler invocations currently in flight, so drain can
	// wait for work this worker already accepted without cancelling it.
	handlers *handlerTracker
	// admission charges one unit of the workload's quota for the duration of
	// every handler invocation. It is never nil: a workload that declares no
	// budget, like a contributor that belongs to none, gets a limiter that
	// admits immediately.
	admission plugin.Admission
	// server is the worker's server. It is nil before Init built it and again
	// once stop has taken it away; a managed task that finds it nil has lost
	// the race with a shutdown and must not start it.
	server workerServer
}

// Plugin owns one Redis connection and enqueue client, plus one worker per
// group of contributing Plugins. Its Enqueuer contract is available to
// dependants after Init succeeds. A Plugin must not be copied after first use.
type Plugin struct {
	cfg     Config
	factory backendFactory

	mu       sync.Mutex
	workerMu sync.Mutex

	redis       *goredis.Client
	client      *Client
	groups      []*workerGroup
	initialized bool
	starting    bool
	started     bool
	stopping    bool
	stopped     bool
	stopDone    chan struct{}
	stopErr     error

	// drainDone is closed once every worker has been told to stop fetching new
	// tasks; drainErr is the error that left behind, replayed to every later
	// drain call so drain is idempotent including its result. The handlers that
	// were still running are waited for per drain call, within that call's own
	// context, and any that outlive the drain budget are left to stop.
	drainDone chan struct{}
	drainErr  error
}

var _ Enqueuer = (*Plugin)(nil)

func prepareConfig(cfg Config) (Config, error) {
	cfg = cfg.clone()
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func buildPlugin(ctx plugin.BuildContext, cfg Config) (*Plugin, error) {
	return newPlugin(cfg, handlerContributors.Get(ctx))
}

func newPlugin(cfg Config, contributors []plugin.Entry[HandlerContributor]) (*Plugin, error) {
	groups, err := newWorkerGroups(cfg, contributors)
	if err != nil {
		return nil, err
	}
	return &Plugin{
		cfg:     cfg.clone(),
		factory: defaultBackendFactory(),
		groups:  groups,
	}, nil
}

// newWorkerGroups resolves every handler group into the worker that serves it.
//
// The unowned group takes the top-level queues and concurrency, which is what
// every composition that declares no workload keeps. A workload group takes its
// own entry, and a workload whose Plugins contribute handlers without one is a
// configuration error rather than a silent fallback to the top-level queues:
// falling back would put the workload's handlers on a queue set shared with
// everything unowned, which is the cross-consumption this split exists to stop.
func newWorkerGroups(cfg Config, contributors []plugin.Entry[HandlerContributor]) ([]*workerGroup, error) {
	grouped, err := groupHandlers(contributors)
	if err != nil {
		return nil, err
	}
	groups := make([]*workerGroup, 0, len(grouped))
	for _, group := range grouped {
		queues := cfg.Queues
		concurrency := cfg.Concurrency
		if group.workload != "" {
			declared, ok := cfg.Workloads[string(group.workload)]
			if !ok {
				return nil, fmt.Errorf("asynq: workload %q contributes task handlers but plugins.asynq.workloads.%s is not configured; declare the queues its worker consumes", group.workload, group.workload)
			}
			queues = declared.Queues
			if declared.Concurrency > 0 {
				concurrency = declared.Concurrency
			}
		}
		groups = append(groups, &workerGroup{
			workload:    group.workload,
			queues:      maps.Clone(queues),
			concurrency: concurrency,
			dispatcher:  group.dispatcher,
			handlers:    newHandlerTracker(),
		})
	}
	if err := requireDisjointQueues(groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// requireDisjointQueues refuses a composition in which two workers consume the
// same queue.
//
// A queue fetched by two servers in one process is delivered to whichever of
// them polls it first, so the handler that runs would be decided by a race --
// and a task meant for one workload would spend another workload's quota. The
// queue sets are configuration, so this is checked where they become groups
// rather than at every delivery, and the error names both groups.
func requireDisjointQueues(groups []*workerGroup) error {
	owner := make(map[string]plugin.WorkloadKey)
	for _, group := range groups {
		for queue := range group.queues {
			if previous, taken := owner[queue]; taken {
				return fmt.Errorf("asynq: queue %q is consumed by both %s and %s; a queue belongs to one worker", queue, groupName(previous), groupName(group.workload))
			}
			owner[queue] = group.workload
		}
	}
	return nil
}

// groupName names a group in operator-facing text.
func groupName(workload plugin.WorkloadKey) string {
	if workload == "" {
		return "the unowned group"
	}
	return fmt.Sprintf("workload %q", workload)
}

// Enqueue persists a task through the integration-owned client.
func (p *Plugin) Enqueue(ctx context.Context, task Task, options ...TaskOption) (TaskInfo, error) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		return TaskInfo{}, ErrClosed
	}
	return client.Enqueue(ctx, task, options...)
}

// init connects to Redis and constructs the enqueue and worker clients. The
// primary Plugin is already framework-owned, so every failure leaves state
// safe for the lifecycle's mandatory Stop call.
func (p *Plugin) init(ctx *plugin.Context) (err error) {
	if ctx == nil {
		return fmt.Errorf("asynq: initialize: nil plugin context")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initialized || p.redis != nil {
		return fmt.Errorf("asynq: initialize: plugin is already initialized")
	}
	if p.stopping || p.stopped {
		return fmt.Errorf("asynq: initialize: plugin has stopped")
	}

	cfg := p.cfg.clone()
	redisClient := p.factory.newRedis(cfg.Redis.options())
	if redisClient == nil {
		return fmt.Errorf("asynq: initialize: Redis factory returned nil")
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if closeErr := redisClient.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("asynq: close Redis after initialization failure: %w", closeErr))
		}
	}()

	if pingErr := redisClient.Ping(ctx).Err(); pingErr != nil {
		return fmt.Errorf("asynq: ping Redis at %s: %w", cfg.Redis.Addr, pingErr)
	}

	backend := p.factory.newClient(redisClient)
	if isNilInterface(backend) {
		return fmt.Errorf("asynq: initialize: enqueue backend factory returned nil")
	}
	for _, group := range p.groups {
		server := p.factory.newServer(redisClient, hibiken.Config{
			Concurrency:       group.concurrency,
			Queues:            maps.Clone(group.queues),
			StrictPriority:    cfg.StrictPriority,
			TaskCheckInterval: cfg.TaskCheckInterval,
			ShutdownTimeout:   cfg.ShutdownTimeout,
			Logger:            asynqLogger{logger: ctx.Log()},
		})
		if isNilInterface(server) {
			return fmt.Errorf("asynq: initialize: worker server factory returned nil")
		}
		group.server = server
		group.admission = groupAdmission(ctx, group.workload)
	}

	p.cfg = cfg
	p.redis = redisClient
	p.client = newClient(backend, cfg)
	p.initialized = true
	committed = true
	return nil
}

// groupAdmission reports the limiter a group's handler invocations charge.
//
// A contributor that belongs to no workload charges nothing, which is the same
// answer Context.Admission gives for an unowned plugin. A contributor that
// belongs to one charges that workload's quota even though the submitting
// Plugin is the integration rather than the contributor: the entry a
// HandlerContributor was collected as carries the workload, and the work runs
// on that workload's behalf. This is what keeps a shared worker plugin -- one
// that serves several workloads in one process -- from escaping the budgets
// those workloads declared.
func groupAdmission(ctx *plugin.Context, workload plugin.WorkloadKey) plugin.Admission {
	if workload == "" {
		return ctx.Admission()
	}
	return ctx.AdmissionFor(workload)
}

// start admits one critical managed task per worker group. Each task waits on
// XBC's global traffic gate before polling Redis, so task submission occurs
// only inside Start while no work is consumed before all traffic preparation
// succeeds.
//
// A process whose Plugins contribute no handlers still submits one critical
// task, which does nothing but wait for the stop. It is not a decoration: the
// runtime refuses to hand a process to its signal loop when nothing provides a
// long-lived capability, and a worker that is between roles -- a standby
// waiting for a restart that will give it handlers -- is exactly such a
// process. Refusing to start was what made the takeover model unusable, since
// the process that must survive to be restarted is the one with no work.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return fmt.Errorf("asynq: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("asynq: Start canceled: %w", err)
	}
	gate := ctx.TrafficGate()
	if gate == nil {
		return fmt.Errorf("asynq: Start requires a runtime traffic gate")
	}

	p.mu.Lock()
	switch {
	case !p.initialized:
		p.mu.Unlock()
		return fmt.Errorf("asynq: Start called before Init")
	case p.starting || p.started:
		p.mu.Unlock()
		return fmt.Errorf("asynq: Start called more than once")
	case p.stopping || p.stopped:
		p.mu.Unlock()
		return fmt.Errorf("asynq: cannot Start after Stop")
	case p.drainDone != nil:
		p.mu.Unlock()
		return fmt.Errorf("asynq: cannot Start after Drain")
	}
	for _, group := range p.groups {
		if group.server == nil || group.dispatcher == nil {
			p.mu.Unlock()
			return fmt.Errorf("asynq: Start called without a prepared worker")
		}
	}
	p.starting = true
	groups := append([]*workerGroup(nil), p.groups...)
	logger := ctx.Log()
	p.mu.Unlock()

	if len(groups) == 0 {
		if !ctx.GoCritical(func(taskCtx context.Context) { <-taskCtx.Done() }) {
			p.mu.Lock()
			p.starting = false
			p.mu.Unlock()
			return fmt.Errorf("asynq: idle worker task was not accepted during Start")
		}
	} else {
		for _, group := range groups {
			accepted := ctx.GoCritical(func(taskCtx context.Context) {
				p.runWorker(taskCtx, gate, group, logger)
			})
			if !accepted {
				p.mu.Lock()
				p.starting = false
				p.mu.Unlock()
				return fmt.Errorf("asynq: worker task for %s was not accepted during Start", groupName(group.workload))
			}
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping || p.stopped {
		p.starting = false
		return fmt.Errorf("asynq: cannot finish Start after Stop")
	}
	p.starting = false
	p.started = true
	return nil
}

func (p *Plugin) runWorker(taskCtx context.Context, gate <-chan struct{}, group *workerGroup, logger interface {
	Error(string, ...any)
}) {
	select {
	case <-gate:
	case <-taskCtx.Done():
		return
	}

	p.workerMu.Lock()
	startErr := func() error {
		defer p.workerMu.Unlock()
		p.mu.Lock()
		server := group.server
		if p.stopping || p.stopped || p.drainDone != nil || server == nil {
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()

		// The tracker is the outer wrapper: a task this worker already dequeued
		// counts as accepted work for the whole of its invocation, including
		// the wait for a quota slot, so drain never reports idle while a task
		// the worker took is still pending.
		handler := group.handlers.wrap(admittedHandler{admission: group.admission, inner: group.dispatcher})
		if err := server.Start(handler); err != nil {
			return fmt.Errorf("asynq: start worker for %s: %w", groupName(group.workload), err)
		}
		return nil
	}()
	if startErr != nil {
		logger.Error("asynq worker terminated during startup", "error", startErr)
		return
	}

	<-taskCtx.Done()
}

// drain stops the worker from fetching new tasks and waits, within ctx, for the
// handler invocations already accepted to return. It is the graceful half of
// shutdown: Enqueue and the owned Redis connection stay usable until stop, so a
// plugin that is still draining its own work may keep enqueueing follow-up
// tasks.
//
// The wait is best-effort at the fetch boundary: asynq's Server.Stop only stops
// the processor's fetch loop and does not wait for a worker goroutine that
// already dequeued a task, so a task dequeued just before the worker stopped
// can begin after the tracker saw no invocation in flight. Stop's library
// Shutdown is the backstop for that window -- it waits for every worker and
// requeues whatever exceeds plugins.asynq.shutdown_timeout, so such a task is
// finished or requeued, never lost.
//
// An expired context means stop waiting, never abort: handlers that outlive
// the drain budget are left running, with their contexts untouched, for stop
// to cancel. Every worker is told to stop exactly once, on its own goroutine
// because the library call is not context-aware; every later drain call
// replays that call's error and then waits for the handlers it left. A worker
// that never opened is stopped as well, so the split is safe before Start,
// after a failed Start, and while the background task still waits for the
// traffic gate.
func (p *Plugin) drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	started := p.drainDone == nil
	var servers []workerServer
	var groups []*workerGroup
	if started {
		p.drainDone = make(chan struct{})
		groups = append([]*workerGroup(nil), p.groups...)
		for _, group := range groups {
			if group.server != nil {
				servers = append(servers, group.server)
			}
		}
	}
	done := p.drainDone
	p.mu.Unlock()
	if started {
		go p.stopWorkersForDrain(servers, done)
	}

	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("asynq: drain running handlers: %w", ctx.Err())
	}

	p.mu.Lock()
	err := p.drainErr
	if !started {
		groups = append([]*workerGroup(nil), p.groups...)
	}
	p.mu.Unlock()

	// A failed worker stop does not mean the handlers already accepted have
	// returned: wait for them first and report both outcomes joined, so the
	// failure never turns into a shortcut that abandons running work. Every
	// group is waited for within the same context, so their waits overlap
	// rather than dividing the drain budget between them.
	var waits []error
	for _, group := range groups {
		if waitErr := group.handlers.waitIdle(ctx); waitErr != nil {
			waits = append(waits, fmt.Errorf("asynq: drain running handlers of %s: %w", groupName(group.workload), waitErr))
		}
	}
	return errors.Join(err, errors.Join(waits...))
}

// stopWorkersForDrain stops every worker from fetching new tasks and records
// the result for every drain call to replay. It shares workerMu with the
// workers' Start, so a worker that is already being brought up is stopped after
// it opened, and one that has not started yet never opens after the drain.
func (p *Plugin) stopWorkersForDrain(servers []workerServer, done chan struct{}) {
	var errs []error
	for _, server := range servers {
		p.workerMu.Lock()
		if err := stopWorker(server); err != nil {
			errs = append(errs, err)
		}
		p.workerMu.Unlock()
	}
	p.mu.Lock()
	p.drainErr = errors.Join(errs...)
	close(done)
	p.mu.Unlock()
}

// stop closes enqueue admission, shuts the worker down through the asynq
// library, and closes the owned Redis connection. It is safe in every
// factory-owned partial state and idempotent under repeated or concurrent
// calls. The shutdown gives the handlers that outlived drain another
// plugins.asynq.shutdown_timeout to finish and requeues whatever still
// exceeds it; Redis is closed only after the worker stopped. Stop never waits
// for drain to finish: drain waits for handlers, and only stop can release
// the ones it left.
//
// Cleanup runs on its own goroutine, and every caller, including the one that
// started it, waits on the shared completion within its own context: a caller
// whose context expires returns its error while cleanup continues, and a
// later call reports the final result.
func (p *Plugin) stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	if p.stopDone == nil {
		p.stopping = true
		p.stopDone = make(chan struct{})
		client := p.client
		var servers []workerServer
		for _, group := range p.groups {
			if group.server != nil {
				servers = append(servers, group.server)
				// Clearing the field is what tells a managed task that has not
				// reached its Start yet that the worker it would start is no
				// longer this plugin's to run.
				group.server = nil
			}
		}
		redisClient := p.redis
		if client != nil {
			client.beginClose()
		}
		p.client = nil
		p.redis = nil
		p.mu.Unlock()
		// Nothing cancels the handlers' contexts here. Drain left them running
		// on purpose, and the library Shutdown below is what gives them their
		// remaining ShutdownTimeout to finish; cancelling them first would not
		// abort the handlers, it would make asynq treat their tasks as failed
		// and requeue them while the handler goroutines were still working.
		// That same library timeout is what ends a handler parked on its
		// workload's admission quota, by cancelling the context it waits on.
		go p.finishStop(client, servers, redisClient)
	} else {
		p.mu.Unlock()
	}

	p.mu.Lock()
	done := p.stopDone
	p.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	err := p.stopErr
	p.mu.Unlock()
	return err
}

func (p *Plugin) finishStop(client *Client, servers []workerServer, redisClient *goredis.Client) {
	if client != nil {
		client.close()
	}
	var errs []error
	for _, server := range servers {
		p.workerMu.Lock()
		if err := shutdownWorker(server); err != nil {
			errs = append(errs, err)
		}
		p.workerMu.Unlock()
	}
	if redisClient != nil {
		if err := redisClient.Close(); err != nil {
			errs = append(errs, fmt.Errorf("asynq: close Redis: %w", err))
		}
	}

	p.mu.Lock()
	p.stopErr = errors.Join(errs...)
	p.stopping = false
	p.stopped = true
	p.initialized = false
	p.starting = false
	p.groups = nil
	close(p.stopDone)
	p.mu.Unlock()
}

func stopWorker(server workerServer) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("asynq: worker stop panicked: %v", recovered)
		}
	}()
	server.Stop()
	return nil
}

func shutdownWorker(server workerServer) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("asynq: worker shutdown panicked: %v", recovered)
		}
	}()
	server.Shutdown()
	return nil
}

// handlerTracker counts the handler invocations currently in flight. The
// worker hands every task through wrap, so once an invocation is counted it is
// covered exactly; drain waits on the count to let work the worker already
// accepted finish without cancelling it.
//
// The count is best-effort at the fetch boundary. asynq v0.26.0's Server.Stop
// only stops the processor's fetch loop and does not wait for a worker
// goroutine that already dequeued a task, so a task dequeued just before Stop
// returned can still reach wrap and call begin after waitIdle observed the
// count at zero. That task is not lost: the library Shutdown in stop is the
// backstop, waiting for every worker and requeueing whatever exceeds
// plugins.asynq.shutdown_timeout.
type handlerTracker struct {
	mu     sync.Mutex
	active int
	idle   chan struct{}
}

func newHandlerTracker() *handlerTracker {
	tracker := &handlerTracker{idle: make(chan struct{})}
	close(tracker.idle)
	return tracker
}

// wrap returns a handler that keeps the in-flight count for the duration of
// every invocation.
func (t *handlerTracker) wrap(inner hibiken.Handler) hibiken.Handler {
	return hibiken.HandlerFunc(func(ctx context.Context, task *hibiken.Task) error {
		t.begin()
		defer t.end()
		return inner.ProcessTask(ctx, task)
	})
}

func (t *handlerTracker) begin() {
	t.mu.Lock()
	if t.active == 0 {
		t.idle = make(chan struct{})
	}
	t.active++
	t.mu.Unlock()
}

func (t *handlerTracker) end() {
	t.mu.Lock()
	t.active--
	if t.active == 0 {
		close(t.idle)
	}
	t.mu.Unlock()
}

// waitIdle blocks until no handler invocation is in flight, or ctx is done.
// Callers must have stopped the worker from fetching new tasks first;
// otherwise a handler that starts after the count was observed at zero is
// missed. Even with the fetch loop stopped the observation is best-effort:
// asynq may start a task dequeued just before Server.Stop returned after this
// wait saw an idle tracker (see handlerTracker), and only the library Shutdown
// in stop covers that invocation.
func (t *handlerTracker) waitIdle(ctx context.Context) error {
	for {
		t.mu.Lock()
		if t.active == 0 {
			t.mu.Unlock()
			return nil
		}
		idle := t.idle
		t.mu.Unlock()

		select {
		case <-idle:
			// A handler finished; re-check in case another started.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
