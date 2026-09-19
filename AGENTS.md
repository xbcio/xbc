# Repository Guidelines

## Project Direction
XBC is an actively developed Go framework for composing applications from plugins and driving their lifecycle. Optimize for a small and understandable public surface, explicit dependencies, clear ownership, predictable lifecycle behavior, and protocol-neutral reusable abstractions.

The architecture, module layout, and public APIs are still evolving. Prefer long-term clarity and coherent boundaries over compatibility with an interim design.

## Product Goal
Build XBC into a plugin-assembled Go service framework that lets an application select explicit Bundles and obtain a production-ready Web service, gRPC service, background service, or a deliberate combination of them with minimal bootstrap code. The same plugin model must cover application components, transports, infrastructure integrations, lifecycle, configuration, security, health, and observability without turning Core into a Web- or gRPC-specific framework.

“Plugin” means a statically linked, compile-time Go component described by an immutable Definition and selected at the application composition root. Runtime installation of binaries, hot plug/unplug, package scanning, and hidden discovery are not product goals.

The product is successful when:
- A new service can start from a small composition root plus environment-driven configuration and receive safe operational defaults, strict validation, diagnostics, health/readiness, observability hooks, and deterministic shutdown.
- Web and gRPC own their native route/service registration and middleware/interceptor models while sharing only proven protocol-neutral capabilities.
- Plugins remain independently selectable, explicitly dependent, testable, and lifecycle-owned; optional transports and heavy integrations do not enter the Core dependency closure.
- Published modules work outside this repository without `go.work`, local replacements, placeholder versions, or unpublished sibling assumptions.
- Microservice capabilities are added from concrete deployment requirements after the single-service Web and gRPC paths are reliable, rather than through speculative common SPIs.

Local design artifacts stay outside version control: specifications and architecture designs belong in `.claude/specs/`, implementation plans and task breakdowns in `.claude/plans/`, and per-change SDD work products in `.claude/sdd/`. `CLAUDE.md` is a symlink to `AGENTS.md`; edit the latter. Committed source, module manifests, tests, and user documentation remain the shared source of truth.

## Where Things Live
The root module `github.com/xbcio/xbc` holds the facade and framework core: `xbc.go` is the recommended entry point; `plugin/` is the public plugin framework and lifecycle SPI (`assembly/` plans and binds, `model/` is the type-erased layer, `ordering/` is shared dependency ordering); `runtime/` owns execution and process lifecycle; `config/` and `log/` are facades. Transports and optional capabilities are separate publishable modules under `transport/web/` and `extensions/`; runnable consumers live in `examples/`. `tests/architecture/` and `tests/integration/` are the design guards. The full directory and module map, including why each module is separate, is in `README.md` under "Directories and boundaries".

## Decision Rules
Define a change by the outcome it must achieve and the behavior it must not violate, not by the current implementation. Prescribe a specific implementation only when that implementation is itself a deliberate project constraint.

Do:
- Inspect the affected code, tests, and callers before choosing a design.
- Prefer the simplest coherent solution that addresses the concrete requirement.
- Make intentional architecture or API changes explicit and update all affected modules, tests, documentation, configuration, and examples in the same change.
- Allow breaking changes when they materially simplify the design or improve its boundaries.

Do not:
- Treat the current module, package, directory, or API layout as a permanent requirement.
- Add speculative abstractions, fallback paths, compatibility shims, or dependencies without a concrete need.
- Bypass or weaken tests and architecture guards merely to make a change pass.
- Mix unrelated refactors into the requested change.

## Architectural Working Agreements
Treat `go.work`, module manifests, source code, and tests as the source of truth for the current architecture. Before extending an existing pattern, check that it still serves the project direction.

Keep dependency graphs acyclic and ownership explicit. Keep reusable core concepts independent of a particular transport or framework unless an intentional architecture change revises that boundary. Hide runtime assembly details behind focused contracts rather than leaking them into plugin APIs.

Architecture and public-API guards live in `tests/architecture/` and `tests/integration/`. They represent the currently intended design. When changing that design deliberately, update the guards and affected callers in the same change; do not merely bypass or delete a failing guard.

## Build & Validation
Run commands from the repository root:

```sh
make fmt        # format all Go files with gofmt
make check      # format check, go vet, and tests for every workspace module
make test       # uncached tests for every workspace module
make test-race  # uncached race-enabled tests for every workspace module
go run ./examples/quickstart --config examples/quickstart/application.yml
```

Do not use root-level `go test ./...` as full-repository validation: it excludes nested modules. The Makefile uses `scripts/for-each-module` to cover every module listed in `go.work`.

While iterating, test only the affected package. From the root, `go test ./plugin/...` covers core packages; a submodule is tested from its own directory, e.g. `cd transport/web && go test ./...`. Run the workspace-wide `make fmt`, `make check`, and `make test-race` once when the change is complete.

`go run ./examples/quickstart doctor --config examples/quickstart/application.yml` validates and prints the full plan -- configuration, plugin activation, typed inputs, contracts, and start order -- without opening a listener.

After changing Go code, run `make fmt` followed by `make check`. Also run `make test-race` for concurrent runtime, lifecycle, plugin, or server changes. Targeted package tests are useful during development but do not replace the final workspace-wide check. Documentation-only changes do not require tests unless they alter commands, configuration, or runnable examples.

## Change Quality
Use `gofmt`, standard Go naming, and the style of the surrounding package. Prefer cohesive, purpose-specific packages; do not introduce catch-all `common`, `utils`, or `pkg` directories. Place focused tests beside the package and cover behavior, error paths, lifecycle cleanup, and concurrency invariants. Follow the local assertion style instead of mixing conventions.

Keep third-party dependencies scoped to the module that owns the feature, and inspect manifest changes for unrelated churn. Unless intentionally redesigning the workspace or release strategy, do not add local `replace` directives or placeholder unpublished inter-module versions to publishable module manifests; local resolution belongs in `go.work`.

Public APIs and configuration may change during this development phase, but a change must not leave stale callers or contradictory documentation behind. Update affected defaults, decoding and validation tests, architecture guards, documentation, and `examples/quickstart/application.yml` together when applicable.

## Git Workflow
When commits are requested, use a single English subject in the form `type(scope): imperative summary`, following Conventional Commits with the concise, concrete style used by Go and Git. Name the primary behavior or outcome rather than the work process; avoid task or review identifiers, coverage figures, validation summaries, and exhaustive change lists. Aim for about 50 characters when practical, never exceed 72 characters, and omit a trailing period. Omit the commit body unless the user explicitly requests one. Do not create commits or push changes unless explicitly requested, and do not overwrite unrelated worktree changes.
