# Quickstart

This guide runs the repository's self-contained Web example, explains its explicit plugin composition, and covers the configuration needed to adapt it. The example requires no database, Redis, broker, object store, or cluster peer.

## Requirements

Install Go 1.25 or newer, then run all commands from the repository root.

## Validate and start the example

Inspect the complete application plan without constructing resources or opening a listener:

```bash
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
```

`doctor` validates configuration, plugin activation, typed inputs and contracts, and dependency order. It then reports the enabled instances in start order, where each one was selected, and which producers were bound to each of its declared inputs -- including the optional and collecting inputs that were bound to nothing. Its output includes source paths, plugin identities, and contract type names, but never configuration values. See [Diagnosing why a plugin is in the graph](recipes.md#diagnosing-why-a-plugin-is-in-the-graph) for how to read that section.

Because it constructs nothing, anything that exists only after construction is absent from its output by design. The most visible case is the route table: routes are contributed by their plugins during start, so `doctor` cannot list them or the authentication policy each one resolves to.

`validate` covers exactly that gap:

```bash
go run ./examples/quickstart validate --config examples/quickstart/application.yml
```

It constructs every enabled plugin and runs their `Preflight` hooks -- the state a start would build, from each contributed route to the compiled authentication policy -- then prints the same report a start prints just before it opens the gate: the middleware chain in order, the route table, and the policy decision behind each route. It stops short of activating anything: no listener is bound, so it can run beside a serving instance, and the process exits with the report. The plugins a real boot still has to prove -- anything that only fails when it is running -- remain the start's to discover.

A successful check closes with `xbc: application validation finished`, stating how many instances were checked and how long that took, so a pipeline gets a duration to watch and not only an exit code. At `debug` level the log breaks that time down per plugin, by `Preflight` stage.

Constructing is not the same as touching nothing: a plugin's `Init` runs here exactly as it would at boot, so a composition that opens a database or a queue connection really does contact it. `doctor` is the command that touches nothing; `validate` is the one that finds out whether the connections a boot would open can be opened.

`validate` never migrates. Neither `--migrate` nor `xbc.auto_migrate` is consulted on that path, so a check run against a deployment cannot change its schema first; run `migrate` (or a normal boot with the flag) when the schema is what should move.

Start the application:

```bash
go run ./examples/quickstart --config examples/quickstart/application.yml
```

The process listens on `localhost:8080` and serves routes below `/api/v1`.

The example also registers a subcommand of its own, the seam an application uses for one-off management commands:

```bash
go run ./examples/quickstart --config examples/quickstart/application.yml probe
```

`probe` runs instead of booting, which is what lets it run beside a serving instance: it reads the address, base path, and liveness route from the same merged configuration a boot would serve from, opens and closes its own connection, and exits -- 0 when the endpoint answered, 1 with its error otherwise. A command's arguments are its own, so the shared flags precede its name; `probe --config ...` is a mistake the command reports rather than a check that silently read another file. See [Shipping the service's own subcommands](recipes.md#shipping-the-services-own-subcommands).

## Understand the composition root

[`examples/quickstart/main.go`](../examples/quickstart/main.go) selects every capability explicitly:

```go
func main() {
    xbc.Run(xbc.WithBundles(
        prelude.Bundle(),
        ginengine.Bundle(),
        biz.Bundle(),
        cors.Bundle(),
        swag.Bundle(),
        greeter.Bundle(),
        async.Bundle(),
    ))
}
```

`xbc.Run` owns the process on the application's behalf: it reads the command line, subscribes to `SIGINT` and `SIGTERM` to start a graceful shutdown, abandons that shutdown and terminates when a stop signal repeats, reports a failure, flushes buffered log records, and exits with the command's code. It does not return, so `main` needs nothing else.

Use `xbc.New` and `App.Execute` instead when the process is not XBC's to own -- when the application must supply its own parent context, or when XBC is embedded in a larger process that already handles signals and decides the exit code. That pair touches none of the facilities listed above, which then belong to the caller.

`prelude.Bundle()` contributes the production Web baseline: the Web server, request IDs, access logging, security headers, compression, cooperative timeouts, and health probes. Contributing is not enabling. The server and the health probes are active as soon as the Bundle is selected; access logging, request IDs, security headers, compression, and the cooperative timeout each stay dormant until their `plugins.<key>` section exists in the merged configuration -- a YAML section or a single environment variable both count, and `application.yml` below configures all five. The Web runtime is engine-neutral, so the HTTP engine is a separate choice: `ginengine.Bundle()` from `github.com/xbcio/xbc/transport/web/engines/gin` supplies it, and exactly one engine Bundle must be selected. The response envelope, CORS policy, generated Swagger UI, and application-owned Greeter remain explicit choices.

The panic boundary is not part of this list because it is no longer a plugin: the Server assembles it unconditionally for every `web.Bundle()` composition, alongside the process-level in-flight gate, the request body ceiling, and the error resolver. It sits outermost among the ordered middleware -- outside even `prelude`'s own entries -- but still inside those three framework-owned stages, so its guarantee covers middleware and routing, not `net/http` itself or the gate ahead of it. See `web.recovery` under [Configuration](#configuration) below.

`async.Bundle()` adds a drained background task pool: XBC's analogue of a managed task executor. `greeter`'s `createGreeting` handler uses it to fire a best-effort "welcome" follow-up through `tasks.Go` -- which dispatches the function to the process-wide executor this Bundle installs -- after building its response, without making the request wait on it; see [`examples/quickstart/internal/greeter/greeter.go`](../examples/quickstart/internal/greeter/greeter.go) and the `async` package documentation for why the task outlives the request's own context and is still drained on shutdown.

A Bundle contains side-effect-free composition data. XBC finishes configuration and dependency planning before any plugin factory runs, then owns successfully constructed resources through reverse-order shutdown.

## Generate the API document

The `swag` extension serves a document generated by [`swaggo/swag`](https://github.com/swaggo/swag); it does not infer business operations from XBC's runtime route catalog. General API annotations live in `examples/quickstart/main.go`, while request, response, and operation annotations stay beside the business handlers. For example:

```go
// createGreeting returns a greeting for the submitted name.
//
// @Summary Create a greeting
// @Tags greetings
// @Accept json
// @Produce json
// @Param request body GreetingRequest true "Greeting request"
// @Success 200 {object} biz.Response[Greeting]
// @Failure 400 {object} web.ProblemDetail
// @Router /hello [post]
func (*Plugin) createGreeting(_ context.Context, c *web.Ctx) error { /* ... */ }
```

Regenerate the committed document after changing an annotation or API model:

```bash
go generate ./examples/quickstart
```

The `go:generate` directive pins `github.com/swaggo/swag/cmd/swag` to `v1.16.6`. The generated `examples/quickstart/docs` package registers the named document through its normal Go `init`; the composition root blank-imports that package and selects `swag.Bundle()`. Normal builds and startup therefore neither scan source comments nor run an offline generator.

## Configuration

The runnable baseline is [`examples/quickstart/application.yml`](../examples/quickstart/application.yml). Its top-level sections have distinct owners:

- `xbc` configures the core runtime.
- `log` configures logging.
- `web` configures the HTTP transport selected by `prelude.Bundle()`.
- `plugins.<key>` configures the plugin with that key.
- `app` is a free-form namespace for application settings.

Only Bundles selected at the composition root can own configuration sections. A misspelled section, an unknown field in a typed section, an unknown key under a plugin that declares no configuration of its own and therefore accepts only `enabled`, or configuration for an unselected plugin fails startup instead of being ignored.

**Breaking change:** the panic boundary moved from the optional `recovery` plugin into the Web server itself. `plugins.recovery.stack` is now `web.recovery.stack`, owned by the `web` section like `web.security` and `web.max_in_flight`. A configuration file still carrying `plugins.recovery` fails startup rather than silently keeping the old behavior -- strict decoding rejects it as an orphaned section once the plugin is gone. Migrate by moving the `stack` key:

```yaml
# before
plugins:
  recovery:
    stack: true

# after
web:
  recovery:
    stack: true
```

There is no enable/disable switch for it: the boundary is assembled unconditionally for every `web.Bundle()` composition, so there is no `enabled` key to carry over.

The boundary's log lines also change owner: they were emitted by the `recovery` plugin and are now emitted by the `web` plugin, so an alert or filter keyed on `plugin=recovery` must be repointed at `plugin=web`. The messages themselves are unchanged.

The example sets `web.shutdown.pre_drain_delay: 2s` so `/readyz` can return 503 after runtime cancellation before HTTP draining begins. The transport default is `0s`; a nonzero deployment-specific interval consumes the shared shutdown budget and still accepts ordinary traffic, so it is a propagation opportunity rather than acknowledgement that a load balancer has withdrawn the instance.

`xbc.slow_startup_after` (default `30s`) covers the opposite end: while startup has not finished, the runtime repeats a warning naming the phase it is in and the plugin and lifecycle stage still holding it, for example `phase=start plugin=gorm[primary] stage=Start`. The `plugin` field is the one to act on — it names the hook that has not returned. This is a report and not a timeout: nothing is cancelled or aborted, and startup keeps waiting for the plugin however long it takes. Reports repeat so consecutive lines can be compared: an unchanged phase and plugin mean stuck, a moving one means slow but progressing. Set it to `0s` to switch the report off, or raise it, when migrations legitimately run for minutes.

`xbc.runtime` holds the process-level resource knobs, and `web.max_in_flight` holds the transport's admission ceiling:

```yaml
xbc:
  runtime:
    max_procs: auto      # auto | a positive processor count | 0 (leave GOMAXPROCS alone)
    memory_limit: 0      # bytes | a percentage such as "75%" | 0 (no limit)
    gc_percent: 0        # 0 leaves the Go default of 100 alone

web:
  max_in_flight: 0       # 0 applies the fixed default ceiling (1024 concurrent requests)
```

They live there rather than in a plugin because each is process-global: a plugin that set one would be tuning the whole process from inside one component, and an architecture guard fails the build for one that tries. The resolved values and their sources are logged once per boot and printed by `doctor` as `max_procs=auto (runtime) memory_limit=1GiB (cgroup, 75%) gc_percent=default (unset)`. `max_procs: auto` installs nothing and leaves the count to the Go runtime, which sizes the scheduler from the container's cgroup CPU quota rather than from the host's core count -- a container limited to two cores runs with `GOMAXPROCS=2` -- and keeps re-reading that quota while the process runs. Setting a count, here or in the `GOMAXPROCS` environment variable, turns that re-reading off; write a positive integer only when the width should stay fixed as the container's limits change. An explicit `0` leaves the process value alone too, and reads as `default (unset)` on the line rather than as `auto (runtime)`. `memory_limit` written as a percentage needs a derivable container limit, and fails startup rather than silently doing nothing without one. A request past `web.max_in_flight` is refused with `503` and `Retry-After` before any handler runs, rather than queued; the health probes are the one built-in exception, registered unmetered so a saturated process can still answer the orchestrator that would otherwise restart it. The default ceiling is a fixed count of concurrent requests, not a multiple of the processor count: a request waiting on a database or an upstream holds no CPU, so size the key from the service's memory and dependency budget rather than from `max_procs`.

`xbc.pre_stop_timeout` budgets the pre-stop phase, which runs before shutdown proper and is accounted separately from `xbc.shutdown_timeout`, so the worst case for one stop is the two added together (`2s` + `25s` by default). It exists as its own knob because pre-stop retracts a value's externally visible participation while the process is still fully alive — a lease release is a real remote call — whereas Stop runs against an already-cancelled execution context. The `2s`/`25s` split is deliberate: their sum stays below Kubernetes' default `terminationGracePeriodSeconds` of `30s`, so a pod left at XBC's own defaults is not killed mid-shutdown by a cluster that never set the grace period explicitly.

`xbc.drain_timeout` budgets the drain phase, which is not a third budget beside the other two: it runs *inside* `xbc.shutdown_timeout`, after the process has stopped accepting ingress and before the remaining plugins' `Stop` runs, so it does not change the `2s + 25s` sum above. It exists for a plugin that accepted work it must finish handing over before being torn down — the `async` task pool and background workers such as `asynq`, `cron`, `kafka` consumers, `webhook` queues, the `elasticsearch` bulk indexer, and async `auditlog` dispatch implement it — the same role Spring's executor `SmartLifecycle` phase plays between the web container's graceful shutdown and bean destruction. Left unset, it defaults to 60% of the effective `xbc.shutdown_timeout` (`25s` → `15s`), which is always strictly smaller than it and therefore never trips the containment rule below, whatever `xbc.shutdown_timeout` a deployment chooses. `0s` skips the phase entirely, which is the documented off switch, matching `xbc.pre_stop_timeout`'s. Because the phase runs inside the shutdown budget, an explicitly set `xbc.drain_timeout` must be strictly less than `xbc.shutdown_timeout` whenever it is positive; a deployment that sets both explicitly must lower `xbc.drain_timeout` or raise `xbc.shutdown_timeout` to satisfy that.

`xbc.instance_id` names this process. It is the token the runtime prints on the placement line at startup (`instance=…`), the one an operator correlates a log line with when several processes run from one configuration file, and the name published into any store the replicas share — `doctor` reports it on the `holder` line, a lease-backed placement source writes it into the slot key, and a distributed cron lock carries it too, so reading a key answers which process holds a slot or is running a job. Plugins reach the same string through `plugin.Context.ProcessInstance`. Left empty it is derived — the hostname, the boot second, and a short random suffix, as in `host-7-1758091200-9f3c1a2b` — so two processes on one host are distinguishable with no deployment input at all, and a restart is visibly a different process. Because YAML does not expand `${VAR}`, a per-process value comes from the environment as `XBC_INSTANCE_ID`:

```bash
XBC_INSTANCE_ID=orders-3 \
  go run ./examples/quickstart --config examples/quickstart/application.yml
```

An explicitly set value is used exactly as written and is not made unique per boot: naming a process means that name to survive a restart. Whitespace in it fails startup, because it is presented as one token wherever it is printed.

### Files and profiles

Without `--config`, XBC uses the first file found in the current working directory:

1. `application.yml`
2. `configs/application.yml`

An explicit `--config <path>` must exist. Add `--profile prod` to merge an optional sibling such as `application-prod.yml`; when the flag is absent, the runtime reads `XBC_PROFILE`.

For a normally executed XBC application, precedence from lowest to highest is:

1. Go struct `default` tags, which fill a field only while it is still at its zero value
2. The non-zero values `ConfigSpec.Defaults` pre-fills
3. Base YAML
4. Profile YAML
5. Environment variables

A map or list written in the configuration replaces a pre-filled value rather than merging into it: a `queues` map a plugin pre-fills with `{default: 1}` becomes exactly the keys the file writes. Scalars are decoded without coercion, so a fractional number for an integer field, a quoted numeric string, or a boolean where a string is expected fails startup instead of being truncated or rewritten into a value nobody wrote. A list is not widened from a single scalar either: `skip_paths: /healthz` where a list field is expected fails startup rather than becoming a one-element list — write `skip_paths: ["/healthz"]`.

Two rules matter when authoring a plugin's `Config` struct. A `default` tag inside a pointer sub-struct allocates that sub-struct even when the user configured nothing, so a `required` field inside it would fail for every user who left the optional block out — validate such a block conditionally in `Prepare` instead. And a `default` tag never overwrites a value `Defaults` pre-filled; it supplies a value only where the field is still at its zero value.

YAML strings do not expand `${VAR}`. Supply deployment values through environment variables or an application-owned secret provider instead.

### Environment variables

Environment variables use the complete configuration path with the `XBC_` prefix. Dots and hyphens become underscores and letters become uppercase:

| Configuration path | Environment variable |
| --- | --- |
| `xbc.shutdown_timeout` | `XBC_SHUTDOWN_TIMEOUT` |
| `xbc.drain_timeout` | `XBC_DRAIN_TIMEOUT` |
| `xbc.instance_id` | `XBC_INSTANCE_ID` |
| `xbc.runtime.max_procs` | `XBC_RUNTIME_MAX_PROCS` |
| `web.addr` | `XBC_WEB_ADDR` |
| `web.management.addr` | `XBC_WEB_MANAGEMENT_ADDR` |
| `web.tls.cert_file` | `XBC_WEB_TLS_CERT_FILE` |
| `plugins.jwt.secret` | `XBC_PLUGINS_JWT_SECRET` |
| `plugins.redis.cache.password` | `XBC_PLUGINS_REDIS_CACHE_PASSWORD` |

A section whose root already spells the `XBC_` prefix does not repeat it. The framework's own section is rooted at `xbc`, so `xbc.shutdown_timeout` is `XBC_SHUTDOWN_TIMEOUT` — never `XBC_XBC_SHUTDOWN_TIMEOUT`. The rule matches a whole path segment, so an unrelated section named `xbcx` would keep its complete spelling.

For example:

```bash
XBC_WEB_ADDR=:9090 \
XBC_SHUTDOWN_TIMEOUT=20s \
  go run ./examples/quickstart --config examples/quickstart/application.yml
```

Environment variables are a complete configuration layer. They can activate a selected plugin whose section is absent from YAML and can declare named instances such as `redis.cache`. They cannot activate code whose Bundle was not selected. A variable that names no declared section, or no field of the section it names, fails startup rather than being ignored. `XBC_PROFILE` is reserved for the loader itself and never names a section.

### Reloading at runtime

A running process watches the files its boot read — the base file and the profile sibling, watched whether or not the overlay exists yet, so `application-prod.yml` appearing after the start is picked up like any other change. On a change, the whole configuration is re-read and validated exactly as it was at startup: the same strict decoding, the same section ownership, the same environment overlay. A reload is all-or-nothing: a save caught mid-write, a parse error, or a key that would not have been accepted at boot rejects the entire reload, the process keeps running the configuration it already holds, and the warning names the paths that failed. A file that mixes a change the process can take with one it cannot applies neither half.

**What a reload can and cannot move.** `log.level` is the one leaf that moves in place: the running backend switches to the new level, and every logger a plugin already holds observes it. Anything under `app` is accepted and reported but never interpreted — the runtime will not reject a reload over the section that belongs to the application author. Everything else requires a restart: any change under `plugins.*` or `workloads.*` is a graph change (an `enabled` flip is as much one as a new instance), and every `xbc.*` value was consumed once during startup — the budgets sized a shutdown that is already planned, `instance_id` names a lease the process holds, `auto_migrate` gated a stage that has run. The rejection warns with the paths and the files that set them, so the operator knows which file to edit:

```
xbc: log level changed  level=debug previous=info
xbc: configuration reload rejected; these paths require a restart  paths="[plugins.gorm.enabled (file /etc/app/application.yml)]"
```

**Environment variables never change at runtime.** They are read once by the boot and a reload re-reads the same process environment, so a value that only environment variables can change requires a restart. Reload is driven by file changes alone: there is no SIGHUP handler, and no signal re-reads the configuration.

If a watched file is deleted, the watch re-arms rather than going deaf — a file that reappears is reported, which is the ConfigMap-style atomic swap — and a change landing in the brief re-arm gap is reported by the next round rather than dropped. If the watch cannot be armed at all, the process logs a warning and serves on without reload rather than failing to start. `xbc doctor` prints the watched files and the same applies/accepts/restart summary without arming anything, so a rollout can answer "what would a reload do with this change" before it swaps the file.

## Exercise the API

In another terminal, call the example routes:

```bash
curl -i localhost:8080/api/v1/hello
curl -i -H 'Content-Type: application/json' \
  -d '{"name":"XBC"}' localhost:8080/api/v1/hello
curl localhost:8080/api/v1/healthz
curl localhost:8080/api/v1/readyz
curl localhost:8080/api/v1/swagger.json
```

Open <http://localhost:8080/api/v1/docs> for the Swagger UI.

Malformed JSON, unknown fields, validation failures, routing errors, authentication failures, and unexpected server errors use RFC 9457 Problem Details. Unexpected errors never expose their internal cause.

## Add a capability

To add Redis, import its package, select its Bundle, and configure a named instance:

```go
xbc.Run(xbc.WithBundles(
    prelude.Bundle(),
    ginengine.Bundle(),
    biz.Bundle(),
    cors.Bundle(),
    swag.Bundle(),
    greeter.Bundle(),
    async.Bundle(),
    redis.Bundle(),
))
```

```yaml
plugins:
  redis:
    default:
      addr: "127.0.0.1:6379"
```

Selecting a Bundle makes the implementation available; its activation policy and configuration determine whether XBC constructs an instance.

## Run one binary as several roles

A single binary does not have to serve the whole application. The application declares its heavier parts as **workloads** — named groups of Definitions a process carries as a unit or not at all — and placement decides which of them a given process hosts. Roles then become a deployment question rather than a second `main`, a build tag, or a flag.

[`examples/workloads`](../examples/workloads) is the runnable example of the whole surface: one binary, two workloads, and one unowned plugin. The exclusive workload needs a process of its own; the co-resident one may share. The unowned plugin belongs to no workload, so every role carries it — which is what makes the difference between roles visible rather than merely stated:

```bash
# Co-resident role: serves ingest and heartbeat; the transcode route is 404.
go run ./examples/workloads --config examples/workloads/application.yml

# Exclusive role: the same binary, the same config file, a different role.
XBC_WORKLOADS_TRANSCODE_ENABLED=true XBC_WORKLOADS_INGEST_ENABLED=false \
  go run ./examples/workloads --config examples/workloads/application.yml

# Standby: carries neither workload, ready to be handed one.
XBC_WORKLOADS_TRANSCODE_ENABLED=false XBC_WORKLOADS_INGEST_ENABLED=false \
  go run ./examples/workloads --config examples/workloads/application.yml
```

`doctor` prints the resolved decision instead of running the process it describes — the placement source, each declared workload with whether this process carries it, and the Definitions belonging to none of them. Resolving is not entirely passive for a lease-backed source: it claims its slots to answer and gives them back before the command returns, so nothing is held past the report. Its `workload` rows are the quickest way to answer "what is this process actually for":

```
workload ingest     hosted  replicas=4  plugins=1  max_goroutines=8
workload transcode  not held  exclusive  replicas=2  plugins=0
unowned             plugins=11
```

Two properties are worth knowing before reaching for this:

- A workload this process does not host is not a disabled plugin. Its Definitions never enter the plan, so no instance is constructed, no connection pool is opened, no queue handler is registered, and no route exists. A request for one of its routes is answered `404` and is never forwarded to a process that does carry it.
- Which roles exist is a cluster decision, so `replicas` and exclusivity are declared in Go beside the Definitions, not in YAML. `StaticPlacement`, which this example uses, ignores `replicas`: the `replicas=` figures in the rows above are declarations, and only a source that hands out slots — the lease source — turns them into a limit on how many processes may carry the workload. Only what one process may decide about itself — `enabled` and `max_goroutines` — lives under the `workloads` root, one section per declared workload. A deployment that declares no workload, like the quickstart itself, must not have a `workloads:` block at all: the root is declared only when a workload is, so an empty one fails startup as an unowned key.

The operator arithmetic this guide deliberately leaves out — how many processes to start for a given replica count, how a standby takes over after a holder dies, how long a rollout grace period has to be, and what placement does not do — is in [Slots, standbys, and how many processes to start](recipes.md#slots-standbys-and-how-many-processes-to-start) and the sections after it.

## Next steps

- A service with no transport at all follows the same shape: see [Background-only service](recipes.md#background-only-service) and the runnable `examples/worker`.
- One binary hosting several roles is the runnable `examples/workloads`, with the operator side in [Hosting a subset of workloads](recipes.md#hosting-a-subset-of-workloads).
- [Deployment recipes](recipes.md) show explicit compositions for authentication, persistence, messaging, scheduling, and multi-replica services, including terminating TLS in the process or behind a proxy, moving metrics and pprof to a listener of their own, and how the runnable [`examples/production`](../examples/production) image is built and run from environment variables alone.
- [Package `web`](https://pkg.go.dev/github.com/xbcio/xbc/transport/web) documents routing, middleware, request binding, errors, limits, and trusted proxies.
- [Package `config`](https://pkg.go.dev/github.com/xbcio/xbc/config) documents the programmatic loader and schema contract.
