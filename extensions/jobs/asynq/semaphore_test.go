package asynq

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	hibiken "github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/tasks"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

func newSemaphoreTestClient(t *testing.T, server *miniredis.Miniredis) *goredis.Client {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestDispatcherClusterConcurrencyCapsParallelHandlers(t *testing.T) {
	server := miniredis.RunT(t)
	client := newSemaphoreTestClient(t, server)

	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	handler := HandlerFunc(func(ctx context.Context, _ Task) error {
		current := active.Add(1)
		for {
			observed := peak.Load()
			if current <= observed || peak.CompareAndSwap(observed, current) {
				break
			}
		}
		defer active.Add(-1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	const deferDelay = 250 * time.Millisecond
	dispatch := &dispatcher{
		handlers: map[string]Handler{"work": handler},
		limits: map[string]clusterLimit{
			"work": {limit: 2, lease: 5 * time.Second, retryDelay: deferDelay},
		},
		semaphore: newClusterSemaphore(client, corelog.Nop()),
	}

	var returned atomic.Int32
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			err := dispatch.ProcessTask(context.Background(), hibiken.NewTask("work", nil))
			returned.Add(1)
			errs <- err
		}()
	}

	// Two executions hold the two slots and are parked in the handler; the
	// other two must have returned without entering it.
	waitForCondition(t, 2*time.Second, func() bool {
		return returned.Load() == 2 && active.Load() == 2
	}, "two deferred deliveries and two running handlers")

	retryFunc := newRetryDelayFunc(time.Second, 60*time.Second)
	for range 2 {
		err := <-errs
		if !errors.Is(err, ErrClusterConcurrencyLimit) {
			t.Fatalf("deferred delivery error = %v, want ErrClusterConcurrencyLimit", err)
		}
		delay, ok := retryDelay(err)
		if !ok || delay != deferDelay {
			t.Fatalf("deferred delivery delay = %s/%v, want %s", delay, ok, deferDelay)
		}
		if got := retryFunc(0, err, nil); got != deferDelay {
			t.Fatalf("RetryDelayFunc(deferred) = %s, want the configured %s", got, deferDelay)
		}
	}

	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("admitted delivery error = %v", err)
		}
	}
	if got := peak.Load(); got > 2 {
		t.Fatalf("peak concurrent handlers = %d, want at most 2", got)
	}
	if card := client.ZCard(context.Background(), clusterSemaphoreKey("work")).Val(); card != 0 {
		t.Fatalf("slots left after every handler returned = %d, want 0", card)
	}
}

func TestClusterSemaphoreRecoversExpiredSlots(t *testing.T) {
	server := miniredis.RunT(t)
	client := newSemaphoreTestClient(t, server)
	semaphore := newClusterSemaphore(client, corelog.Nop())
	ctx := context.Background()

	// A replica that died holds the only slot. Nobody will ever release it.
	if _, acquired, err := semaphore.tryAcquire(ctx, "work", 1, 100*time.Millisecond); err != nil || !acquired {
		t.Fatalf("first acquisition = %v/%v", acquired, err)
	}
	if _, acquired, err := semaphore.tryAcquire(ctx, "work", 1, time.Second); err != nil || acquired {
		t.Fatalf("acquisition while the slot is live = %v/%v, want refused", acquired, err)
	}

	time.Sleep(150 * time.Millisecond)
	if _, acquired, err := semaphore.tryAcquire(ctx, "work", 1, time.Second); err != nil || !acquired {
		t.Fatalf("acquisition after the lease lapsed = %v/%v, want admitted", acquired, err)
	}
}

func TestClusterSemaphoreRecoversAForeignRedisType(t *testing.T) {
	server := miniredis.RunT(t)
	client := newSemaphoreTestClient(t, server)
	semaphore := newClusterSemaphore(client, corelog.Nop())
	ctx := context.Background()
	key := clusterSemaphoreKey("work")

	if err := client.Set(ctx, key, "claimed by another writer", 0).Err(); err != nil {
		t.Fatalf("seed foreign key error = %v", err)
	}
	token, acquired, err := semaphore.tryAcquire(ctx, "work", 1, time.Second)
	if err != nil || !acquired || token == "" {
		t.Fatalf("acquisition over a foreign type = %v/%v/%q", acquired, err, token)
	}
	if typ := client.Type(ctx, key).Val(); typ != "zset" {
		t.Fatalf("key type after recovery = %q, want zset", typ)
	}
	if card := client.ZCard(ctx, key).Val(); card != 1 {
		t.Fatalf("recovered key card = %d, want the new slot", card)
	}
}

func TestClusterSemaphoreRenewalLossCancelsHandlerContext(t *testing.T) {
	server := miniredis.RunT(t)
	client := newSemaphoreTestClient(t, server)
	semaphore := newClusterSemaphore(client, corelog.Nop())
	key := clusterSemaphoreKey("work")
	ctx := context.Background()

	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- semaphore.run(ctx, "work", clusterLimit{limit: 1, lease: 150 * time.Millisecond, retryDelay: time.Second}, func(runCtx context.Context) error {
			close(started)
			<-runCtx.Done()
			return runCtx.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	// Remove the slot as a reset or an operator would; the renewal loop then
	// observes that the token is gone.
	if err := client.Del(ctx, key).Err(); err != nil {
		t.Fatalf("delete semaphore key error = %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler result = %v, want the cancelled context", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a lost lease did not cancel the handler context")
	}
}

func TestClusterLimitAppliesToFunctionTasksAndContributorHandlers(t *testing.T) {
	server := miniredis.RunT(t)
	client := newSemaphoreTestClient(t, server)

	contributorDone := make(chan struct{})
	contributorHandler := HandlerFunc(func(ctx context.Context, _ Task) error {
		close(contributorDone)
		<-ctx.Done()
		return ctx.Err()
	})
	binding := funcBinding(t, "test.asynq.cluster.function")
	groups, err := groupHandlers(
		[]plugin.Entry[HandlerContributor]{{
			Identity: plugin.Identity{Plugin: "business"},
			Value:    &testContributor{registrations: []HandlerRegistration{{Type: "contrib.work", Handler: contributorHandler}}},
		}},
		[]taskBinding{{binding: binding, owner: plugin.Identity{Plugin: Key}}},
	)
	if err != nil {
		t.Fatalf("groupHandlers() error = %v", err)
	}
	cfg := defaultConfig()
	cfg.ClusterConcurrencyRetryDelay = 2 * time.Second
	cfg.Tasks = map[string]TaskConfig{
		"contrib.work":                {ClusterConcurrency: 1},
		"test.asynq.cluster.function": {ClusterConcurrency: 1},
	}
	applyClusterLimits(cfg, groups)
	if len(groups) != 1 || len(groups[0].dispatcher.limits) != 2 {
		t.Fatalf("groups/limits = %d/%v, want one group with two caps", len(groups), groups[0].dispatcher.limits)
	}
	dispatch := groups[0].dispatcher
	dispatch.semaphore = newClusterSemaphore(client, corelog.Nop())

	contributorCtx, cancelContributor := context.WithCancel(context.Background())
	contributorErr := make(chan error, 1)
	go func() {
		contributorErr <- dispatch.ProcessTask(contributorCtx, hibiken.NewTask("contrib.work", nil))
	}()
	select {
	case <-contributorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("contributor handler did not start")
	}
	assertClusterLimitDeferred(t, dispatch.ProcessTask(context.Background(), hibiken.NewTask("contrib.work", nil)), 2*time.Second)
	cancelContributor()
	select {
	case err := <-contributorErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("contributor handler error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contributor handler did not stop")
	}

	functionCtx, cancelFunction := context.WithCancel(context.Background())
	functionErr := make(chan error, 1)
	go func() {
		functionErr <- dispatch.ProcessTask(functionCtx, hibiken.NewTask("test.asynq.cluster.function", []byte("7")))
	}()
	waitForCondition(t, 2*time.Second, func() bool {
		return client.ZCard(context.Background(), clusterSemaphoreKey("test.asynq.cluster.function")).Val() == 1
	}, "the function task to take its slot")
	assertClusterLimitDeferred(t, dispatch.ProcessTask(context.Background(), hibiken.NewTask("test.asynq.cluster.function", []byte("7"))), 2*time.Second)
	cancelFunction()
	select {
	case err := <-functionErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("function task error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("function task did not stop")
	}
}

func assertClusterLimitDeferred(t *testing.T, err error, want time.Duration) {
	t.Helper()
	if !errors.Is(err, ErrClusterConcurrencyLimit) {
		t.Fatalf("error = %v, want ErrClusterConcurrencyLimit", err)
	}
	if delay, ok := retryDelay(err); !ok || delay != want {
		t.Fatalf("reported delay = %s/%v, want %s", delay, ok, want)
	}
}

func TestResolvedClusterLimitsUseTaskTimeoutAsLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.DefaultTimeout = 30 * time.Minute
	cfg.ClusterConcurrencyRetryDelay = 7 * time.Second
	cfg.Tasks = map[string]TaskConfig{
		"section-timeout": {ClusterConcurrency: 3},
		"task-timeout":    {ClusterConcurrency: 2, Timeout: 4 * time.Minute},
		"explicit-lease":  {ClusterConcurrency: 5, Timeout: 4 * time.Minute, ClusterConcurrencyLease: 90 * time.Second},
		"unbounded":       {Timeout: time.Minute},
	}
	limits := resolvedClusterLimits(cfg)
	want := map[string]clusterLimit{
		"section-timeout": {limit: 3, lease: 30 * time.Minute, retryDelay: 7 * time.Second},
		"task-timeout":    {limit: 2, lease: 4 * time.Minute, retryDelay: 7 * time.Second},
		"explicit-lease":  {limit: 5, lease: 90 * time.Second, retryDelay: 7 * time.Second},
	}
	if !reflect.DeepEqual(limits, want) {
		t.Fatalf("resolved limits = %#v, want %#v", limits, want)
	}
}

func TestInitAttachesSemaphoreAndLimitsToDispatchers(t *testing.T) {
	redisServer := miniredis.RunT(t)
	cfg := validTestConfig(redisServer.Addr())
	cfg.Tasks = map[string]TaskConfig{"work": {ClusterConcurrency: 2, Timeout: time.Minute}}
	p := newTestPlugin(t, cfg, HandlerFunc(func(context.Context, Task) error { return nil }))
	host := newTestHost()
	defer host.stopTasks()
	if err := p.init(testContext(host)); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	t.Cleanup(func() { _ = p.stop(context.Background()) })
	if p.semaphore == nil {
		t.Fatal("init() did not build the semaphore")
	}
	if len(p.groups) == 0 {
		t.Fatal("init() built no worker group")
	}
	for _, group := range p.groups {
		if group.dispatcher.semaphore != p.semaphore {
			t.Fatal("dispatcher is not sharing the plugin semaphore")
		}
		if got := group.dispatcher.limits["work"]; got.limit != 2 || got.lease != time.Minute {
			t.Fatalf("dispatcher limit = %+v, want 2 slots leased for 1m", got)
		}
	}
}

func TestResetSemaphoreKeepsLiveSlots(t *testing.T) {
	p, _, _ := initInspectionPlugin(t, nil, nil)
	ctx := context.Background()
	key := clusterSemaphoreKey("work")

	expired := float64(time.Now().Add(-time.Minute).UnixMilli())
	live := float64(time.Now().Add(time.Minute).UnixMilli())
	if err := p.redis.ZAdd(ctx, key, goredis.Z{Score: expired, Member: "expired-token"}).Err(); err != nil {
		t.Fatalf("seed expired token error = %v", err)
	}
	if err := p.redis.ZAdd(ctx, key, goredis.Z{Score: live, Member: "live-token"}).Err(); err != nil {
		t.Fatalf("seed live token error = %v", err)
	}
	if err := p.ResetSemaphore(ctx, "work"); err != nil {
		t.Fatalf("ResetSemaphore() error = %v", err)
	}
	members, err := p.redis.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		t.Fatalf("read semaphore key error = %v", err)
	}
	if !reflect.DeepEqual(members, []string{"live-token"}) {
		t.Fatalf("semaphore members after reset = %v, want only the live slot", members)
	}

	// A key claimed by a foreign type is dropped rather than left wedged.
	if err := p.redis.Set(ctx, clusterSemaphoreKey("foreign"), "junk", 0).Err(); err != nil {
		t.Fatalf("seed foreign key error = %v", err)
	}
	if err := p.ResetSemaphore(ctx, "foreign"); err != nil {
		t.Fatalf("ResetSemaphore(foreign) error = %v", err)
	}
	if typ := p.redis.Type(ctx, clusterSemaphoreKey("foreign")).Val(); typ != "none" {
		t.Fatalf("foreign key type after reset = %q, want none", typ)
	}
}

func TestConfigClusterConcurrencyDefaultsBindingAndValidation(t *testing.T) {
	cfg := bindAsynqConfig(t, map[string]any{})
	if cfg.ClusterConcurrencyRetryDelay != 5*time.Second {
		t.Fatalf("cluster_concurrency_retry_delay default = %s, want 5s", cfg.ClusterConcurrencyRetryDelay)
	}

	bound := bindAsynqConfig(t, map[string]any{
		"cluster_concurrency_retry_delay": "2s",
		"tasks": map[string]any{
			"work": map[string]any{"cluster_concurrency": 3, "cluster_concurrency_lease": "1m"},
		},
	})
	if bound.ClusterConcurrencyRetryDelay != 2*time.Second {
		t.Fatalf("bound retry delay = %s, want 2s", bound.ClusterConcurrencyRetryDelay)
	}
	if work := bound.Tasks["work"]; work.ClusterConcurrency != 3 || work.ClusterConcurrencyLease != time.Minute {
		t.Fatalf("bound task cluster config = %+v", work)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "zero retry delay", mutate: func(c *Config) { c.ClusterConcurrencyRetryDelay = 0 }, want: "cluster_concurrency_retry_delay"},
		{name: "negative limit", mutate: func(c *Config) {
			c.Tasks = map[string]TaskConfig{"work": {ClusterConcurrency: -1}}
		}, want: "cluster_concurrency cannot be negative"},
		{name: "negative lease", mutate: func(c *Config) {
			c.Tasks = map[string]TaskConfig{"work": {ClusterConcurrency: 1, ClusterConcurrencyLease: -time.Second}}
		}, want: "cluster_concurrency_lease cannot be negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := defaultConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

// testClusterFunction is defined once per test binary because tasks.New claims
// its name process-wide. The handler parks until its context is cancelled so a
// test can hold the task type's only slot.
var testClusterFunction = tasks.New("test.asynq.cluster.function", func(ctx context.Context, arg int) error {
	<-ctx.Done()
	return ctx.Err()
})

func funcBinding(t *testing.T, name string) tasks.Binding {
	t.Helper()
	for _, binding := range tasks.Funcs() {
		if binding.Name() == name {
			return binding
		}
	}
	t.Fatalf("no tasks.New binding named %q", name)
	return tasks.Binding{}
}
