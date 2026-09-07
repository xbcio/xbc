# xbc

xbc is a transport-protocol-agnostic Go plugin application runtime. Core is responsible only for explicit composition, strict configuration binding, dependency planning, resource construction, lifecycle, managed tasks, and reverse-order shutdown; the Web runtime stack and external system integrations are isolated by actual owner and dependency weight, and do not enter core's dependency closure.

The repository is still ahead of its first stable tag. The root `go.work` currently links 23 modules: 22 product modules (core, examples, `transport/web`, 9 Web integrations, 10 protocol-neutral integrations) and 1 tooling module (`scripts/plugin-snapshots`). Architecture tests guarantee that every `go.mod` in the repository is covered by the workspace, the Makefile, and CI. Publishable submodules must not use local `replace`, `v0.0.0`, or pseudo-versions; a real release must use real tags in dependency-topology order.

## Directories and boundaries

```text
xbc/
├── config/                              # Layered configuration facade
├── docs/                                # User guides and deployment recipes
├── examples/                            # Independent module with runnable consumer examples
│   └── quickstart/                      # Minimal explicit application composition
├── integrations/                        # Protocol-neutral external-system integrations
│   └── {asynq,cron,elasticsearch,gorm,kafka,objectstorage,
│       outbox,raft,redis,webhook}/       # One independently versioned module per integration
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
├── security/                            # Protocol-neutral authentication contracts and policy
│   └── rbac/                            # Independent RBAC business plugin
│       └── autoload/                    # Optional leaf autoload adapter
├── tests/
│   ├── architecture/                    # Dependency, API, side-effect, and module guards
│   └── integration/                     # End-to-end public-contract tests
├── transport/
│   └── web/                             # Independent Gin-backed Web runtime module
│       ├── autoload/                    # Optional Web runtime autoload adapter
│       ├── {accesslog,apikey,auditlog,biz,cors,gracefulshutdown,
│       │   gzip,health,pprof,ratelimit,recovery,requestid,
│       │   securityheaders,tenant,timeout}/ # Built-ins released with the Web runtime
│       ├── integrations/                # Third-party or heavy Web integrations
│       │   └── {casbin,casbin-gorm,casbin-redis,idempotency,jwt,
│       │       metrics,session,swagger,tracing}/ # One independent module each
│       ├── prelude/                     # Side-effect-free production baseline Bundle
│       └── rbac/                        # Thin RequireAll/RequireAny middleware adapter
├── go.mod                               # Core module manifest
├── go.work                              # Workspace linking all repository modules
├── Makefile                             # Workspace-wide formatting and validation entry points
└── xbc.go                               # Thin application facade and recommended entry point
```

This layout follows three rules common to mature Go projects: the root-level `runtime` uniformly carries low-level process execution and its private command parsing, and the Plugin model, assembly, and optional collection mechanism belong to the same `plugin` owner; packages are organized by owner rather than by technical label; a Go module is split out only when independent versioning, dependency isolation, or release cadence is actually needed. The root `xbc` package therefore stays a stable, thin facade, Web built-in capabilities are released together with the Web runtime stack, and only heavy integrations bear the maintenance cost of an independent module. Do not add vague `common`, `utils`, or `pkg` packages, and do not create empty directories or placeholder APIs for unimplemented capabilities. A new `transport/<stack>` should be created only once a new protocol runtime stack is genuinely implemented; gRPC is explicitly not implemented in this round. The full constraints are defined jointly by `AGENTS.md` and the guards in `tests/architecture/`.

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
