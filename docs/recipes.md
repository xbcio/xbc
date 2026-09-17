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

The reverse unwind is reported the same way. A shutdown that stayed inside its budget records the per-instance waits at debug, which is what attributes a slow rolling restart to a plugin:

```
DEBUG xbc: reverse unwind finished inside its budget  budget=15s reason=signal waited="[web 2.001s]"
```

A shutdown that ran out of budget warns instead, and the warning carries the same `waited` list alongside the plugins that were abandoned or never attempted -- the casualty list names who was cut off, the waits name who spent the budget.

These reports contain only identities, stage names, and durations; no configured value reaches them.


## Diagnosing why a plugin is in the graph

`doctor` answers two questions the enabled-instances table cannot: who selected each plugin, and what is actually feeding it. Its `selection and inputs` section walks the same start order and prints, per instance, the composition site that introduced it and one line per declared input:

```
selection and inputs, in start order
  health
    selected at /Users/dev/xbc/extensions/reliability/health/plugin.go:48
    requires many      health.Contributor                     from greeter
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
