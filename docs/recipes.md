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

Authentication is performed once by the framework's built-in authentication middleware; plugins contribute `authentication.Authenticator` and `web.CredentialExtractor` implementations but do not intercept requests themselves. Route access is decided by a three-tier precedence model: an explicit `web.security` policy rule (tier 1) overrides a route's `.Auth()` declaration (tier 2), and the global `web.security.default` (factory setting `deny`) covers routes that neither tier addresses. Protected routes receive the shared `web.Principal`, which lets audit records correlate the principal, route name, request ID, status code, and duration.

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

## Persistent Casbin policy with multi-replica synchronization

The base `casbin` extension reads static policy from inline content or a file. A service that must manage roles and permissions at runtime should explicitly compose `gorm.Bundle()`, `casbin-gorm.Bundle()`, `casbin-redis.Bundle()`, `casbin.Bundle()`, and the protocol-neutral `rbac.Bundle()` from `github.com/xbcio/xbc/extensions/authorization/rbac`. Connect capabilities by exact plugin key and instance:

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

  rbac:
    backend:
      plugin: casbin
      instance: default
    admin_role: admin
```

`casbin-gorm` defaults to `migrate: false` and never runs AutoMigrate implicitly during construction. When enabled, migration occurs only in XBC's Migrate stage. Casbin first loads policy at Start, after the table can exist. External-adapter mode enables AutoSave, so `AddPolicy`, role-relation changes, and filtered removals persist to the database. The Redis Watcher propagates changes to other replicas. The watcher owns its Redis clients; the named `gorm` plugin continues to own the GORM connection pool.

`github.com/xbcio/xbc/extensions/authorization/rbac` is a protocol-neutral application plugin with its own Definition, Config, Manager, Backend, Permission model, and autoload adapter. Application code should depend on `rbac.Manager` for administrator checks, AND/OR authorization, role and permission queries, and replacement or deletion of direct relations. The Casbin extension supplies its Backend. Depend directly on `casbin.EnforcerProvider` only for migrations or Casbin-specific advanced operations.

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

Asynq starts consuming only after `OpenTraffic`, freezes handler registration at Start, and stops admission before draining during shutdown. If committing business data and enqueueing work must be atomic, write an outbox event first and let its publisher enqueue the task. Do not rely on a post-commit dual write.

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

Distributed mode uses a random owner token, a `SET NX` lease with TTL, and Lua scripts for atomic renewal and release. Only the lease owner runs a given job among healthy replicas, and TTL expiry enables takeover after a node is lost.

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
```

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

`WithExclusiveProcess` is for a workload with process-wide side effects -- tuning a global GC target, setting a process-wide memory limit, sizing a pool every other plugin shares -- which nothing sharing its process can be protected from. The reason is not that the workload is heavy. A merely heavy workload is placed by its replica count and bounded by its own budget instead.

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
- `max_goroutines` defaults to `0` (unbounded) and bounds how many managed tasks this process may run on behalf of that workload. A task submitted past the budget is rejected and counted instead of started, and other workloads are unaffected. Plugins with no workload -- transports, infrastructure, observability -- are not bounded by it.
- Environment overrides use the full path: `XBC_WORKLOADS_SAST_ENABLED`, `XBC_WORKLOADS_CODERANGER_MAX_GOROUTINES`.

A `workloads.<key>` section that names no declared workload fails startup as an unowned key, exactly like a misspelled plugin section.

Placement decides the hosted set once, before the plugin graph is built. The default is static placement, which contacts nothing and hosts exactly what the configuration enables:

```go
app, err := xbc.New(
	xbc.WithPlacement(placement), // omitted -> xbc.StaticPlacement()
	xbc.WithBundles(prelude.Bundle(), ginengine.Bundle(), sast.Bundle()),
)
```

A workload this process does not host is not disabled: its Definitions never enter the plan at all. No instance is constructed, no connection pool is opened, no queue handler is registered, and no timer is created. Its routes do not exist in this process either, so a request for one is answered `404` rather than forwarded.

The process-level runtime knobs belong to the framework rather than to any plugin, so a plugin cannot change them for its own benefit:

```yaml
xbc:
  runtime:
    max_procs: auto      # derive from the container's CPU quota
    memory_limit: "75%"  # bytes, or a percentage of the container memory limit
    gc_percent: 0        # 0 leaves the Go default of 100 alone

web:
  max_in_flight: 0       # 0 derives from the effective GOMAXPROCS
```

`max_procs: auto` reads the container's cgroup CPU quota, so a container limited to two cores runs with `GOMAXPROCS=2` rather than the host's core count. `memory_limit` makes the runtime collect harder as the container approaches its limit instead of being killed. A build in which a plugin called `debug.SetGCPercent`, `debug.SetMemoryLimit`, or `runtime.GOMAXPROCS` outside the framework fails the repository's architecture guard, which is what makes the values `doctor` reports worth trusting.

The two defaults are deliberately asymmetric, and this block is a recommendation rather than a description of them. `max_procs` defaults to `auto` because sizing the scheduler from the host's cores inside a quota is a factual error with no trade-off to weigh. `memory_limit` defaults to `0`, no limit, because a soft limit trades CPU for heap and only the deployment knows how much of each it wants -- and because a percentage needs a derivable container limit, so a framework default of `"75%"` would fail startup on bare metal for a deployment that configured nothing. `"75%"` is the value to write for a containerized process; outside a container write a byte count or leave it at `0`.

`workloads`, `xbc.runtime`, `xbc.pre_stop_timeout`, and `web.max_in_flight` are introduced together with workload placement. A deployment that declares no workload keeps exactly its previous behaviour.

`web.max_in_flight` reports itself as a pair of state transitions rather than per refused request. The refusal that finds the process newly saturated logs `web: in-flight limit reached, refusing requests until in-flight work drains` at warn with `limit`, `rejections`, `rejections_total`, and `retry_after_seconds`; the release that leaves nothing in flight logs `web: in-flight limit cleared, admitting requests again` at info with `limit`, `rejections`, and `rejections_total`. `rejections` counts only what was refused since the gate's previous line -- the part nobody has seen yet -- while `rejections_total` is the count since boot, so consecutive lines can be compared without double counting. One saturation episode therefore produces at most those two lines however long it lasts, and it ends only once in-flight work drains to zero rather than merely below the ceiling: the number of log lines is not a proxy for the number of refusals, and a process parked at its ceiling reports one episode where an operator might have counted several. Read `rejections_total` or `Server.InFlightStats()` for the quantity, and treat the warn line as the episode's start rather than as a per-request signal.

## Slots, standbys, and how many processes to start

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

The `*placement.Placement` is both the decision and the plugin that keeps it alive, so it is passed to `WithPlacement` and its `Bundle()` is selected alongside the workloads. There is no placement constructor on the `xbc` facade itself: core's dependency closure excludes everything beneath `extensions/`, so the lease contract cannot be named there.

That is one extra connection to the store, and it is the price of deciding the hosted set before the graph is built rather than during construction.

Takeover is a restart, not a live handover. When a holder is lost, its slot does not become available until its lease expires, then a standby has to notice, and the process the supervisor starts has to come back up before the role is really served again:

```
takeover latency = lease TTL + standby retry interval + process restart time
```

The lease TTL dominates, and it is three times the renew interval by default. With a 10s renew interval, a 30s TTL, a standby retrying every 5s, and a 2s restart, the worst case is `30 + 5 + 2 = 37s`.

That is acceptable here because these workloads are queue-backed. A task in flight when the holder died is redelivered by the queue, and mutual exclusion between the failed holder and its successor is the queue's, the distributed lock's, and the database's job rather than the lease's.

Renewal failing is treated differently from a cold start, and the asymmetry is deliberate:

- **A running holder keeps its role.** A failed renewal is logged, counted, and otherwise ignored: the process does not release its slot and does not exit. The worst outcome is a workload briefly running more replicas than declared. That is a resource problem, not a correctness one, and the alternative -- dropping the role on a lease-store hiccup -- would reshuffle roles across the whole cluster.
- **A cold start that cannot reach the lease store fails.** A process that cannot claim anything would have to guess, and the only guess available is "carry everything". That makes the process shape non-deterministic: `doctor` output, startup validation, snapshot diffing, and the exclusivity check all derive from the hosted set, and the capacity decision becomes fail-open. A holder has something to protect; a starter has nothing to guess with.

Alert on the renewal-failure counter, where a sustained increase means the store is degraded and takeover is impaired, and on the lease age, where a holder's lease age growing without a matching renewal success means renewal is stalling. Aggregate the held gauge by workload across processes to see how many replicas each workload actually has; a value above `replicas` is the soft-placement case above, not a bug. A process restart on its own is expected rather than alarming -- that is what takeover looks like.

Those three series are not published by the placement module itself. Core owns no metrics registry -- the Prometheus registry is a Web extension, and the placement module deliberately does not depend on a transport -- so it exposes a `placement.Stats()` snapshot instead and the three series are what a bridge over that snapshot should publish:

| Series | Source field |
| --- | --- |
| `xbc_workload_held{workload}` | one series per `Stats().Held` entry |
| `xbc_workload_lease_age_seconds{workload}` | `Stats().Held[].Age` |
| `xbc_workload_lease_renew_failures_total` | `Stats().RenewFailures` |

`Stats().Held[].Degraded` is the per-slot form of the same signal: it reports "still serving, but the claim is not being confirmed", which is the difference between a degraded store and a stopped process. A readiness probe already exported for the health aggregator (`placement-health`) carries the same verdict without any metrics stack, and it reads this process's own renewal state only -- never the store, and never other members.

Two different identities appear when asking "who holds this slot", and they answer different questions. `xbc.instance_id` names the process: derived from the hostname, the boot second and a random suffix when left empty, and settable per process as `XBC_INSTANCE_ID` -- see [`xbc.instance_id`](quickstart.md#configuration) for the derivation and the whitespace rule. The slot's *owner token* is what the lease backend stores under the slot key and what it compares before renewing or releasing; it identifies one acquisition rather than a process.

The identity is what gets reported, because it is the one an operator can act on. `doctor`'s `holder` line and `Stats().Instance` both carry it, so a report names a process you can go and look at rather than a token. The token remains available per slot as `Stats().Held[].Owner`.

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

Shutdown now has two phases. `xbc.pre_stop_timeout` defaults to `2s` and runs `PreStop` on every started plugin before any `Stop` begins; that is where a placement plugin gives its slot back. `xbc.shutdown_timeout` defaults to `30s` and then covers cancellation, HTTP drain, and reverse-order `Stop`:

```yaml
xbc:
  pre_stop_timeout: 2s
  shutdown_timeout: 30s
```

The total budget is their sum, `2s + 30s = 32s`, and the supervisor must allow at least that much before it kills the process. A supervisor that kills earlier interrupts the release, and the slot then waits for its TTL instead of being freed immediately.

systemd's `TimeoutStopSec` and Docker's `stop_grace_period` are the two settings that matter. Docker's default of `10s` is shorter than the default budget and is the one that bites in practice:

```ini
# systemd
[Service]
ExecStart=/usr/local/bin/orders --config /etc/orders/application.yml
Restart=always
RestartSec=1s
KillSignal=SIGTERM
# 2s pre_stop_timeout + 30s shutdown_timeout = 32s; 45s leaves scheduling headroom.
TimeoutStopSec=45s
```

```yaml
# docker compose
services:
  orders:
    image: registry.internal/orders:1.2.3
    restart: always
    # Default is 10s, which is shorter than the 32s stop budget.
    stop_grace_period: 45s
```

Setting `pre_stop_timeout: 0s` skips the phase and makes the total `shutdown_timeout` alone.

The restart policy is not optional. Takeover works by a standby requesting shutdown on purpose once it has won a slot, and the supervisor is what brings that process back as the real holder. Without `Restart=always` or `restart: always`, the first takeover turns a standby into a stopped container.

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

With the default static placement each job hosts exactly the workloads its file enables, and nothing else. An application that selects a lease-backed placement must give this one-off job a way to run without it -- a separate composition root, or a switch `main` reads -- because otherwise "hosts everything" depends on winning every slot in a race, and a migration that wins only some of them silently migrates a subset.

Verify before trusting the run. `doctor` resolves the hosted set without constructing anything, so every declared workload in that run must appear as hosted:

```sh
orders doctor --config /etc/orders/migrate-rest.yml
```

`xbc.auto_migrate` defaults to `false`, so the flag is a deliberate opt-in and an ordinary boot never mutates a schema.

## Attributing CPU to a workload

Every managed task -- anything submitted through `plugin.Context.Go` or `GoCritical` -- runs under a `workload` profiler label naming the workload its plugin belongs to. A CPU profile can therefore be read per role:

```sh
# CPU by role, for every labelled workload at once
go tool pprof -tags http://127.0.0.1:8080/debug/pprof/profile?seconds=30

# only one role's stacks
go tool pprof -tagfocus=workload=ingest http://127.0.0.1:8080/debug/pprof/profile?seconds=30
```

`-tags` prints one line per workload with its share of the profile; `-tagfocus` narrows every later view to that workload's samples.

The label exists because a stack cannot answer the question. Frames say which plugin is burning CPU; workload membership is decided at composition, so the same binary attributes the same function to different workloads depending on which slots each process won.

Two limits are worth knowing before reading a profile this way:

- **It covers background work, not request handling.** Labels are inherited by goroutines started under them, and the Web server belongs to no workload -- its accept loop serves every workload's routes. Labelling it would file each request under a name that denies the workload actually being served, so shared plugins are left unlabelled and request CPU is untagged.
- **Untagged is not a workload.** Samples with no `workload` tag are the shared infrastructure plus the runtime itself. There is no `unowned` tag to focus on, deliberately: `unowned` is a legal workload key.

Heap profiles carry no labels at all, so memory is not attributable this way. `xbc.runtime.memory_limit` bounds the process rather than a role.

## What workload placement does not do

These boundaries are deliberate, and knowing them prevents several wrong deployments:

- **No runtime re-placement.** Roles are claimed once at startup and held for the life of the process. Changing a process's role means restarting it, because a workload's registration work -- queue handlers, timer callbacks, consumers -- happens once at construction and cannot be undone.
- **No in-process request forwarding and no cluster routing table.** A request for a workload this process does not host is a `404` here. A management tool talks to the process that holds the role rather than to "the service" as a whole.
- **No member enumeration and no service-discovery contract.** Each process reports only what it holds. Aggregating that into "how many replicas of `sast` are running" is the monitoring side's job, which is why the metrics above are per-process.
- **No fencing tokens, split-brain detection, or lease generations.** Those are what hard mutual exclusion needs, and the lease is not that.
- **No per-workload HTTP in-flight budget.** `web.max_in_flight` is process-wide; a request over the limit is answered `503` with `Retry-After` before any handler runs. A per-workload share would require resolving the route before admitting the request, which is exactly the work the gate exists to refuse before.
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

pprof and remote shutdown are disabled by default. Neither endpoint carries its own authentication mechanism: they fall through to the `web.security` global default, which is `deny` out of the box. An application that enables them must register at least one authenticator, or startup fails with `requires authentication but no authenticator is registered`.

The `deny` default only means "must authenticate" -- it accepts any registered scheme, not "reachable by operators only". If the application registers an authenticator for any purpose and writes no tier-1 rule for these routes, pprof, metrics, and remote shutdown become reachable by any authenticated principal, not just operators. Restricting them to operators requires two things: a tier-1 rule that narrows the accepted scheme, and an authorization layer on top of authentication, because none of these three routes carries a `.Perm` for Casbin or another authorizer to check (see below).

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

  gracefulshutdown:
    http:
      enabled: true
      path: "/-/shutdown"
```

Select the corresponding Bundles at the composition root before configuring these sections. Never commit JWT secrets, Redis or database passwords, API keys, or operations tokens to the repository. Environment variables are only a minimum deployment interface; production systems should inject them through a secret manager.

XBC does not echo these values. `doctor` output and startup reports contain only paths, identities, and source labels, while validation errors describe fields tagged with `mask:"true"` without reproducing their values.

Do not trust forwarded headers from the public network unless a trusted reverse proxy removes untrusted values first. Remote shutdown reuses core's unified cancellation, HTTP drain, and reverse-order plugin shutdown path; it must not call `os.Exit` independently.


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
    selected at /Users/dev/xbc/transport/web/plugin.go:87
    requires one       web.EngineFactory                      from web-engine-gin
    requires many      web.Middleware                         from accesslog, biz, cors, gzip, recovery, requestid, securityheaders, timeout
    requires many      web.ErrorMapper                        unsatisfied: no enabled plugin exports it
    requires many      web.RouteContributor                   from greeter, health-http, swag
    requires many      web.RouteCatalogListener               from swag
    requires many      authentication.Authenticator           unsatisfied: no enabled plugin exports it
    requires many      web.CredentialExtractor                unsatisfied: no enabled plugin exports it
```

`unsatisfied` is the line to look for. `ref` and `one` inputs cannot appear that way -- a missing or ambiguous producer fails planning with an error naming the consumer -- but `optional` and `many` inputs binding nothing is legal by design, which is what makes it dangerous: the application starts, nothing is logged, and the capability you selected a Bundle for is simply absent. The output above is the quickstart's own, and it is correct there: no authenticator or credential extractor Bundle is selected, which is exactly why its `web.security` rules may only `permit` and not `authenticate`, and no plugin contributes an error mapper, so errors fall back to Web's built-in problem mapping. The same three lines in a deployment that does select `jwt.Bundle()` mean the Bundle never reached the composition root, or its section is disabled -- and in that deployment the first authenticating rule would fail startup instead of silently letting a request through.

`selected at` is the `BundleOf` call that first introduced the Definition, as an absolute `file:line`. For a plugin selected through an aggregate such as `prelude.Bundle()`, that site is the owning package's own `Bundle()` rather than the aggregate, because that is where the Definition entered a Bundle; for an application plugin it is the application's own file. Selecting the same Definition twice -- an aggregate plus an explicit selection -- stays legal and is not reported as a conflict; the first selection wins and is the one printed. Two *different* Definitions claiming one key is the real conflict, and that fails planning with both declaration and selection sites named.

Like the rest of `doctor`, this section prints only identities, contract type names, and source locations. Reading it never constructs a plugin, opens a connection, or starts a goroutine.
