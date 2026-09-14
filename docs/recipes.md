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

## Operational endpoints and secrets

Health endpoints return aggregate status by default. Use `detail_policy: never` to prevent unauthenticated probes from receiving dependency errors. When XBC begins graceful shutdown, readiness changes to 503 immediately while liveness remains Up. `web.shutdown.pre_drain_delay` defaults to `0s`, which begins HTTP draining immediately; configure a nonzero, deployment-specific interval when probes or load balancers need time to observe the readiness transition:

```yaml
web:
  shutdown:
    pre_drain_delay: 2s
```

The interval leaves the listener up after runtime cancellation so `/readyz` can return 503 before HTTP drain. It consumes the one shared runtime shutdown budget, still accepts ordinary traffic, and is not acknowledgement that a load balancer has withdrawn the instance. Choose it from probe and load-balancer convergence time while reserving enough budget for draining.

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
