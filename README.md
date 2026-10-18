# xbc

xbc is a transport-protocol-agnostic Go plugin application runtime. A Definition is one runtime unit, with its Key and applicable configuration, contracts, dependencies, factory, and lifecycle; a Bundle is an explicit, side-effect-free static composition of zero or more Definitions. A package's Definition() accessor, when present, identifies its primary unit, while its Bundle() may select additional independent units or purely aggregate other Bundles. Core is responsible only for explicit composition, strict configuration binding, dependency planning, resource construction, lifecycle, managed tasks, and reverse-order shutdown; transport stacks and optional extensions are isolated by owner and dependency weight, and do not enter core's dependency closure.

Releases travel as tag waves, and the first one is out: every module was tagged `v0.1.0` in dependency-topology order, recorded wave by wave in the checked-in `tests/architecture/testdata/release-manifest.json`; a stable tag is still ahead. The root `go.work` links 50 modules: 49 product modules (core, examples, `transport/web`, `transport/grpc`, 29 protocol-neutral extensions, 11 Web engine, extension, or adapter modules, 4 gRPC extension modules, and the `tests/composition` end-to-end module) and 1 tooling module (`scripts/plugin-snapshots`). Architecture tests guarantee that every `go.mod` in the repository is covered by the workspace, the Makefile, and CI. Publishable submodules must not use local `replace`, `v0.0.0`, or pseudo-versions; their XBC requirements name released tags.

## Directories and boundaries

```text
xbc/
├── config/                              # Layered configuration facade
├── docs/                                # User guides and deployment recipes
├── examples/                            # Independent module with runnable consumer examples
│   ├── quickstart/                      # Minimal explicit application composition
│   ├── grpcquickstart/                  # Minimal gRPC service: buf-generated API, ENV-only startup
│   ├── worker/                          # Background-only service; no transport selected
│   └── workloads/                       # One binary, several roles: workload placement
├── extensions/                          # Optional protocol-neutral capabilities
│   ├── authentication/                  # Protocol-neutral authentication contracts; zero-dependency module
│   │   ├── apikey/                      # API-key authenticator and digest repository; nested module, Web half reads the headers
│   │   ├── federation/                  # Protocol-neutral external-identity contract; package of this module, no plugin
│   │   ├── jwt/                         # HMAC JWT authenticator and signer; nested module, Web half extracts the token
│   │   ├── oauth2/                      # OAuth 2.0 authorization-code client: PKCE, token exchange, userinfo mapping; nested module, no plugin
│   │   ├── oidc/                        # OpenID Connect authorization-code client: discovery, JWKS, ID token verification; nested module, no plugin
│   │   └── session/                     # Opaque server-side sessions, stores, and Manager; nested module, Web half owns the cookie
│   ├── authorization/{casbin,casbin-gorm,
│   │   casbin-redis,rbac}/              # Casbin engine, its adapter and watcher providers, and RBAC
│   ├── concurrency/async/               # Drained background task pool plugin; process-global Spawner
│   ├── coordination/                    # Coordination capabilities
│   │   ├── lease/                       # Lease contract module; zero-dependency vocabulary
│   │   ├── placement/                   # Lease-backed plugin.PlacementSource
│   │   └── raft/                        # Raft coordination extension module
│   ├── httpclient/                      # One configured outbound HTTP client per downstream; timeout, retry, TLS, propagation
│   ├── tasks/                           # Protocol-neutral task facade; local via async, remote via asynq
│   ├── reliability/{gracefulshutdown,health}/ # Protocol-neutral reliability capability modules
│   ├── storage/{elasticsearch,gorm,
│   │   objectstorage,redis}/             # Storage extension modules
│   ├── jobs/{asynq,cron}/                # Background-job extension modules
│   ├── messaging/{kafka,outbox,webhook}/ # Messaging extension modules
│   └── observability/{metrics,tracing}/ # Protocol-neutral metrics registry and OpenTelemetry pipeline modules
├── log/                                 # Logging facade; independent of config
├── plugin/                              # Public plugin framework and lifecycle SPI
│   ├── assembly/                        # Planning, binding, validation, graph, and construction
│   ├── autoload/                        # Optional process-wide Bundle collector
│   ├── model/                           # Low-level type-erased framework representations
│   └── ordering/                        # Stable string-keyed ordering; used by the Web transport
├── runtime/                             # Low-level application execution and process lifecycle
├── scripts/                             # Repository validation and migration tooling
│   ├── plugin-migration-inventory/
│   ├── plugin-snapshots/                # Independent tooling module
│   └── release-manifest/                # Release-train tag order generator and guard
├── tests/
│   ├── architecture/                    # Dependency, API, side-effect, and module guards
│   ├── composition/                     # Real-plugin end-to-end composition tests
│   └── integration/                     # End-to-end public-contract tests
├── transport/
│   ├── grpc/                            # Independent gRPC server runtime module; never imports the Web stack
│   │   ├── extensions/                  # All gRPC plugins, grouped by capability
│   │   │   ├── authentication/{apikey,jwt}/ # Metadata credential extractors; jwt is its own module
│   │   │   ├── authorization/casbin/    # Casbin policy adapters; own module
│   │   │   ├── management/              # Operator listener: /metrics, opt-in pprof; package of the gRPC module
│   │   │   ├── observability/{accesslog,
│   │   │   │   metrics,tracing}/        # Access log; metrics and tracing are modules of their own
│   │   │   └── reliability/health/      # grpc.health.v1 probe service; package of the gRPC module
│   │   ├── prelude/                     # Side-effect-free gRPC baseline: composes, never activates
│   │   └── starter/                     # Product gRPC baseline: selects those Bundles, adds the layer that enables them
│   └── web/                             # Independent engine-neutral Web runtime module
│       ├── autoload/                    # Optional Web runtime autoload adapter
│       ├── engines/gin/                 # Gin engine adapter module; the only module that imports Gin
│       ├── extensions/                  # All Web plugins, grouped by capability
│       │   ├── authentication/{apikey,federation,jwt,oidc,session}/
│       │   ├── authorization/{casbin,rbac,tenant}/
│       │   ├── openapi/swag/
│       │   ├── observability/{accesslog,auditlog,metrics,pprof,requestid,tracing}/
│       │   ├── response/{biz,gzip}/
│       │   ├── reliability/{health,idempotency,
│       │   │   ratelimit,timeout}/
│       │   └── security/{cors,securityheaders}/
│       ├── prelude/                     # Side-effect-free Web baseline: composes, never activates
│       └── starter/                     # Product Web baseline: selects those Bundles, adds the layer that enables them
├── go.mod                               # Core module manifest
├── go.work                              # Workspace linking all repository modules
├── Makefile                             # Workspace-wide formatting and validation entry points
└── xbc.go                               # Thin application facade and recommended entry point
```

This layout follows four rules: the root-level `runtime` owns low-level process execution and private command parsing; the Plugin model, assembly, and optional collection mechanism stay under `plugin`; protocol-neutral authentication contracts live in `extensions/authentication`, while selectable RBAC policy belongs to the authorization extension group; and optional capabilities are grouped by purpose beneath `extensions` rather than collected under the narrower name `integrations`. Group directories are namespaces rather than packages. Protocol-neutral extension leaves are independently versioned modules. Under Web, lightweight leaves remain packages of `transport/web`, while dependency-heavy leaves, the engine adapter, and the RBAC adapter are independent modules in the same capability hierarchy. The Web RBAC leaf is deliberately a thin transport adapter rather than a plugin; the protocol-neutral RBAC leaf owns the Definition and lifecycle. Authenticators split the same way: `extensions/authentication/jwt` verifies and signs tokens, `extensions/authentication/apikey` checks keys against a digest repository, and `extensions/authentication/session` owns session storage and lifecycle, each exporting the neutral `Authenticator`, while their Web halves under `transport/web/extensions/authentication` only move the credential between the request and the authenticator (session's Web half also installs and clears the cookie), and each Web Bundle includes its neutral half. Tracing splits likewise: `extensions/observability/tracing` owns the private TracerProvider, the OTLP exporter, the propagator, and the `Handle` contract, while `transport/web/extensions/observability/tracing` is only the HTTP server middleware that records into it. Metrics splits the same way: `extensions/observability/metrics` owns the private registry and the `Registerer` contract plugins publish their collectors through, while `transport/web/extensions/observability/metrics` is only the HTTP server instruments and the scrape route that serves it. Health follows the same split for the same reason: `extensions/reliability/health` owns the Contributor contract, the aggregation and the probe policy so a dependency plugin can contribute a check without inheriting a transport's dependency closure, while `transport/web/extensions/reliability/health` only serves the HTTP endpoints.

One process hosts one App, and the three owners of that constraint are named where each one lives: the log facade's global logger (`log`), the selected engine adapter's process-global diagnostics (`transport/web`), and the `xbc.runtime` processor and memory knobs (`runtime`). Everything else -- plugin values, lifecycle contexts, servers -- is per-App.

The root `xbc` package therefore stays a stable, thin facade. All Web plugins have one discoverable home under `transport/web/extensions`, and all gRPC plugins under `transport/grpc/extensions`; lightweight plugins are released with their runtime module while optional dependency-heavy modules remain isolated. One plugin is deliberately the other shape: the gRPC management plane (`transport/grpc/extensions/management`) is a package of the transport module, because it is a transport's own operator surface -- the gRPC counterpart of the Web management listener -- and it takes the Prometheus client into the transport's closure the same way `transport/web` takes the validator. Recorders stay modules; the port that renders a registry is part of the transport that owns it. Each transport also publishes its own baseline the same way: a side-effect-free prelude Bundle beside its extensions, and a starter that selects the prelude's Bundles and adds the lowest configuration layer switching them on -- the Web pair lives under `transport/web`, the gRPC pair under `transport/grpc`, and neither starter selects telemetry, which stays with the independently versioned metrics and tracing modules a composition root selects explicitly. Do not add vague `common`, `utils`, or `pkg` packages, and do not create empty directories or placeholder APIs for unimplemented capabilities. A new `transport/<stack>` should be created only once a new protocol runtime stack is genuinely implemented. `transport/grpc` is such a sibling stack: it owns gRPC's native service registration and interceptor chains and shares only protocol-neutral capabilities with `transport/web`, never the Web module itself. The full constraints are defined jointly by `AGENTS.md` and the guards in `tests/architecture/`.

## Quickstart

Requires Go 1.25 or later. Run from the repository root:

```bash
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
go run ./examples/quickstart validate --config examples/quickstart/application.yml
go run ./examples/quickstart --config examples/quickstart/application.yml
```

An application that wants a documented baseline rather than a hand-picked Bundle list passes `xbc.WithStarter(starter.Web())` and still selects exactly one engine Bundle beside it; the starter contributes the baseline's Bundles and the lowest configuration layer that switches the capabilities among them on, and a file, an overlay, or an environment variable can override any of it. The two diagnostics bracket each other: `doctor` reports the plan without constructing anything, while `validate` constructs the selected plugins and checks the state a boot would build -- the route table and the authentication policy each route resolves to among it -- before binding a listener or serving. See [Quickstart](docs/quickstart.md) for the full composition walkthrough, configuration entry points, and verification requests. The gRPC counterpart passes `xbc.WithStarter(grpcstarter.GRPC())` beside its buf-generated service and runs as `go run ./examples/grpcquickstart` from environment variables alone -- no configuration file is needed -- with the full walkthrough under [gRPC service](docs/recipes.md#grpc-service). A service with no transport selected follows the same shape and is runnable as `go run ./examples/worker --config examples/worker/application.yml`; see [Background-only service](docs/recipes.md#background-only-service). The production-style counterpart -- two interchangeable authentication schemes, Casbin authorization, a transactional outbox beside a real database, a scrape endpoint, and the container image that runs the same binary from environment variables alone -- is `examples/production`; see [Building and running the production image](docs/recipes.md#building-and-running-the-production-image). The Web runtime contract is documented at [Web package documentation](https://pkg.go.dev/github.com/xbcio/xbc/transport/web), and authentication, persistence, messaging, and multi-replica deployment examples are in [Deployment recipes](docs/recipes.md).

## Plugin configuration

Two rules matter when authoring a plugin's `Config` struct. A `default` tag inside a pointer sub-struct allocates that sub-struct even when the user configured nothing, so a `required` field inside it would fail for every user who left the optional block out -- validate such a block conditionally in `Prepare` instead. And a `default` tag never overwrites a value `Defaults` pre-filled; it supplies a value only where the field is still at its zero value. See [Configuration](docs/quickstart.md#files-and-profiles) for the full precedence rules.

## Development and validation

```bash
make fmt        # format all Go files across the repository
make check      # gofmt check + go vet/go test for every workspace module
make test-race  # race-enabled tests for every workspace module
```

Do not use the root-level `go test ./...` as a substitute for full-repository validation: Go's recursive package pattern does not enter nested modules. The Makefile and CI discover modules dynamically from `go.work`, and the architecture tests also scan the disk, so any omission will fail.
