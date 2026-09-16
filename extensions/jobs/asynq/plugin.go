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

// Plugin owns one Redis connection, enqueue client, and worker server. Its
// Enqueuer contract is available to dependants after Init succeeds. A Plugin
// must not be copied after first use.
type Plugin struct {
	cfg        Config
	factory    backendFactory
	dispatcher *dispatcher

	mu       sync.Mutex
	workerMu sync.Mutex

	redis       *goredis.Client
	client      *Client
	server      workerServer
	initialized bool
	starting    bool
	started     bool
	opened      bool
	stopping    bool
	stopped     bool
	stopDone    chan struct{}
	stopErr     error
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
	dispatcher, err := collectHandlers(contributors)
	if err != nil {
		return nil, err
	}
	return &Plugin{
		cfg:        cfg.clone(),
		factory:    defaultBackendFactory(),
		dispatcher: dispatcher,
	}, nil
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
	server := p.factory.newServer(redisClient, hibiken.Config{
		Concurrency:       cfg.Concurrency,
		Queues:            maps.Clone(cfg.Queues),
		StrictPriority:    cfg.StrictPriority,
		TaskCheckInterval: cfg.TaskCheckInterval,
		ShutdownTimeout:   cfg.ShutdownTimeout,
		Logger:            asynqLogger{logger: ctx.Log()},
	})
	if isNilInterface(server) {
		return fmt.Errorf("asynq: initialize: worker server factory returned nil")
	}

	p.cfg = cfg
	p.redis = redisClient
	p.client = newClient(backend, cfg)
	p.server = server
	p.initialized = true
	committed = true
	return nil
}

// start admits the worker as one critical managed task. The task itself waits
// on XBC's global traffic gate before polling Redis, so task submission occurs
// only inside Start while no work is consumed before all traffic preparation
// succeeds.
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
	case p.server == nil || p.dispatcher == nil:
		p.mu.Unlock()
		return fmt.Errorf("asynq: Start called without a prepared worker")
	}
	p.starting = true
	server := p.server
	dispatcher := p.dispatcher
	logger := ctx.Log()
	p.mu.Unlock()

	accepted := ctx.GoCritical(func(taskCtx context.Context) {
		p.runWorker(taskCtx, gate, server, dispatcher, logger)
	})
	if !accepted {
		p.mu.Lock()
		p.starting = false
		p.mu.Unlock()
		return fmt.Errorf("asynq: worker task was not accepted during Start")
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

func (p *Plugin) runWorker(taskCtx context.Context, gate <-chan struct{}, server workerServer, dispatcher *dispatcher, logger interface {
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
		if p.stopping || p.stopped || p.server != server {
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()

		if err := server.Start(dispatcher); err != nil {
			return fmt.Errorf("asynq: start worker: %w", err)
		}
		p.mu.Lock()
		p.opened = true
		p.mu.Unlock()
		return nil
	}()
	if startErr != nil {
		logger.Error("asynq worker terminated during startup", "error", startErr)
		return
	}

	<-taskCtx.Done()
}

// stop closes enqueue admission, gracefully drains the worker, and closes the
// owned Redis connection. It is safe in every factory-owned partial state and
// idempotent under repeated or concurrent calls.
func (p *Plugin) stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	p.mu.Lock()
	owner := false
	if p.stopDone == nil {
		owner = true
		p.stopping = true
		p.stopDone = make(chan struct{})
		client := p.client
		server := p.server
		redisClient := p.redis
		if client != nil {
			client.beginClose()
		}
		p.client = nil
		p.server = nil
		p.redis = nil
		p.mu.Unlock()
		p.finishStop(client, server, redisClient)
	} else {
		p.mu.Unlock()
	}

	p.mu.Lock()
	done := p.stopDone
	p.mu.Unlock()
	if owner {
		<-done
	} else {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	err := p.stopErr
	p.mu.Unlock()
	return err
}

func (p *Plugin) finishStop(client *Client, server workerServer, redisClient *goredis.Client) {
	if client != nil {
		client.close()
	}
	var errs []error
	if server != nil {
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
	p.dispatcher = nil
	close(p.stopDone)
	p.mu.Unlock()
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
