# xbc

xbc is a transport-protocol-agnostic Go plugin application runtime. A Definition is one runtime unit, with its Key and applicable configuration, contracts, dependencies, factory, and lifecycle; a Bundle is an explicit, side-effect-free static composition of zero or more Definitions. A package's Definition() accessor, when present, identifies its primary unit, while its Bundle() may select additional independent units or purely aggregate other Bundles. Core is responsible only for explicit composition, strict configuration binding, dependency planning, resource construction, lifecycle, managed tasks, and reverse-order shutdown; transport stacks and optional extensions are isolated by owner and dependency weight, and do not enter core's dependency closure.

The repository is still ahead of its first stable tag. The root `go.work` currently links 26 modules: 25 product modules (core, examples, `transport/web`, 12 protocol-neutral extensions, and 10 Web extension or adapter modules) and 1 tooling module (`scripts/plugin-snapshots`). Architecture tests guarantee that every `go.mod` in the repository is covered by the workspace, the Makefile, and CI. Publishable submodules must not use local `replace`, `v0.0.0`, or pseudo-versions; a real release must use real tags in dependency-topology order.

## Directories and boundaries

```text
xbc/
├── config/                              # Layered configuration facade
├── docs/                                # User guides and deployment recipes
├── examples/                            # Independent module with runnable consumer examples
│   └── quickstart/                      # Minimal explicit application composition
├── authentication/                      # Protocol-neutral authentication contracts
├── extensions/                          # Optional protocol-neutral capabilities
│   ├── authorization/rbac/              # RBAC business plugin module
│   ├── coordination/raft/               # Coordination extension module
│   ├── storage/{elasticsearch,gorm,
│   │   objectstorage,redis}/             # Storage extension modules
│   ├── jobs/{asynq,cron}/                # Background-job extension modules
│   └── messaging/{kafka,outbox,webhook}/ # Messaging extension modules
├── log/                                 # Logging facade; independent of config
├── plugin/                              # Public plugin framework and lifecycle SPI
│   ├── assembly/                        # Planning, binding, validation, graph, and construction
│   ├── autoload/                        # Optional process-wide Bundle collector
│   ├── model/                           # Low-level type-erased framework representations
│   └── ordering/                        # Stable dependency ordering shared by core and transports
├── runtime/                             # Low-level application execution and process lifecycle
├── scripts/                             # Repository validation and migration tooling
│   ├── plugin-migration-inventory/
│   └── plugin-snapshots/                # Independent tooling module
├── tests/
│   ├── architecture/                    # Dependency, API, side-effect, and module guards
│   └── integration/                     # End-to-end public-contract tests
├── transport/
│   └── web/                             # Independent Gin-backed Web runtime module
│       ├── autoload/                    # Optional Web runtime autoload adapter
│       ├── extensions/                  # All Web plugins, grouped by capability
│       │   ├── authentication/{apikey,jwt,session}/
│       │   ├── authorization/{casbin,casbin-gorm,casbin-redis,rbac,tenant}/
│       │   ├── openapi/swag/
│       │   ├── observability/{accesslog,auditlog,metrics,pprof,requestid,tracing}/
│       │   ├── response/{biz,gzip}/
│       │   ├── reliability/{gracefulshutdown,health,idempotency,
│       │   │   ratelimit,recovery,timeout}/
│       │   └── security/{cors,securityheaders}/
│       └── prelude/                     # Side-effect-free production baseline Bundle
├── go.mod                               # Core module manifest
├── go.work                              # Workspace linking all repository modules
├── Makefile                             # Workspace-wide formatting and validation entry points
└── xbc.go                               # Thin application facade and recommended entry point
```

This layout follows four rules: the root-level `runtime` owns low-level process execution and private command parsing; the Plugin model, assembly, and optional collection mechanism stay under `plugin`; protocol-neutral authentication contracts live in `authentication`, while selectable RBAC policy belongs to the authorization extension group; and optional capabilities are grouped by purpose beneath `extensions` rather than collected under the narrower name `integrations`. Group directories are namespaces rather than packages. Protocol-neutral extension leaves are independently versioned modules. Under Web, lightweight leaves remain packages of `transport/web`, while dependency-heavy leaves and the RBAC adapter are independent modules in the same capability hierarchy. The Web RBAC leaf is deliberately a thin transport adapter rather than a plugin; the protocol-neutral RBAC leaf owns the Definition and lifecycle.

The root `xbc` package therefore stays a stable, thin facade. All Web plugins have one discoverable home under `transport/web/extensions`; lightweight plugins are released with the Web runtime while optional dependency-heavy modules remain isolated. Do not add vague `common`, `utils`, or `pkg` packages, and do not create empty directories or placeholder APIs for unimplemented capabilities. A new `transport/<stack>` should be created only once a new protocol runtime stack is genuinely implemented; gRPC is explicitly not implemented in this round. The full constraints are defined jointly by `AGENTS.md` and the guards in `tests/architecture/`.

## Quickstart

Requires Go 1.25 or later. Run from the repository root:

```bash
go run ./examples/quickstart doctor --config examples/quickstart/application.yml
go run ./examples/quickstart --config examples/quickstart/application.yml
```

See [Quickstart](docs/quickstart.md) for the full composition walkthrough, configuration entry points, and verification requests. The Web runtime contract is documented at [Web package documentation](https://pkg.go.dev/github.com/xbcio/xbc/transport/web), and authentication, persistence, messaging, and multi-replica deployment examples are in [Deployment recipes](docs/recipes.md).

## Development and validation

```bash
make fmt        # format all Go files across the repository
make check      # gofmt check + go vet/go test for every workspace module
make test-race  # race-enabled tests for every workspace module
```

Do not use the root-level `go test ./...` as a substitute for full-repository validation: Go's recursive package pattern does not enter nested modules. The Makefile and CI discover modules dynamically from `go.work`, and the architecture tests also scan the disk, so any omission will fail.
