# Repository Guidelines

## Project Direction
XBC is an actively developed Go framework for composing applications from plugins and driving their lifecycle. Optimize for a small and understandable public surface, explicit dependencies, clear ownership, predictable lifecycle behavior, and protocol-neutral reusable abstractions.

The architecture, module layout, and public APIs are still evolving. Prefer long-term clarity and coherent boundaries over compatibility with an interim design.

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

After changing Go code, run `make fmt` followed by `make check`. Also run `make test-race` for concurrent runtime, lifecycle, plugin, or server changes. Targeted package tests are useful during development but do not replace the final workspace-wide check. Documentation-only changes do not require tests unless they alter commands, configuration, or runnable examples.

## Change Quality
Use `gofmt`, standard Go naming, and the style of the surrounding package. Prefer cohesive, purpose-specific packages; do not introduce catch-all `common`, `utils`, or `pkg` directories. Place focused tests beside the package and cover behavior, error paths, lifecycle cleanup, and concurrency invariants. Follow the local assertion style instead of mixing conventions.

Keep third-party dependencies scoped to the module that owns the feature, and inspect manifest changes for unrelated churn. Unless intentionally redesigning the workspace or release strategy, do not add local `replace` directives or placeholder unpublished inter-module versions to publishable module manifests; local resolution belongs in `go.work`.

Public APIs and configuration may change during this development phase, but a change must not leave stale callers or contradictory documentation behind. Update affected defaults, decoding and validation tests, architecture guards, documentation, and `examples/quickstart/application.yml` together when applicable.

## Git Workflow
When commits are requested, use a single English subject in the form `type(scope): imperative summary`, following Conventional Commits with the concise, concrete style used by Go and Git. Name the primary behavior or outcome rather than the work process; avoid task or review identifiers, coverage figures, validation summaries, and exhaustive change lists. Aim for about 50 characters when practical, never exceed 72 characters, and omit a trailing period. Omit the commit body unless the user explicitly requests one. Do not create commits or push changes unless explicitly requested, and do not overwrite unrelated worktree changes.
