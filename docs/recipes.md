# Deployment recipes

These recipes build on the [Quickstart](quickstart.md) and demonstrate explicit XBC compositions for production application topologies. They are examples, not implicit discovery: select every required Bundle at the composition root before adding its configuration section. Compose only the capabilities the application needs rather than selecting plugins in anticipation of future use. See [Web package documentation](https://pkg.go.dev/github.com/xbcio/xbc/transport/web) for the HTTP runtime contract.

## API keys and audit logging

The API-key plugin stores SHA-256 digests rather than plaintext keys and compares them with constant-time semantics. Deliver the actual key to the caller through a secure channel instead of storing it in YAML:

```yaml
plugins:
  apikey:
    header: "X-API-Key"
    allow_bearer: true
    min_key_bytes: 32
    static:
      - id: "partner-a-2026-08"
        app_id: "partner-a"
        subject: "partner-a-service"
        # Placeholder digest. Replace it with the 64-character hexadecimal
        # SHA-256 digest of the real key.
        sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  auditlog:
    async: true
    queue_size: 2048
    overflow: drop_newest
    sink_timeout: 2s
    request_id_header: "X-Request-ID"
```

Authentication is performed once by the framework's built-in authentication middleware; plugins contribute `authentication.Authenticator` and `web.CredentialExtractor` implementations but do not intercept requests themselves. Route access is decided by a three-tier precedence model: an explicit `web.security` policy rule (tier 1) overrides a route's `.Auth()` declaration (tier 2), and the global `web.security.default` (factory setting `deny`) covers routes that neither tier addresses. Protected routes receive the shared `authentication.Principal`, which lets audit records correlate the principal, route name, request ID, status code, and duration.

When a request's selection admits more than one scheme, which authenticator gets to try first is a security decision, not an implementation detail: it determines which credential wins when a client presents more than one. `web.security.schemes` makes that order an explicit, application-declared list rather than letting it fall out of registration or plugin-key order:

```yaml
web:
  security:
    schemes: ["jwt", "apikey"]
```

An application with two or more registered authenticators must declare `schemes` as a full permutation of their scheme names, or startup fails with `web.security.schemes must declare the arbitration order`; a missing, unknown, or duplicate entry fails startup the same way, naming the mistake. An application with exactly one registered authenticator may omit `schemes` entirely -- there is no arbitration to order.

For high-volume or dynamic credentials, inject an API-key repository instead of repeatedly editing static YAML.

## Idempotent write endpoints

Only routes explicitly marked `.Idempotent()` enter idempotency handling. A single-process development environment can use the in-memory backend:

```yaml
plugins:
  idempotency:
    backend: memory
    header: "Idempotency-Key"
    ttl: 24h
    pending_ttl: 30s
    max_request_bytes: 1048576
    max_response_bytes: 1048576
```

A multi-replica deployment must use shared Redis and select `redis.Bundle()` at the composition root:

```yaml
plugins:
  redis:
    coordination:
      addr: "redis.internal:6379"
      password: ""
      db: 2

  idempotency:
    backend: redis
    redis_instance: coordination
    redis_prefix: "orders:idempotency:"
    ttl: 24h
    pending_ttl: 30s
    operation_timeout: 2s
```

An idempotency key is not an authentication credential. Gateways and logs should not record unbounded request bodies or sensitive headers.

A stored response is keyed and fingerprinted by the authenticated Principal's Subject, so idempotent routes require an authenticated caller: a request carrying the `Idempotency-Key` header without a published Principal is refused with `403 idempotency_requires_principal` before the store is ever touched -- including on a public route marked `.Idempotent()`. A request without the header never reaches that refusal: a route marked `.Idempotent()` answers a missing or malformed header with `400 invalid_idempotency_key` exactly as it did before this rule, and the middleware does not act on any other route at all.

## Persistent Casbin policy with multi-replica synchronization

The Casbin integration is two independently selectable products. `github.com/xbcio/xbc/extensions/authorization/casbin` (key `casbin`) is the protocol-neutral engine: it reads static policy from inline content or a file, owns the adapter and watcher providers, and exports `rbac.Backend` and `EnforcerProvider` for callers with no HTTP surface at all. `github.com/xbcio/xbc/transport/web/extensions/authorization/casbin` (key `casbin-http`) is the Web middleware that enforces the engine on matched routes, and its `Bundle()` includes the engine. A service that must manage roles and permissions at runtime should explicitly compose `gorm.Bundle()`, `casbin-gorm.Bundle()`, `casbin-redis.Bundle()`, `casbinhttp.Bundle()`, and the protocol-neutral `rbac.Bundle()` from `github.com/xbcio/xbc/extensions/authorization/rbac`. Connect capabilities by exact plugin key and instance:

```yaml
plugins:
  gorm:
    primary:
      driver: postgres
      dsn: "host=db.internal user=app dbname=sas sslmode=require"

  casbin-gorm:
    policy:
      db_instance: primary
      table: sys_casbin_rule
      migrate: true

  casbin-redis:
    policy:
      mode: standalone
      addrs: ["redis.internal:6379"]
      channel: /casbin
      ignore_self: true
      # Supply password through deployment secrets.

  casbin:
    model: |
      [request_definition]
      r = sub, obj, act

      [policy_definition]
      p = sub, obj, act

      [role_definition]
      g = _, _

      [policy_effect]
      e = some(where (p.eft == allow))

      [matchers]
      m = g(r.sub, p.sub) && r.obj == p.obj && (p.act == "*" || r.act == p.act)
    request_convention: path_method
    adapter:
      plugin: casbin-gorm
      instance: policy
    watcher:
      plugin: casbin-redis
      instance: policy

  # The Web face of the engine: without a section of its own the middleware is
  # dormant and every matched route is served unauthorized. A service that only
  # manages policy through rbac.Manager omits this section.
  casbin-http:
    missing_permission: deny

  rbac:
    backend:
      plugin: casbin
      instance: default
    admin_role: admin
```

`casbin-gorm` defaults to `migrate: false` and never runs AutoMigrate implicitly during construction. When enabled, migration occurs only in XBC's Migrate stage. Casbin first loads policy at Start, after the table can exist. External-adapter mode enables AutoSave, so `AddPolicy`, role-relation changes, and filtered removals persist to the database. The Redis Watcher propagates changes to other replicas. The watcher owns its Redis clients; the named `gorm` plugin continues to own the GORM connection pool.

`github.com/xbcio/xbc/extensions/authorization/rbac` is a protocol-neutral application plugin with its own Definition, Config, Manager, Backend, Permission model, and autoload adapter. Application code should depend on `rbac.Manager` for administrator checks, AND/OR authorization, role and permission queries, and replacement or deletion of direct relations. The Casbin engine supplies its Backend. Depend directly on the engine's `casbin.EnforcerProvider` only for migrations or Casbin-specific advanced operations.

`github.com/xbcio/xbc/transport/web/extensions/authorization/rbac` is not a plugin. It is a thin adapter from `web.CurrentPrincipal` to `rbac.Manager` and supplies only `RequireAll` and `RequireAny`. Apply those functions explicitly to routes or groups. It owns no Definition, Config, Backend, or autoload registration and contributes no global authorization chain. If a concrete ABAC requirement emerges, it should be designed as a separate business plugin parallel to RBAC with its own transport adapters; XBC does not reserve placeholder ABAC directories or APIs.

Replacing a user's roles uses serialized remove/add operations because Casbin cannot atomically replace filtered grouping policy. The implementation attempts restore and reload on failure but cannot promise a transaction shared with application business tables.

XBC intentionally provides no generic RBAC administration HTTP API. User, role, and permission models; subject naming; administrative authentication; auditing; and transactions spanning policy and application tables belong to the application. A service-specific scheme such as `u<ID>`, `r<ID>`, `object#action`, and `sys_*` CRUD remains in that service. Do not place the Manager or Enforcer in a process-global variable.

Upstream `gorm-adapter/v3 v3.39.0` has a legacy `Adapter.Transaction` limitation: it rebuilds the adapter with `NewAdapterByDB` and resets a custom policy table to `casbin_rule`. A default-table transaction smoke test is covered. SAS currently fixes this in its main module with `replace github.com/casbin/gorm-adapter/v3 => ./internal/gorm-adapter`, so that replacement continues to apply when SAS migrates to XBC. Other consumers that rely on a custom table must use an upstream release containing the fix, carry an appropriate patch, or use a transaction-context API that preserves table metadata. XBC does not put local replacements or hidden forks in publishable modules.

## Server-side sessions and tenant selection

The session cookie carries only an opaque bearer ID generated from at least 32 random bytes. Identity and attributes remain server-side, and built-in stores index the ID by its SHA-256 digest rather than putting plaintext identity or an "encrypted object" in the cookie.

Use the `memory` backend only for a single-process deployment. Replicas must share Redis:

```yaml
plugins:
  redis:
    auth:
      addr: "redis.internal:6379"
      db: 4

  session:
    backend: redis
    redis_instance: auth
    redis_prefix: "orders:session:"
    name: "__Host-xbc_session"
    path: "/"
    http_only: true
    secure: true
    same_site: lax
    ttl: 24h
    idle_ttl: 30m
    touch_interval: 5m

  tenant:
    required: true
    header: "X-Tenant-ID"
    tenant_id_attribute: "tenant_id"
    tenant_ids_attribute: "tenant_ids"
    tenant_attributes_attribute: "tenant_attributes"
    auto_select_single: true
```

A login handler creates a session through `session.Manager.Create` and then calls `SetCookie`. Use atomic `Rotate` after privilege or authentication changes, and use `Revoke` plus `ClearCookie` during logout.

The server-side authentication flow must write `tenant_id` and `tenant_ids` session attributes. `X-Tenant-ID` merely selects one tenant from that verified membership set; it never proves membership. Anonymous requests receive 401 from the framework's built-in authentication middleware when the route resolves to deny; the tenant middleware itself only produces 403 for forged or unauthorized selections.

A session cookie is a bearer credential. Production deployments must use HTTPS and add appropriate SameSite and CSRF protection for browser requests that change state.

## Transactional outbox and Webhooks

Write business data and pending events with the same caller-owned GORM transaction. `outbox.Service.Enqueue(ctx, tx, event)` does not start or commit a transaction. The dispatcher claims a lease atomically in a short transaction, then publishes outside the database transaction. It therefore provides at-least-once delivery, not exactly-once delivery; consumers must deduplicate by `Event.ID`.

```yaml
plugins:
  gorm:
    primary:
      driver: postgres
      dsn: "host=db.internal user=app dbname=orders sslmode=require"

  outbox:
    db_instance: primary
    table: xbc_outbox_events
    migrate: true
    max_payload_bytes: 1048576
    worker:
      enabled: true
      poll_interval: 1s
      batch_size: 100
      concurrency: 4
      lease_duration: 30s
      renew_interval: 10s
      publish_timeout: 10s
      max_attempts: 10

  webhook:
    partner:
      workers: 4
      queue_size: 1024
      backpressure: block
      max_attempts: 5
      request_timeout: 10s
      max_payload_bytes: 1048576
      max_response_bytes: 65536
      allow_http: false
```

`migrate: true` permits table creation only during XBC's migration stage; it never mutates the schema implicitly during ordinary Init. An enabled worker requires the application to inject an `outbox.Publisher`, which can adapt a named Kafka producer or Webhook client.

A Webhook adapter should obtain destination URLs and secrets from a trusted subscription repository. Never let an event payload choose arbitrary credentials. The default Webhook transport permits only HTTPS and public addresses, validates resolved IPs at each actual dial, and validates every redirect target again. Response bodies, queues, and retries all have hard limits. Private destinations require an explicitly injected `EndpointPolicy`; `allow_http` alone cannot bypass the SSRF policy.

## Asynq and object storage

Do not place large files or long-running work in an HTTP handler or Redis task payload. Stream the object to storage first, enqueue only a stable object key and business ID, then let the handler retrieve the object and process it under a business idempotency key:

```yaml
plugins:
  objectstorage:
    assets:
      backend: s3
      max_object_bytes: 67108864
      max_list_items: 1000
      s3:
        endpoint: "s3.internal:9000"
        tls: true
        region: "us-east-1"
        bucket: "orders-assets"
        path_style: true
        # Supply access_key_id and secret_key through deployment secrets.

  asynq:
    redis:
      addr: "redis.internal:6379"
      db: 5
    queues:
      critical: 6
      default: 3
      low: 1
    default_queue: default
    concurrency: 10
    default_max_retries: 25
    default_timeout: 30m
    shutdown_timeout: 8s
```

The object-storage `local` backend is suitable for development and single-node deployments. It enforces a confined root, atomic temporary-file replacement, and streaming size limits. Replicas should use a shared S3-compatible backend.

Asynq freezes handler registration during construction and starts consuming only after `OpenTraffic`. The contributors that belong to a workload are consumed by that workload's own worker, with the queues and concurrency declared under `plugins.asynq.workloads.<key>`, while contributors that belong to none share the top-level worker and the top-level `queues`. Which process consumes a queue follows from which workloads it hosts, so a process that only enqueues for a workload it does not host is configured exactly the same way and simply has no worker for it; the queue sets of one process must not overlap, because a queue two workers both poll is delivered to whichever fetches first. Each delivery charges one unit of its workload's admission quota -- the same `workloads.<key>.max_goroutines` budget the workload's managed tasks charge -- and waits for a unit instead of failing the task when the quota is full. Size that quota above the workload's process-lifetime tasks: a quota the resident work alone fills leaves every delivery waiting for the whole run rather than being failed. A process whose plugins contribute no handlers still starts and provides the long-lived task the runtime needs, consuming nothing, which is what a standby role runs as. The `ingest` workload in [`examples/workloads`](../examples/workloads) carries one such handler beside its timer: enabling the queue integration there gives an ingest worker to the role that hosts ingest, and leaves a batch enqueued under a role that does not sitting at the head of its queue.

Shutdown splits in two. Drain stops every worker fetching new tasks and waits, within `xbc.drain_timeout`, for the handlers already running to return, while Enqueue and the owned Redis connection stay usable; an expired drain means stop waiting, never abort, so handlers it leaves behind keep running with their contexts untouched. Stop then rejects further Enqueue calls and shuts the workers down through the asynq library, which gives whatever outlived the drain up to `plugins.asynq.shutdown_timeout` (default `8s`) to finish before requeueing the rest, and closes Redis only once the workers stopped. Tasks may be redelivered, so handlers should be idempotent and payloads should not contain unprotected secrets. If committing business data and enqueueing work must be atomic, write an outbox event first and let its publisher enqueue the task. Do not rely on a post-commit dual write.

## Background tasks that finish before shutdown

Three mechanisms submit background work, and they answer different questions. `async.Spawn` is for a best-effort task fired from a request or any other call site that must not wait on it -- a cache warm, a notification, a metrics flush -- where losing the task on a crash is an acceptable cost. `plugin.Context.Go` (and `GoCritical`) is for a plugin-owned, long-lived loop submitted once from that plugin's own `Start` hook, such as the sweeper in [Background-only service](#background-only-service); submission is accepted only while `Start` is executing, so it is not a per-request API. `outbox` or `asynq` are for work that must survive a crash: a `Spawn`ed task is lost outright on SIGKILL or an OOM kill, drain or no drain, while outbox commits the pending event in the same database transaction as the business write, and asynq's queue is Redis rather than process memory.

```go
app, err := xbc.New(xbc.WithBundles(
	prelude.Bundle(),
	ginengine.Bundle(),
	async.Bundle(),
))
```

```go
func (p *Plugin) createGreeting(ctx context.Context, c *web.Ctx) error {
	greeting := p.build(c)
	if err := async.Spawn(ctx, "notify-greeting", func(taskCtx context.Context) {
		p.notify(taskCtx, greeting)
	}); err != nil {
		if errors.Is(err, async.ErrSaturated) || errors.Is(err, async.ErrShuttingDown) {
			p.notify(ctx, greeting) // fall back inline rather than losing the work
		} else {
			return err
		}
	}
	return c.JSON(http.StatusOK, greeting)
}
```

`ctx` here only bounds how long `Spawn` waits for capacity (`plugins.async.submit_timeout`, `0s` by default rejects immediately); the task itself keeps running after `Spawn` returns and stops only when the Pool stops. A plugin that would rather depend on the capability explicitly than reach for the process-wide `async.Spawn` declares the same typed input every other consumer of a Definition's primary value does:

```go
var spawnerInput = plugin.RefTo[async.Spawner](async.Key)

var definition = plugin.Define(
	"order-service",
	func(ctx plugin.BuildContext) (*OrderService, error) {
		return &OrderService{spawner: spawnerInput.Get(ctx).Value}, nil
	},
	plugin.Options[*OrderService]{
		Inputs: plugin.Inputs(spawnerInput),
	},
)
```

Selecting `async.Bundle()` is enough; every key below is already at its default:

```yaml
plugins:
  async:
    executor: goroutine          # or "ants"
    max_concurrency: 256         # tasks running at once; 0 = unlimited (ants requires > 0)
    queue_capacity: 1024         # tasks waiting once max_concurrency is reached; 0 = no queue
    submit_timeout: 0s           # how long Spawn waits for capacity; 0s = reject immediately
    shutdown:
      await_termination: true          # Drain waits for running and queued tasks
      await_termination_period: 0s     # extra cap on that wait; 0s = bounded only by xbc.drain_timeout
```

Shutdown walks through this Pool the same way it walks through `asynq` above. SIGTERM runs `PreStop` on every started plugin, then ingress -- the Web server and anything else depending on a `TrafficOpener` -- stops first and finishes requests already in flight. Only then does the drain phase run: the Pool stops admitting (`Spawn` now returns `ErrShuttingDown`) and, since `await_termination` defaults to `true`, waits for running and queued tasks to finish within `min(xbc.drain_timeout, plugins.async.shutdown.await_termination_period)` -- `0s` for the period means the wait is bounded only by `xbc.drain_timeout`. Databases, Redis clients, and every other resource a still-running task depends on stay open through this phase; nothing is closed yet. `Stop` then cancels whatever outlived the drain, discards any still-queued tasks (logging their names), waits for the goroutines it just cancelled to actually return, and only after that releases the executor and closes what Drain left open.

The drain budget comes out of the same arithmetic as [The supervisor's stop grace period](#the-supervisors-stop-grace-period): `xbc.drain_timeout` defaults to 60% of the effective `xbc.shutdown_timeout` (`25s` → `15s`), and it runs *inside* that budget rather than adding to it, so the total a supervisor must allow is still `xbc.pre_stop_timeout + xbc.shutdown_timeout` (`2s + 25s = 27s` at the defaults). Keep the deployment's `terminationGracePeriodSeconds` (or `TimeoutStopSec` / `stop_grace_period`) above that sum, exactly as that section describes, rather than sizing it from `drain_timeout` alone.

`executor: ants` reuses a fixed pool of goroutines sized to `max_concurrency` instead of starting one per task; both executors honor identical `Spawn`/Drain/Stop semantics, so switching this key changes only how an admitted task runs. It is not a default-safe upgrade: measured on the async package's own `BenchmarkSpawn`/`BenchmarkSpawnQueued` (Apple M3), `ExecutorGoroutine` was consistently as fast as or faster than `ExecutorAnts` for a tiny task, both unsaturated (~1.2us/op vs ~1.3us/op) and queued (~1.08us/op for both, no measurable difference). Worker reuse did not pay for its own overhead on a task this cheap; reach for `ants` only when profiling an application's actual workload shows goroutine creation and stack-allocation GC pressure are the measured bottleneck, and benchmark that workload rather than trusting this one.

## Distributed Cron

Jobs are registered through the Go API; configuration controls only the scheduler and lease behavior. A single replica needs no Redis by default. Multiple replicas must enable distributed mode and select a shared Redis instance:

```yaml
plugins:
  redis:
    coordination:
      addr: "redis.internal:6379"
      db: 3

  cron:
    timezone: "Asia/Shanghai"
    seconds: false
    concurrency: skip
    distributed:
      enabled: true
      key_prefix: "orders:cron"
      ttl: 30s
      renew_interval: 10s
      redis_instance: coordination
```

Distributed mode uses a `SET NX` lease with TTL and Lua scripts for atomic renewal and release. Only the lease owner runs a given job among healthy replicas, and TTL expiry enables takeover after a node is lost.

The owner token names the replica that won, so a lock answers *which* process is running a job and not only that one is:

```
$ redis-cli GET orders:cron:nightly-report
"host-7-1758091200-9f3c1a2b/4f2ab9c1d0e3f5a7b9c1d3e5f7a9b1c3"
```

Everything before the last `/` is `xbc.instance_id` -- the same identity `doctor` prints as a slot's `holder`, so a job lock and a workload slot read as one process rather than two. The random half identifies the acquisition rather than the process, and it is what renewal and release compare: an explicitly set `XBC_INSTANCE_ID` survives a restart, so a token that were only the name would let a dead replica's lease renew the lock its successor now holds.

This mechanism provides at-most-one-active-owner coordination, not exactly-once business execution. Jobs must remain idempotent and record retryable outcomes.

## Background-only service

A background service is not a special mode. It is the same composition root with no transport selected, so nothing binds a port and nothing serves probes:

```go
func main() {
	xbc.Run(xbc.WithBundles(
		health.Bundle(),
		sweeper.Bundle(),
		healthlog.Bundle(),
	))
}
```

What keeps such a process running is a critical managed task, and a plugin may submit one only from its `Start` hook:

```go
func (p *Plugin) Start(ctx *plugin.Context) error {
	gate := ctx.TrafficGate()
	if !ctx.GoCritical(func(taskCtx context.Context) { p.run(taskCtx, gate) }) {
		return errors.New("sweeper: runtime rejected the sweep task")
	}
	return nil
}
```

`GoCritical` rather than `Go` is the load-bearing choice. Only a critical task counts as a long-lived capability, so an application whose plugins neither open traffic nor submit one is refused at startup instead of idling until a signal with nothing running inside it. Keep `Context.Go` for supporting loops that may legitimately end on their own; an unprompted return from a critical task requests shutdown and exits non-zero.

Wait on `ctx.TrafficGate()` before doing any work, exactly as a serving transport does. The gate closes only after every plugin's traffic preparation has succeeded, so background work never observes a half-built application. Return when the task context is canceled: that return is prompted, and it is how the plugin cooperates with reverse-order shutdown.

Health does not require HTTP. `plugins.health` is the protocol-neutral capability and its endpoint adapter is a separate plugin, so a background service configures the section with no adapter selected and reads the aggregate programmatically through `Plugin.Check`:

```yaml
plugins:
  health:
    timeout: 2s
```

Adding a `web:` section to a service that selected no transport fails startup by name, because every section must be owned by a selected plugin or by the framework.

`examples/worker` is a runnable version of all of the above, including a readiness report that stays down until the first unit of work lands:

```sh
go run ./examples/worker --config examples/worker/application.yml
go run ./examples/worker doctor --config examples/worker/application.yml
go run ./examples/worker validate --config examples/worker/application.yml
```

`validate` is worth more on this shape than it looks: a background service has no route table to check, so what it exercises is the plugins' real `Init` and `Stop` -- the connection, the client, the credential -- and it exits afterwards, turning a misconfiguration into an exit code instead of a service that starts and immediately dies.

## Hosting a subset of workloads

A process does not have to carry every plugin the application declares. A workload is a named group of Definitions a process carries as a unit or not at all. It is declared once at the composition root and selected by placement:

```go
// Package sast is the application's static analysis workload.
package sast

// Key is this workload's stable placement and configuration identity.
const Key plugin.WorkloadKey = "sast"

var bundle = plugin.WorkloadOf(
	Key,
	plugin.BundleOf(dispatcherDefinition, workerDefinition, apiDefinition),
	// A process carrying sast carries nothing else.
	plugin.WithExclusiveProcess(),
	// At most three processes may carry it.
	plugin.WithReplicas(3),
)

// Bundle returns this workload's side-effect-free composition Bundle.
func Bundle() plugin.Bundle { return bundle }
```

The composition root then selects it exactly like any other Bundle:

```go
app, err := xbc.New(xbc.WithBundles(
	prelude.Bundle(),
	ginengine.Bundle(),
	sast.Bundle(),
	coderanger.Bundle(),
	webscan.Bundle(),
))
```

`sast`, `coderanger`, and `webscan` are illustrative names. [`examples/workloads`](../examples/workloads) is the runnable version of the same two steps -- one binary, two workloads (one exclusive, one co-resident), and one unowned plugin every role carries -- where the same config file yields a different role per process:

```sh
# Co-resident role: serves ingest and heartbeat; the transcode route is 404.
go run ./examples/workloads --config examples/workloads/application.yml

# Exclusive role: the same binary and the same config file, a different role.
XBC_WORKLOADS_TRANSCODE_ENABLED=true XBC_WORKLOADS_INGEST_ENABLED=false \
  go run ./examples/workloads --config examples/workloads/application.yml

# The decision without constructing anything.
go run ./examples/workloads doctor --config examples/workloads/application.yml
```

`WithExclusiveProcess` is for a workload with process-wide side effects -- tuning a global GC target, setting a process-wide memory limit, sizing a pool every other plugin shares -- which nothing sharing its process can be protected from. The reason is not that the workload is heavy. A heavy workload expresses that as a replica count and a task budget instead, neither of which forces it into a process of its own.

`replicas` and `exclusive` are deliberately not configurable. They describe the cluster rather than one process, and making them per-process would let a single host reinterpret how many replicas may run, or whether it must run alone, silently invalidating the decision every other process derived from the same declaration.

What one process may decide about itself lives under the `workloads` root, one section per declared workload:

```yaml
workloads:
  sast:
    enabled: true
    max_goroutines: 64
  coderanger:
    enabled: false
    max_goroutines: 256
```

- `enabled` defaults to `true` and is a hard veto. A workload disabled here is refused by every placement source, including a lease-backed one; this is how a deployment excludes a process from a role outright.
- `max_goroutines` defaults to `0` (unbounded) and bounds how many units of work this process may run on behalf of that workload. It is one number, not two budgets: a managed task submitted with `Context.Go` and a unit a shared integration takes with `Context.Admission` both count against it. A task submitted past the budget is rejected and counted instead of started, while an admission waits for a unit to come free within its own context; either way other workloads are unaffected. Plugins with no workload -- transports, infrastructure, observability -- are not bounded by it. Size it above the process-lifetime tasks the workload's own plugins submit from `Start`: such a task holds its unit until shutdown, so a limit the resident work alone reaches leaves every admission for the workload -- a queue delivery, a pooled task -- waiting for as long as they run. The runtime warns about exactly that, naming the workload, when the last `Start` hook returns with no unit left to admit.
- Environment overrides use the full path: `XBC_WORKLOADS_SAST_ENABLED`, `XBC_WORKLOADS_CODERANGER_MAX_GOROUTINES`.

A `workloads.<key>` section that names no declared workload fails startup as an unowned key, exactly like a misspelled plugin section.

Placement decides the hosted set once, before the plugin graph is built. The default is static placement, which contacts nothing and hosts exactly what the configuration enables:

```go
app, err := xbc.New(
	xbc.WithPlacement(placement), // omitted -> xbc.StaticPlacement()
	xbc.WithBundles(prelude.Bundle(), ginengine.Bundle(), sast.Bundle()),
)
```

A workload this process does not host is not disabled: its Definitions never enter the plan at all. No instance is constructed, no connection pool is opened, no queue worker consumes its queues, and no timer is created. Its routes do not exist in this process either, so a request for one is answered `404` rather than forwarded.

Shared integrations follow the same rule. A queue runtime keeps one Definition and one enqueue client, and runs one worker per group of handlers that shares a workload, so what a process consumes stays tied to what it hosts. The queues of an unhosted workload remain valid names to enqueue to -- an API process legitimately hands `sast` work to a `sast` worker it does not run -- but nothing in this process polls them:

```yaml
plugins:
  asynq:
    queues: {default: 1}          # the worker for contributors that belong to no workload
    concurrency: 10
    workloads:
      sast:                       # required once a hosted workload contributes handlers
        queues: {sast: 5}         # must not overlap another group's queues
        concurrency: 20           # 0 takes the top-level concurrency
```

The process-level runtime knobs belong to the framework rather than to any plugin, so a plugin cannot change them for its own benefit:

```yaml
xbc:
  runtime:
    max_procs: auto      # derive from the container's CPU quota
    memory_limit: "75%"  # bytes, or a percentage of the container memory limit
    gc_percent: 0        # 0 leaves the Go default of 100 alone

web:
  max_in_flight: 0       # 0 applies the fixed default (1024 concurrent requests)
```

`max_procs: auto` reads the container's cgroup CPU quota, so a container limited to two cores runs with `GOMAXPROCS=2` rather than the host's core count. `memory_limit` makes the runtime collect harder as the container approaches its limit instead of being killed. A build in which a plugin called `debug.SetGCPercent`, `debug.SetMemoryLimit`, or `runtime.GOMAXPROCS` outside the framework fails the repository's architecture guard, which is what makes the values `doctor` reports worth trusting.

The two defaults are deliberately asymmetric, and this block is a recommendation rather than a description of them. `max_procs` defaults to `auto` because sizing the scheduler from the host's cores inside a quota is a factual error with no trade-off to weigh. `memory_limit` defaults to `0`, no limit, because a soft limit trades CPU for heap and only the deployment knows how much of each it wants -- and because a percentage needs a derivable container limit, so a framework default of `"75%"` would fail startup on bare metal for a deployment that configured nothing. `"75%"` is the value to write for a containerized process; outside a container write a byte count or leave it at `0`.

`workloads`, `xbc.runtime`, `xbc.pre_stop_timeout`, and `web.max_in_flight` are introduced together with workload placement. A deployment that declares no workload keeps exactly its previous behaviour.

`web.max_in_flight` reports itself as a pair of state transitions rather than per refused request. The refusal that finds the process newly saturated logs `web: in-flight limit reached, refusing requests until in-flight work drains` at warn with `limit`, `rejections`, `rejections_total`, and `retry_after_seconds`; the release that leaves nothing in flight logs `web: in-flight limit cleared, admitting requests again` at info with `limit`, `rejections`, and `rejections_total`. `rejections` counts only what was refused since the gate's previous line -- the part nobody has seen yet -- while `rejections_total` is the count since boot, so consecutive lines can be compared without double counting. One saturation episode therefore produces at most those two lines however long it lasts, and it ends only once in-flight work drains to zero rather than merely below the ceiling: the number of log lines is not a proxy for the number of refusals, and a process parked at its ceiling reports one episode where an operator might have counted several. Read `rejections_total` or `Server.InFlightStats()` for the quantity, and treat the warn line as the episode's start rather than as a per-request signal. Leaving the key unset applies a fixed default of 1024 concurrent requests, independent of `max_procs`: the ceiling counts requests rather than runnable goroutines, and a request waiting on a database or an upstream holds no CPU, so size the key from the deployment's memory and dependency budget when the default is wrong for it.

## Slots, standbys, and how many processes to start

This section assumes a placement source that hands out slots by replica count — the lease source under `extensions/coordination/placement`. The default `StaticPlacement` does not: it ignores `replicas` entirely and hosts every workload the configuration enables in every process that enables it. Slots, standbys, takeover, and the arithmetic below therefore presuppose a source that reads `replicas`; under `StaticPlacement`, `replicas` is a declaration nothing enforces.

A workload's `replicas` is its number of slots: `<prefix>:workload:<key>:<index>` for `index` in `[0, replicas)`. Each process competes for exactly one slot per workload, so `replicas` is the true maximum concurrency of that workload across the cluster.

The arithmetic for the minimum process count follows from two rules. A process holding an exclusive workload holds nothing else, so exclusive workloads never share a process. Non-exclusive workloads do share, so the busiest one sets the count:

```
minimum processes = sum of replicas of the exclusive workloads
                  + largest replicas among the non-exclusive workloads
```

Take this declaration:

| workload | exclusive | replicas |
| --- | --- | --- |
| `sast` | yes | 3 |
| `coderanger` | no | 6 |
| `webscan` | no | 2 |

- exclusive sum: 3
- largest non-exclusive: 6
- minimum: **9**

Starting exactly nine processes fills every slot and leaves no takeover capacity at all. Losing a process then means a role stays unfilled until someone restarts it by hand.

Every process beyond that minimum is a standby, and the surplus cannot be absorbed instead: the busiest non-exclusive workload has already taken its `replicas` generalists, and every lighter one is full by the time those have finished claiming. A standby starts normally, hosts only the plugins that belong to no workload -- the transport, health, observability -- reports readiness, and retries its claim on a timer. Starting `N + k` buys `k` concurrent takeovers, and the pool drains as it is used: a standby that wins a slot releases it and requests shutdown, and the process the supervisor brings back is a working replica rather than a standby again. Start eleven processes in the example above and two of them sit idle, absorbing two failures without a role going unfilled.

To work out a deployment, take every declared workload with its `exclusive` flag and `replicas`, sum the exclusive ones, take the largest remaining one, add them, and then add the number of simultaneous failures you want to survive. One is the minimum useful answer: with zero spares there is no automatic takeover, only a role that stays empty until an operator intervenes.

## Takeover latency and soft placement

Roles claimed by lease are what make takeover possible. The composition root builds the locker itself, because placement is decided before the plugin graph exists and therefore cannot be provided by a plugin:

```go
// github.com/redis/go-redis/v9, github.com/xbcio/xbc/extensions/storage/redis
// and github.com/xbcio/xbc/extensions/coordination/placement
client := goredis.NewClient(&goredis.Options{Addr: "redis.internal:6379", DB: 1})
locker, err := redis.NewLocker(client)
if err != nil {
	return err
}
hosting, err := placement.New(locker, placement.WithTTL(30*time.Second))
if err != nil {
	return err
}

app, err := xbc.New(
	xbc.WithPlacement(hosting),
	xbc.WithBundles(hosting.Bundle(), prelude.Bundle(), sast.Bundle()),
)
```

The `*placement.Placement` is both the decision and the plugin that keeps it alive, so it is passed to `WithPlacement` and its `Bundle()` is selected alongside the workloads. Selecting one without the other is not refused -- the two are one value at runtime, but nothing in core can check that the composition root passed it to both call sites -- and the consequence is bounded rather than silent: the runtime gives the claim back when the run exits, so a slot whose releasing hooks were left out of the graph returns with the process instead of waiting out its lease TTL. There is no placement constructor on the `xbc` facade itself: core's dependency closure excludes everything beneath `extensions/`, so the lease contract cannot be named there.

That wiring is exercised end to end by the placement module's own tests, which run a real application through `xbc.New` against a Redis-backed store -- an in-process one, so nothing has to be started first:

```sh
go test ./extensions/coordination/placement/ -run TestALeaseHolder -v
```

That is one extra connection to the store, and it is the price of deciding the hosted set before the graph is built rather than during construction. Every command that builds a plan pays it, `doctor` included, and a command that returns without constructing a plugin gives the slots it won to answer straight back, so a diagnostic never holds the capacity its report was read from.

Takeover is a restart, not a live handover. When a holder is lost, its slot does not become available until its lease expires, then a standby has to notice, and the process the supervisor starts has to come back up before the role is really served again:

```
takeover latency = lease TTL + standby retry interval + process restart time
```

The lease TTL dominates, and it is three times the renew interval by default. With a 10s renew interval, a 30s TTL, a standby retrying every 5s, and a 2s restart, the worst case is `30 + 5 + 2 = 37s`.

That is acceptable here because these workloads are queue-backed. A task in flight when the holder died is redelivered by the queue, and mutual exclusion between the failed holder and its successor is the queue's, the distributed lock's, and the database's job rather than the lease's.

Renewal failing is treated differently from a cold start, and the asymmetry is deliberate:
The retry interval is jittered by up to half its length in either direction, so the arithmetic's worst case is `TTL + 1.5 x interval + restart` -- `30 + 7.5 + 2` in the example above -- and its average is the one in the formula. The jitter is there because a fleet of standbys started together keeps its phase for the life of the run: on a fixed period they find a freed slot in the same round, and they hand it back and restart together.

One more term belongs to the observation rather than to the slot: the standby that wins **hands the slot back and restarts**, so the role is not served again until that process has finished its own graceful stop and come back up. Add the winner's own `pre_stop_timeout + shutdown_timeout` to the gap you actually observe, and expect the slot to sit free (or be taken by another standby, which restarts in turn) for that long. Nothing is lost by that -- the workloads are queue-backed -- but a deployment sized on the formula alone will find recovery slower than its arithmetic.

A holder that dies **without releasing** -- an OOM kill, which is this class of workload's ordinary failure -- costs one more restart than a clean stop. Its lease outlives it, and the token that claim is stored under (`instance_id` plus a per-acquisition random suffix) died with the process, so the replacement starts as a standby: it waits out the TTL, wins the slot, hands it back, and restarts a second time to take it up. Budget one extra TTL plus restart in the recovery of a crashed holder. Reclaiming the old claim by name is not the fix it looks like: the random half is what makes a token name one *acquisition*, and a process that could resume a claim by name could resume one a standby had already been handed.

Changing `replicas`, or flipping `exclusive`, is a declaration change, and it takes effect one binary at a time. A running process keeps renewing the indices its own declaration names and never searches outside them, so during an overlap the fleet briefly runs both declarations: scaling down from 4 to 2, the old binary's holders of the first two slots are correct under either declaration, while its holders of the last two keep serving until each process is replaced. Scaling up is the mirror image -- the new binary searches the new indices, and old standbys keep retrying the old range -- so the added capacity appears only once the rollout reaches the processes that declare it.

The ordering that keeps this harmless is to roll out first and scale down last: add replicas before the rollout, remove them after it has finished, so no moment of the deployment declares less capacity than it is running. Give an exclusivity flip its own rollout rather than folding it into a count change, so that each intermediate fleet differs from its predecessor by one declaration, which is the difference an operator can read off `doctor` from any process in it.


- **A running holder keeps its role.** A failed renewal is logged, counted, and otherwise ignored: the process does not release its slot and does not exit. The worst outcome is a workload briefly running more replicas than declared. That is a resource problem, not a correctness one, and the alternative -- dropping the role on a lease-store hiccup -- would reshuffle roles across the whole cluster.
- **A cold start that cannot reach the lease store fails.** A process that cannot claim anything would have to guess, and the only guess available is "carry everything". That makes the process shape non-deterministic: `doctor` output, startup validation, snapshot diffing, and the exclusivity check all derive from the hosted set, and the capacity decision becomes fail-open. A holder has something to protect; a starter has nothing to guess with.

Alert on the renewal-failure counter, where a sustained increase means the store is degraded and takeover is impaired, and on the lease age, where a holder's lease age growing without a matching renewal success means renewal is stalling. Those two series move on the first unconfirmed round while readiness waits out a wider grace period (below), so page on the metrics when the goal is to see degradation before any traffic moves. Aggregate the held gauge by workload across processes to see how many replicas each workload actually has; a value above `replicas` is the soft-placement case above, not a bug. A process restart on its own is expected rather than alarming -- that is what takeover looks like.

None of these series are published by the placement module itself. Core owns no metrics registry -- the Prometheus registry is a Web extension, and the placement module deliberately does not depend on a transport -- so it exposes a `placement.Stats()` snapshot instead, and the series below are what a bridge over that snapshot should publish:

| Series | Source field |
| --- | --- |
| `xbc_workload_held{workload}` | one series per `Stats().Held` entry |
| `xbc_workload_lease_age_seconds{workload}` | `Stats().Held[].Age` |
| `xbc_workload_lease_renew_failures_total` | `Stats().RenewFailures` |

`Stats().Held[].Degraded` is the per-slot form of the same signal: it reports "still serving, but the claim is not being confirmed", which is the difference between a degraded store and a stopped process. A readiness probe already exported for the health aggregator (`placement-health`) carries the same verdict without any metrics stack, and it reads this process's own renewal state only -- never the store, and never other members.

| `xbc_workload_declared_replicas{workload,exclusive}` | `Stats().Declared[].Replicas` / `.Exclusive` |
| `xbc_placement_standby` | `Stats().Standby`, as 1 or 0 |

The declared-replicas gauge is what makes the two cluster-level questions answerable without copying Go constants into alert rules. Every process reports the replica count and exclusivity it was assembled with, so a rule compares the held gauge against a number the deployment itself stated:

```promql
# A workload held by fewer processes than its declaration allows. Pair it with
# `for:` a little longer than a takeover takes, or it fires during every one.
sum by (workload) (xbc_workload_held)
  < max by (workload) (xbc_workload_declared_replicas)

# A declared workload held by nobody at all. The right-hand side appears even
# when the held series is gone, which is the case a plain comparison misses:
# an aggregation with no input series simply returns nothing.
max by (workload) (xbc_workload_declared_replicas)
  unless sum by (workload) (xbc_workload_held)
```

`xbc_placement_standby` is the other side of the same arithmetic: it is how many takeovers the deployment currently has in reserve, and it reaching zero is the condition the sizing section above warns about rather than an alarm on its own. A workload this process vetoes by configuration contributes no declared row from that process, so a rule built on these series sees the declaration of the fleet that admits the workload rather than a count nobody would honour.
Two different identities appear when asking "who holds this slot", and they answer different questions. `xbc.instance_id` names the process: derived from the hostname, the boot second and a random suffix when left empty, and settable per process as `XBC_INSTANCE_ID` -- see [`xbc.instance_id`](quickstart.md#configuration) for the derivation and the whitespace rule. The slot's *owner token* is what the lease backend stores under the slot key and what it compares before renewing or releasing; it identifies one acquisition rather than a process.

The identity is what gets reported, because it is the one an operator can act on. `doctor`'s `holder` line and `Stats().Instance` both carry it, so a report names a process you can go and look at rather than a token. The token remains available per slot as `Stats().Held[].Owner`.
The probe carries that verdict with a grace period, and the two readings are meant to differ. A claim reports `Degraded` from the first unconfirmed round onward, while `placement-health` turns down only once a claim has gone unconfirmed for longer than one TTL. One failed round says the store did not answer; it does not say the role is gone, because the key the previous round wrote is still inside its TTL. Reporting down there would withdraw every holder at once -- they share one store -- and move that traffic onto standbys that do not host these workloads, which is exactly the failure the lease store was never asked to be highly available for. Past one TTL the strongest local fact changes: the key cannot still exist, so a claim held in local memory may already be someone else's. That is the condition worth withdrawing for, and it is the one readiness reports. In between, the metrics and the log lines carry the episode.


The store is readable without either of them. The token the backend mints is `<instance_id>/<random>`, so reading a slot key answers who holds it:

```
$ redis-cli GET xbc:workload:ingest:0
"host-7-1758091200-9f3c1a2b/4f2ab9c1d0e3f5a7b9c1d3e5f7a9b1c3"
```

Everything before the last `/` is the process. The random half is not decoration: an explicitly set `XBC_INSTANCE_ID` survives a restart, so a token that were only the name would let a dead process's lease renew the lock its successor now holds. Take the name, ignore the rest.

One log line binds the exact token to the exact identity it was minted for, at acquisition:

```
INFO  placement: slot won  workload=ingest slot=0 key=xbc:workload:ingest:0 instance=host-7-1758091200-9f3c1a2b owner=host-7-1758091200-9f3c1a2b/4f2ab9c1…
```

A process that supplies no identity -- a direct `Resolve` caller outside a run -- still gets a unique token, and its slot key then reads as a bare random value. That is the only case where the store cannot name a holder.

Setting `XBC_INSTANCE_ID` per process is still worth doing: a derived identity is unique but says nothing about which deployment slot the process is, and a supervisor that already knows that can name it.

The lease store itself needs no high availability. It carries resource placement, not correctness: mutual exclusion is already guaranteed downstream by the queue, a distributed lock, and a database compare-and-swap. Two processes briefly both holding a role is therefore a resource question, and the cost of preventing it is not worth paying. A single-node store is the intended deployment, and losing it does not corrupt anything -- holders keep running, and new processes refuse to start rather than guess. It needs no backup and no replica, and it can share the Redis the queues already use under its own key prefix.

## The supervisor's stop grace period

Shutdown has two outer phases plus one inner one. `xbc.pre_stop_timeout` defaults to `2s` and runs `PreStop` on every started plugin before any `Stop` begins; that is where a placement plugin gives its slot back. `xbc.shutdown_timeout` defaults to `25s` and then covers cancellation, ingress shutdown, draining, HTTP drain, and reverse-order `Stop`. `xbc.drain_timeout` is not a third outer budget: it runs inside `shutdown_timeout`, between the ingress stop and the remaining `Stop` calls, so it does not add to the sum a supervisor has to allow for. Left unset it defaults to 60% of the effective `shutdown_timeout` (`15s` at the `25s` default):

```yaml
xbc:
  pre_stop_timeout: 2s
  shutdown_timeout: 25s
  drain_timeout: 15s
```

The total budget is still just the two outer phases' sum, `2s + 25s = 27s`, and the supervisor must allow at least that much before it kills the process. A supervisor that kills earlier interrupts the release, and the slot then waits for its TTL instead of being freed immediately.

The 2s/25s split is deliberate: their sum stays below Kubernetes' default `terminationGracePeriodSeconds` of `30s`, so a pod using XBC's own defaults is not killed mid-shutdown by a cluster that never set the grace period explicitly. A deployment that raises either budget should raise `terminationGracePeriodSeconds` to match.

systemd's `TimeoutStopSec` and Docker's `stop_grace_period` are the two settings that matter outside Kubernetes. Docker's default of `10s` is shorter than the default budget and is the one that bites in practice:

```ini
# systemd
[Service]
ExecStart=/usr/local/bin/orders --config /etc/orders/application.yml
Restart=always
RestartSec=1s
KillSignal=SIGTERM
# 2s pre_stop_timeout + 25s shutdown_timeout = 27s; 45s leaves scheduling headroom.
TimeoutStopSec=45s
```

```yaml
# docker compose
services:
  orders:
    image: registry.internal/orders:1.2.3
    restart: always
    # Default is 10s, which is shorter than the 27s stop budget.
    stop_grace_period: 45s
```

Setting `pre_stop_timeout: 0s` skips the phase and makes the total `shutdown_timeout` alone. Setting `drain_timeout: 0s` skips the drain phase the same way, without changing the total at all, because that phase was never counted beside `shutdown_timeout` in the first place.

The restart policy is not optional. Takeover works by a standby requesting shutdown on purpose once it has won a slot, and the supervisor is what brings that process back as the real holder. Without `Restart=always` or `restart: always`, the first takeover turns a standby into a stopped container.

## Running behind a TLS-terminating proxy

The Web runtime does not terminate TLS. It loads no certificate, reloads none, verifies no client certificate, and has no keystore to configure; the process speaks plain HTTP and a reverse proxy, ingress, or load balancer in front of it terminates TLS. That is a deployment shape rather than a gap to work around, and two keys are the whole of the process's side of it.

**Forwarded headers are opt-in.** `web.trusted_proxies` is empty by default, which makes the engine ignore `X-Forwarded-For` and `X-Real-IP` completely: the client address a handler or an access-log line reports is then whichever peer opened the connection, which is the proxy. List the proxy's exact addresses or CIDRs to make them the sources whose forwarded headers are believed:

```yaml
web:
  addr: ":8080"
  trusted_proxies: ["10.0.30.11", "10.0.30.0/24"]
```

Never write `0.0.0.0/0` or `::/0`. These headers are client-controlled text, so a wildcard entry lets any caller claim any client address — in an access log, in a rate limit, and in anything else keyed by client IP. The proxy must also strip client-supplied `X-Forwarded-*` headers and add its own; a proxy that forwards what it received turns every address in the log into a claim by the client.

**HSTS needs a second key when the proxy terminates TLS.** `securityheaders` emits HSTS only for a TLS request, and a request arriving from a TLS-terminating proxy is already plain HTTP by the time this process sees it, so the header never appears at all. What the deployment usually wants is for the proxy to add it, since the proxy is the layer that knows whether the client connection was secure. A deployment that would rather this process emit HSTS switches the decision to the `X-Forwarded-Proto` header:

```yaml
plugins:
  securityheaders:
    hsts_trust_forwarded_proto: true
```

That key does not consult `web.trusted_proxies` — it reads the header as it arrived — so it is safe exactly when the proxy is what sets that header and strips any client-supplied value first. Enabled in front of a proxy that forwards the client's own header, a client can claim HTTPS it never used and receive an HSTS policy for a service it reached over plain HTTP.

The listener stays what it was: bind `web.addr` to the interface the proxy reaches, keep the certificate and its private key in the proxy's own secret store rather than in this process's configuration, and keep credentials in request bodies and headers rather than in URLs, since a proxy logs the URL it forwards.

## Building and running the production image

`examples/production` is the composition a Web service is meant to grow from: a public metadata route, authenticated and authorized business routes, a transactional outbox beside a real database, a scrape endpoint, and the framework baseline. Its `Dockerfile` is a starting point for any XBC image, and the three parts it is made of are the three any XBC application image needs.

Build it from the repository root, because the build needs the whole workspace:

```bash
docker build -f examples/production/Dockerfile -t xbc-production .
```

`examples/go.mod` resolves every XBC module through this repository's `go.work` rather than through version-tagged releases, so a build context of `examples/production` alone cannot compile. `.dockerignore` keeps the repository's history and local working directories out of the context while leaving the manifests and every module in it.

The build stage runs with `CGO_ENABLED=1`, which is required rather than incidental: the example's database is SQLite, whose driver is cgo-backed, and a cgo-disabled build produces a binary that compiles and then cannot open a database. The runtime stage is distroless `base-debian12`, which carries the glibc that binary links against and the CA bundle an outbound TLS connection needs, with no shell and no package manager to install into. It runs as `nonroot` in `/var/lib/xbc`, the one directory it owns — a non-root process cannot create its data directory, and SQLite cannot create its database file in a directory it cannot write.

The image is startable from environment variables alone; no configuration file is baked in. What it sets is the part that describes the process rather than the deployment:

| variable | value | why it is the image's |
| --- | --- | --- |
| `XBC_WEB_BASE_PATH` | `/api/v1` | the prefix every route is served below; the framework's own default is `/` |
| `XBC_LOG_CONSOLE_FORMAT` | `json` | one JSON object per line is what a container log collector reads; the default is human-oriented console output |
| `XBC_PLUGINS_GORM_DEFAULT_DRIVER` | `sqlite` | the example ships no external database |
| `XBC_PLUGINS_GORM_DEFAULT_DSN` | `file:/var/lib/xbc/production.db?...` | a path that exists and is writable in this filesystem |
| `XBC_PLUGINS_OUTBOX_MIGRATE` | `true` | the outbox's table belongs to the outbox, not to the deployer |
| `XBC_PLUGINS_METRICS_HTTP_ENABLED` | `true` | the scrape endpoint is a process property |
| `XBC_AUTO_MIGRATE` | `true` | one container comes up ready to serve |
| `XBC_PLUGINS_VERSION_VERSION` | the `VERSION` build arg | the same binary reports the image's version |

The one setting an image cannot supply is the credential, and this image supplies none:

```bash
docker run --rm -p 8080:8080 \
  -e XBC_PLUGINS_JWT_SECRET="$(openssl rand -hex 32)" \
  xbc-production
```

`plugins.jwt.secret` has no default anywhere, so a container started without it fails at startup naming the key instead of running with a guessable one. With only that variable set, the process starts, migrates, and serves: `/api/v1/healthz`, `/api/v1/readyz`, and `/api/v1/version` are public, while `/api/v1/orders` and `/api/v1/metrics` answer `401` without a credential. Two capabilities are absent by construction rather than by oversight. The API-key scheme cannot be configured from the environment at all: the environment layer can address any declared configuration path, but the value it carries has to be a scalar or a list of strings, and a set of credentials is a list of structured entries. A deployment that needs service credentials gives this process a file, or an operator-only mechanism, rather than a variable. Authorization is absent unless the run also configures `plugins.casbin` and `plugins.casbin-http`: the policy *is* a single string and therefore expressible in the environment, newlines and all, and so is the middleware's own section, because selecting the middleware takes nothing but `XBC_PLUGINS_CASBIN_HTTP_MISSING_PERMISSION=deny`.

The image migrates at every boot, which is what makes a single container work and is wrong for several replicas starting at once. A deployment runs the migration separately and starts the service without it:

```bash
docker run --rm xbc-production migrate
docker run --rm -p 8080:8080 -e XBC_AUTO_MIGRATE=false \
  -e XBC_PLUGINS_JWT_SECRET="$SECRET" xbc-production
```

`migrate` is a subcommand of the same binary: it runs every plugin's migration, starts and stops nothing, binds no listener, and exits. The migration run needs no credential — with no `plugins.jwt` key present the authenticator is dormant, and a schema operation is not a serving one — so a deployment job runs it without handing a signing secret to a process that would not use it. A service started with `XBC_AUTO_MIGRATE=false` never touches the schema, and `validate` never does either, so a check can run against a live deployment.

Probing follows the paths above: `/api/v1/healthz` for liveness, `/api/v1/readyz` for readiness, both public and neither metered by `web.max_in_flight`. Give the container a stop grace period longer than `xbc.pre_stop_timeout + xbc.shutdown_timeout` — the supervisor section above works through the arithmetic — or it is killed mid-drain. The image is an example, not a release artifact: it is built from this workspace, and publishing, signing, and scanning images belong to whoever deploys the service.

## Migrating a schema across workloads

Migration runs only for the workloads this process hosts. A rolling restart therefore cannot be relied on to migrate everything, and adding a workload later is not migrated merely by deploying it.

Run migration as a one-off job whose composition hosts every workload, with nothing else running. A workload declared `WithExclusiveProcess` cannot share a process with another workload, so "everything at once" is one run per exclusive workload plus one run hosting all the non-exclusive ones:

```yaml
# migrate-rest.yml -- every non-exclusive workload, for one run. `sast` is
# exclusive, so it is migrated by a run of its own rather than by this one.
workloads:
  sast:
    enabled: false
  coderanger:
    enabled: true
  webscan:
    enabled: true
```

```sh
# One run per exclusive workload, then one for the rest.
orders --migrate --config /etc/orders/migrate-sast.yml
orders --migrate --config /etc/orders/migrate-rest.yml
```

The `migrate` subcommand (`orders migrate --config ...`) and the `--migrate` flag both run the migration stage, but they do not do the same thing: the flag migrates and then boots the application normally, while the subcommand migrates, unwinds, and exits without ever starting or serving anything. Every constructed plugin's `Stop` still runs on that path even though no `Start` did, so a plugin whose `Stop` assumes `Start` ran fails there. Reach for the flag when the job is a one-off invocation of an otherwise ordinary command line, and for the subcommand when the process should do nothing but migrate.

With the default static placement each job hosts exactly the workloads its file enables, and nothing else. An application that selects a lease-backed placement must give this one-off job a way to run without it -- a separate composition root, or a switch `main` reads -- because otherwise "hosts everything" depends on winning every slot in a race, and a migration that wins only some of them silently migrates a subset. The switch is small enough to live in `main`, and it has to be decided before composition rather than during the run:

```go
// The migration job is the same binary started with ORDERS_PLACEMENT=static.
// It composes every workload but never builds the lease source, so the default
// placement hosts exactly what the job's own file enables.
options := []xbc.Option{xbc.WithBundles(prelude.Bundle(), sast.Bundle(), webscan.Bundle())}
if os.Getenv("ORDERS_PLACEMENT") != "static" {
	locker, err := redis.NewLocker(client)
	if err != nil {
		return err
	}
	hosting, err := placement.New(locker, placement.WithTTL(30*time.Second))
	if err != nil {
		return err
	}
	options = append(options, xbc.WithPlacement(hosting), xbc.WithBundles(hosting.Bundle()))
}
app, err := xbc.New(options...)
```

```sh
ORDERS_PLACEMENT=static orders --migrate --config /etc/orders/migrate-rest.yml
```

That switch gives up the property the fleet has: the job hosts exactly what its file enables, regardless of what the rest of the fleet is doing -- which is why it runs alone.

Verify before trusting the run. `doctor` resolves the hosted set without constructing anything, so every declared workload in that run must appear as hosted:

```sh
orders doctor --config /etc/orders/migrate-rest.yml
```

`xbc.auto_migrate` defaults to `false`, so the flag is a deliberate opt-in and an ordinary boot never mutates a schema.

## Attributing CPU to a workload

Every managed task -- anything submitted through `plugin.Context.Go` or `GoCritical` -- runs under a `workload` profiler label naming the workload its plugin belongs to, and every queue delivery does too: the asynq integration runs each task under the same label, restoring the worker goroutine's previous labels afterwards because the queue library reuses its workers. A CPU profile can therefore be read per role:

```sh
# CPU by role, for every labelled workload at once
go tool pprof -tags http://127.0.0.1:8080/debug/pprof/profile?seconds=30

# only one role's stacks
go tool pprof -tagfocus=workload=ingest http://127.0.0.1:8080/debug/pprof/profile?seconds=30
```

`-tags` prints one line per workload with its share of the profile; `-tagfocus` narrows every later view to that workload's samples.

The label exists because a stack cannot answer the question. Frames say which plugin is burning CPU; workload membership is decided at composition, so the same binary attributes the same function to different workloads depending on which slots each process won.

Two limits are worth knowing before reading a profile this way:

- **It covers background work, not request handling.** Labels are inherited by goroutines started under them, and the Web server belongs to no workload -- its accept loop serves every workload's routes. Labelling it would file each request under a name that denies the workload actually being served, so shared plugins are left unlabelled and request CPU is untagged. Queue deliveries are labelled because the same test passes there: a worker serves one workload's handlers, and the worker for contributors that belong to none is left unlabelled like every other shared component.
- **Untagged is not a workload.** Samples with no `workload` tag are the shared infrastructure plus the runtime itself. There is no `unowned` tag to focus on, deliberately: `unowned` is a legal workload key.

Heap profiles carry no labels at all, so memory is not attributable this way. `xbc.runtime.memory_limit` bounds the process rather than a role.

## What workload placement does not do

These boundaries are deliberate, and knowing them prevents several wrong deployments:

- **No runtime re-placement.** Roles are claimed once at startup and held for the life of the process. Changing a process's role means restarting it, because a workload's registration work -- queue handlers, timer callbacks, consumers -- happens once at construction and cannot be undone.
- **No in-process request forwarding and no cluster routing table.** A request for a workload this process does not host is a `404` here. A management tool talks to the process that holds the role rather than to "the service" as a whole.
- **No member enumeration and no service-discovery contract.** Each process reports only what it holds. Aggregating that into "how many replicas of `sast` are running" is the monitoring side's job, which is why the metrics above are per-process.
- **No fencing tokens, split-brain detection, or lease generations.** Those are what hard mutual exclusion needs, and the lease is not that.
- **No per-workload HTTP in-flight budget.** `web.max_in_flight` is process-wide; a request over the limit is answered `503` with `Retry-After` before any handler runs. A per-workload share would require resolving the route before admitting the request, which is exactly the work the gate exists to refuse before. A route that genuinely must be answered from a saturated process declares itself exempt instead, by marking its registration `router.GET(...).Unmetered()`; the gate looks the request's own method and path up in the frozen route table, so an unmetered path has to be a literal one and startup rejects a pattern.
- **No placement-independent verdict on a cross-workload `Collect` edge.** A workload that collects a contract another workload exports is refused when both are carried in one process, and resolves to an empty set when the producer's workload is not carried -- so whether that assembly starts depends on the decision. `QueryOne` and `QueryRef` have no such gap; keep cross-workload collect edges out of the design rather than relying on the error.
- **No per-role process knobs.** `xbc.runtime.*` is installed during bootstrap, before a lease decides which workloads the process hosts, so under lease placement every replica receives the same `gc_percent` and `memory_limit`: they are a property of the process, not of the role it wins. A role that needs its own tuning wants its own deployment -- natural for an exclusive workload -- rather than a knob that follows placement.
- **No per-workload memory accounting.** CPU is attributable through the profiler label above; heap profiles carry no labels, so `xbc.runtime.memory_limit` bounds the process rather than a role.

## Streaming a response past the write timeout

`web.write_timeout` defaults to 30s and covers a whole response, not a single write. That is the right bound for request/response traffic and the wrong one for a response with no known length: server-sent events, a progress feed, a long export. A stream that outlives the budget is severed mid-body, so the client sees a truncated response rather than a status it can interpret.

Raising the key for the whole server is not the fix, because that removes the bound from every ordinary route as well. The route that streams lifts the deadline for its own connection:

```go
router.GET("/events", func(ctx context.Context, c *web.Ctx) error {
	if err := http.NewResponseController(c.Writer()).SetWriteDeadline(time.Time{}); err != nil {
		return err
	}
	c.Writer().Header().Set("Content-Type", "text/event-stream")
	for event := range events {
		if _, err := fmt.Fprintf(c.Writer(), "data: %s\n\n", event); err != nil {
			return err
		}
		c.Writer().Flush()
	}
	return nil
})
```

The zero time removes the deadline rather than extending it, which is what a stream of unknown length needs. `http.ResponseController` reaches the connection through the framework's whole writer stack, so this keeps working under the gzip, request-timeout and idempotency middleware; those wrappers forward it rather than absorbing it.

Two bounds still apply and should not be worked around. `read_timeout` and `idle_timeout` are untouched by the call above, and `web.max_in_flight` counts a streaming request for as long as it is open -- a process serving many long-lived streams needs the ceiling raised deliberately, because each open stream is one admission slot that no other request can use.

WebSocket needs none of this: hijacking the connection clears the server's deadlines with it.

## Operational endpoints and secrets

Health endpoints return aggregate status by default. Use `detail_policy: never` to prevent unauthenticated probes from receiving dependency errors. When XBC begins graceful shutdown, readiness changes to 503 immediately while liveness remains Up. `web.shutdown.pre_drain_delay` defaults to `0s`, which begins HTTP draining immediately; configure a nonzero, deployment-specific interval when probes or load balancers need time to observe the readiness transition:

```yaml
web:
  shutdown:
    pre_drain_delay: 2s
```

Readiness only reports what contributes to it: selecting the health capability alone yields an empty `checks` array. `redis.Bundle()` and `gorm.Bundle()` each select a readiness probe next to their client Definition, so every configured instance is probed once the health Bundle is also selected -- reported as `redis-health` and `gorm-health` for the default instance, or `redis-health/<instance>` for a named one. They need that second Definition because their primary value is a third-party type. `elasticsearch`, `objectstorage`, `kafka`, `asynq`, and `raft` own their primary type, so it carries the contract directly and each check is named after the producing identity: `elasticsearch[search]`, `objectstorage`, `kafka[events]`, `asynq`, `raft`. An application component contributes the same way, by exporting `health.Contributor` from its own Definition. Checks inherit `plugins.health.timeout` unless the contributor sets a per-check timeout, and the neutral `plugins.health` section is separate from the HTTP-facing `plugins.health-http` section below.

What each check actually asks matters when reading a 503. `elasticsearch` and `asynq` probe their own connection; `plugins.elasticsearch.<instance>.health_probe: false` removes that cluster from readiness entirely, matching what it already does to the startup probe. `objectstorage` sends one `HeadObject` for a key expected to be absent -- a definitive "no such object" already proves the endpoint, credentials, and bucket are good, and nothing is written. Only an `s3` instance contributes; a `local` one has no peer to probe. S3 answers that request with 403 instead of 404 when the caller lacks `s3:ListBucket`, so such a deployment either grants the permission or sets `s3.health_probe: false`; a 403 is not accepted as reachable because that would also accept a revoked credential. `kafka` asks one broker for cluster metadata through the producer's own dialer, so the probe traverses the configured TLS and SASL path; reaching one broker is enough, which keeps a rolling restart from reporting the instance as unable to serve. `raft` asks whether the cluster has a leader, not whether this node is the leader, so a follower stays ready while a node that never bootstrapped and has no peers reports down.

Two integrations deliberately contribute nothing. `outbox` depends only on a `gorm` database that `gorm-health` already probes, and its publish failures land in the event table with a retry schedule. `cron` has no remote dependency unless distributed mode is enabled, and there a lost lock surfaces within seconds through its own logged acquisition and renewal failures.

The interval leaves the listener up after runtime cancellation so `/readyz` can return 503 before HTTP drain. It consumes the one shared runtime shutdown budget, still accepts ordinary traffic, and is not acknowledgement that a load balancer has withdrawn the instance. Choose it from probe and load-balancer convergence time while reserving enough budget for draining.

A startup that never reaches the listener is reported the other way round. `xbc.slow_startup_after` defaults to `30s`, and while startup is unfinished the runtime repeats a warning naming the phase plus the plugin and lifecycle stage still holding it:

```yaml
xbc:
  slow_startup_after: 30s
```

Read the `plugin` field first: it names the hook that has not returned, which the startup timing breakdown cannot do — that breakdown is emitted only after every phase returns, so a boot stuck in `Start` produces none of it. The threshold is a reporting threshold, not a deadline: nothing is cancelled or aborted, and the startup keeps waiting. Compare consecutive lines to tell the two failures apart — an unchanged phase and plugin mean stuck, a moving one means slow but progressing. An application whose migrations legitimately run for minutes should raise the value or set `0s` to switch the report off.

pprof is disabled by default. It carries no authentication mechanism of its own: it falls through to the `web.security` global default, which is `deny` out of the box. An application that enables it must register at least one authenticator, or startup fails with `requires authentication but no authenticator is registered`.

The `deny` default only means "must authenticate" -- it accepts any registered scheme, not "reachable by operators only". If the application registers an authenticator for any purpose and writes no tier-1 rule for these routes, pprof and metrics become reachable by any authenticated principal, not just operators. Restricting them to operators requires two things: a tier-1 rule that narrows the accepted scheme, and an authorization layer on top of authentication, because neither route carries a `.Perm` for Casbin or another authorizer to check (see below).

A tier-1 `match` pattern is matched against the route's full path, including the `web.base_path` prefix (`RouteInfo.Path` is built by joining the base path with the route's relative path). The example below only matches as written when `base_path: "/"`; an application running with `base_path: "/api/v1"` must write `/api/v1/debug/pprof/**` instead -- see the quickstart's own `/api/v1/docs/**` rule for a working example at a non-root base path.

```yaml
web:
  security:
    default: deny
    policies:
      - match: "/debug/pprof/**"
        authenticate: [jwt]
```

When Casbin is also selected, its `missing_permission` setting defaults to `deny`: a route with no `Perm` is rejected for everyone once Casbin's convention-based enforcement is active. The metrics route carries neither `Auth(web.Public())` nor a `.Perm`, so upgrading straight into that convention silently breaks Prometheus scraping with no startup warning. Fix this with a tier-1 `permit` rule for the exposition endpoint -- it takes effect before authentication and authorization run at all -- rather than flipping `missing_permission` to `allow`, which would also loosen every other `.Perm`-less route.

- **No role preservation across a restart.** A replacement process starts as a standby and wins a slot only once one frees, so a rolling update moves roles to whichever process wins next rather than keeping them where they were: a restored process may host nothing while a standby takes the role. That follows from roles being claims rather than labels, and it means a rolling update is not role-preserving.
```yaml
plugins:
  health:
    timeout: 2s

  health-http:
    liveness_path: "/healthz"
    readiness_path: "/readyz"
    detail_policy: never

  pprof:
    enabled: true
    path: "/debug/pprof"

  gracefulshutdown: {}
```

Signals are handled by the runtime: SIGINT and SIGTERM already drain the Web listener, then unwind every started plugin in reverse order within the shared shutdown budget, with no plugin or opt-in required. For application-triggered shutdown -- a plugin or any application code it holds deciding on its own that the process should stop -- select `gracefulshutdown.Bundle()`, activate it with `plugins.gracefulshutdown: {}`, and depend on its `*gracefulshutdown.Controller` to call `Request(reason)`; a plugin may instead call `ctx.RequestShutdown(reason)` on its own `plugin.Context` directly. Neither path offers or needs an HTTP endpoint: exposing process control over a route anyone can reach is a liability, not a convenience, so no such endpoint is offered by design.

Select the corresponding Bundles at the composition root before configuring these sections. Never commit JWT secrets, Redis or database passwords, API keys, or operations tokens to the repository. Environment variables are only a minimum deployment interface; production systems should inject them through a secret manager.

XBC does not echo these values. `doctor` output and startup reports contain only paths, identities, and source labels, while validation errors describe fields tagged with `mask:"true"` without reproducing their values.

Do not trust forwarded headers from the public network unless a trusted reverse proxy removes untrusted values first. Graceful shutdown, whichever way it is triggered, reuses the same unified cancellation, HTTP drain, and reverse-order plugin shutdown path, and it must not call `os.Exit` independently.


## Diagnosing a slow boot or a slow shutdown

Every boot logs how long it took to become servable on the released-gate line:

```
INFO  xbc: application traffic gate released  instances=14 order=... startup=7.562ms
```

Set `log.level` to `debug` to get the breakdown behind that number. It arrives as a single record listing the six ordered startup phases, then one line per instance naming only the stages that actually ran:

```
DEBUG xbc: startup timings, total 7.562ms
  phases: bootstrap 1.488ms, planning 4.492ms, construct 368µs, migrate 0s, start 1.021ms, traffic 185µs
  health                       factory 1.25µs, Init 3.041µs
  web                          factory 22.583µs, Start 1.019ms, OpenTraffic 185µs
```

Read the phase line first: it separates a slow configuration source or a slow plugin graph from a plugin that is slow to start. A stage the plugin never declared is absent rather than reported as `0s`, and a stage that ended in an error still reports what it spent.

The reverse unwind is reported the same way. A shutdown that stayed inside its budget records the per-instance waits at debug, which is what attributes a slow rolling restart to a plugin. The line states the pre-stop phase beside the walk, because the two are one stop to a supervisor:

```
DEBUG xbc: reverse unwind finished inside its budget  budget=15s pre_stop=1.583µs reason=signal total_budget=17s waited="[web 2.00123775s transcode 126.5µs heartbeat 38.75µs]"
```

A shutdown that ran out of budget warns instead, and the warning carries the same `waited` list alongside the plugins that were abandoned or never attempted -- the casualty list names who was cut off, the waits name who spent the budget.

These reports contain only identities, stage names, and durations; no configured value reaches them.


## Diagnosing why a plugin is in the graph

`doctor` answers two questions the enabled-instances table cannot: who selected each plugin, and what is actually feeding it. It groups the graph the same way the workload rows do -- each declared workload, then `unowned` -- and walks that order, printing per instance the composition site that introduced it and one line per declared input:
Both probes are registered public and unmetered: they answer before authentication and outside the process-level in-flight gate. The second exemption matters as much as the first. `web.max_in_flight` refuses work when the process is saturated, and a probe is not work; a busy process whose own liveness endpoint returned `503` would be restarted at exactly the moment it is carrying its full traffic ceiling, moving that traffic onto replicas that then fail their probes in turn. An orchestrator cannot tell "saturated" from "dead" unless the probe answers, and the two call for opposite responses. The price is that an unmetered probe is bounded by nothing but its own checks, which is why each check declares a timeout and the aggregator runs them concurrently. A probe that reaches a dependency is still a probe that spends that dependency's budget, so a saturated process answering them quickly is the design, not a gap in it.


```
unowned             plugins=14
  health
    selected at /Users/dev/xbc/extensions/reliability/health/plugin.go:48
    requires    many      health.Contributor                     from greeter
  health-http
    selected at /Users/dev/xbc/transport/web/extensions/reliability/health/plugin.go:56
    requires ref       *health.Plugin                         from health
  web-engine-gin
    selected at /Users/dev/xbc/transport/web/engines/gin/bundle.go:30
    no declared inputs
  web
    selected at /Users/dev/xbc/transport/web/plugin.go:95
    requires one       web.EngineFactory                      from web-engine-gin
    requires many      web.Middleware                         from accesslog, biz, cors, gzip, requestid, securityheaders, timeout
    requires many      web.ErrorMapper                        unsatisfied: no enabled plugin exports it
    requires many      web.RouteContributor                   from greeter, health-http, swag
    requires many      web.RouteCatalogListener               from swag
    requires many      authentication.Authenticator           unsatisfied: no enabled plugin exports it
    requires many      web.CredentialExtractor                unsatisfied: no enabled plugin exports it
```

`unsatisfied` is the line to look for. `ref` and `one` inputs cannot appear that way -- a missing or ambiguous producer fails planning with an error naming the consumer -- but `optional` and `many` inputs binding nothing is legal by design, which is what makes it dangerous: the application starts, nothing is logged, and the capability you selected a Bundle for is simply absent. The output above is the quickstart's own, and it is correct there: no authenticator or credential extractor Bundle is selected, which is exactly why its `web.security` rules may only `permit` and not `authenticate`, and no plugin contributes an error mapper, so errors fall back to Web's built-in problem mapping. The same three lines in a deployment that does select `jwt.Bundle()` mean the Bundle never reached the composition root, or its section is disabled -- and in that deployment the first authenticating rule would fail startup instead of silently letting a request through.

`selected at` is the `BundleOf` call that first introduced the Definition, as an absolute `file:line`. For a plugin selected through an aggregate such as `prelude.Bundle()`, that site is the owning package's own `Bundle()` rather than the aggregate, because that is where the Definition entered a Bundle; for an application plugin it is the application's own file. Selecting the same Definition twice -- an aggregate plus an explicit selection -- stays legal and is not reported as a conflict; the first selection wins and is the one printed. Selections that disagree on workload ownership are the exception: a Definition tagged by a workload at one selection and left plain at another fails planning with `conflicting workload ownership`, naming both selection points, rather than letting Bundle order decide the membership. Two *different* Definitions claiming one key is the real conflict, and that fails planning with both declaration and selection sites named.

Like the rest of `doctor`, this section prints only identities, contract type names, and source locations. Reading it constructs no plugin and starts no goroutine. The one thing `doctor` does reach for is the placement decision, which it resolves before planning: a lease source reads its store and wins its slots there, and the runtime gives them back before the command returns -- the acquisition is transient, and nothing it claims outlives the report that needed it.
## Reading the startup report

Before it releases the traffic gate, a successful start prints the assembled request pipeline and the decision behind every route. The `validate` subcommand prints the same report for the same composition without binding a listener, so the decision can be read -- and a mistake found -- from a process that serves nothing:

```sh
go run ./examples/quickstart validate --config examples/quickstart/application.yml
```

The policy table is the one to read when asking what an unauthenticated caller can actually reach:

```
INFO web: route table (7)
  1. GET   /api/v1/hello
  2. POST  /api/v1/hello
  ...
INFO web: public endpoints (7)
  1. GET   /api/v1/hello
  ...
INFO web: policy decisions (7)
  1. GET   /api/v1/hello         -> permit    (route)
  6. GET   /api/v1/docs          -> permit    (application-rule, rule 0)
```

Each policy row names the outcome and, in parentheses, the tier that decided it: `route` is the route's own `.Auth()` declaration, `application-rule` a rule from `web.security.policies` (`rule N` is that rule's index there), `default` the `web.security.default` fallback. The outcome is `permit`, `deny`, the scheme names the route authenticates with, or `authenticate(default: ...)` when the route resolves through the authenticator manager's own default selection rather than an explicit list. `public endpoints` lists every route that resolved to `permit` -- that is, every route served without authentication -- and warns there when `web.security.default` is `permit`, because a fail-open fallback makes every uncovered route public. `authentication arbitration order` appears when authenticators are registered, and prints the order actually in effect rather than the configured list.

This report is the only place route-level policy is visible, and the reason is structural: a route is contributed by its plugin during `Start`, and its decision needs the registered authenticators, so both exist only after construction. The start and `validate` are therefore the two commands that print it, and they print the same one: `validate` runs the identical assembly and stops before the listener. `doctor` deliberately constructs nothing, so it validates the configuration's shape -- `web.security.default` must be `deny` or `permit`, and every rule must be well-formed -- and reports the graph without ever printing a route table. Every check that needs the registered authenticators runs in that startup path instead, before the gate opens or the `validate` report is printed: a declared `web.security.schemes` order with no authenticator registered is refused, and so is a route that requires authentication with none available. Both fail the command rather than appearing as rows in the table above, which is the point -- a policy mistake fails the process instead of being served.


A `validate` run has its own pair, measured from the same anchor -- the moment argument parsing succeeded -- rather than from a gate release, so the two totals are comparable across commands. It closes with the always-on line, and at debug it lists one line per instance whose `Preflight` hook ran, which is the only stage the command runs beyond construction:

```
INFO  xbc: application validation finished  instances=14 validation=8.203ms
DEBUG xbc: validation timings, total 8.203ms
  web                          Preflight 7.912ms
```

