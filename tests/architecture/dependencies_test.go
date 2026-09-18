package architecture_test

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archPackageJSON mirrors the subset of `go list -json` used by the
// direction/reverse-dependency guards: each package's own *direct*
// import lists (Imports/TestImports/XTestImports -- what its source files
// literally wrote), never Deps (the full transitive closure).
//
// Per package-layout design §9.1, the evidence must match the question:
//
//   - A closure (Deps) judgment is required for "must never end up
//     compiling this in" questions: a package can pick up a forbidden
//     dependency through an intermediate package, and only the closure sees
//     that.
//   - A direct-import judgment is required for "must never itself reach
//     past its declared dependencies" questions (for example web/examples
//     reaching into core internal). Deps cannot distinguish an import the
//     package wrote from one inherited through a legitimate owner package.
//
// archGoList serves the latter rules. archDeps below serves transitive-purity
// rules; go.mod, AST and filesystem guards use their own stronger evidence.
type archPackageJSON struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
	// DepOnly is set by `go list -deps -json`. It is true for a package that
	// entered the listing only as a dependency of the packages named on the
	// command line, and false for a package the pattern actually matched.
	//
	// archModuleClosure reads it to tell a module's own packages apart from
	// the closure those packages pull in. That distinction is what lets the
	// per-module guards below ask "what does this module compile in" without
	// also needing the module's path as a second, separately-sourced fact.
	DepOnly bool
}

// archGoCommand runs every Go command from the repository root. Test binaries
// normally start in their package directory, and callers may choose any other
// cwd, so relative package patterns must never inherit process cwd.
func archGoCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	return archGoCommandIn(t, archRepositoryRoot(t), args...)
}

// archGoCommandIn runs a Go command from a specific module directory, so a
// guard can ask a question about a module other than the root one.
//
// This exists because every other helper in this file is rooted at the
// repository root, and `./...` from there resolves only within the root
// module. A guard that asked "does this module's closure pull in the Web
// stack" from the root would therefore be asking it about the wrong module
// and, for a module added later, about nothing at all -- it would pass
// without ever having read the module it claims to protect. Callers pass the
// module's own directory so `./...` means that module.
func archGoCommandIn(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	return cmd
}

// archGoList runs `go list -json` over pattern and decodes every package
// object it prints -- one JSON object per matched package, not a single
// JSON array, hence the streaming decoder rather than a single Unmarshal.
//
// It calls t.Fatal, not t.Skip, when go list matched zero packages. A
// silently-empty match (wrong working directory, a build that no longer
// compiles, a typo'd pattern) must not read as "the guard passed" -- it read
// nothing, so it protected nothing, and a green result here would be worse
// than no test at all.
func archGoList(t *testing.T, pattern string) []archPackageJSON {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command unavailable, skipping dependency direction check")
	}

	out, err := archGoCommand(t, "list", "-json", pattern).Output()
	require.NoError(t, err, "go list -json %s failed", pattern)

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []archPackageJSON
	for dec.More() {
		var pkg archPackageJSON
		require.NoError(t, dec.Decode(&pkg), "Parsing go list -json %s output failed", pattern)
		pkgs = append(pkgs, pkg)
	}
	if len(pkgs) == 0 {
		t.Fatalf("go list -json %s returned no packages, dependency direction check actually did not take effect", pattern)
	}
	return pkgs
}

// archDirectImports concatenates a package's own three direct-import lists:
// production sources, in-package (_test.go) sources, and the external test
// package's sources. A forbidden import written in any of the three is
// exactly as real a violation as one written in the production file --
// tests ship in the same repository and a bad import there is still a bad
// import, so none of the three is skipped.
func archDirectImports(pkg archPackageJSON) []string {
	all := make([]string, 0, len(pkg.Imports)+len(pkg.TestImports)+len(pkg.XTestImports))
	all = append(all, pkg.Imports...)
	all = append(all, pkg.TestImports...)
	all = append(all, pkg.XTestImports...)
	return all
}

// archPathAtOrBelow reports whether importPath names prefix itself or one of
// its subpackages. Architecture boundaries are module/package-tree boundaries,
// so checking only the exact root path would let (for example) gin/binding
// bypass a rule that forbids Gin.
func archPathAtOrBelow(importPath, prefix string) bool {
	return importPath == prefix || strings.HasPrefix(importPath, prefix+"/")
}

// ── guard 1: core must never directly import Gin ──────────────────────────

// TestArchCorePackagesDoNotImportGinDirectly is the package-layout design
// §9 rule 1 guard, restricted to core's own module (`./...` from this
// repository root resolves only within the core module even under go.work --
// transport/web and examples each carry their own go.mod and arch_test.go).
//
// The core's whole reason to exist is staying protocol-agnostic: the moment
// any package under this module writes `import "github.com/gin-gonic/gin"`,
// a purely-gRPC or purely-task application depending on core would start
// pulling Gin in for no reason it asked for.
func TestArchCorePackagesDoNotImportGinDirectly(t *testing.T) {
	pkgs := archGoList(t, "./...")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if archPathAtOrBelow(dep, "github.com/gin-gonic/gin") {
				t.Errorf("%s directly imported %q: core is protocol-agnostic runtime, Gin can only exist in standalone transport/web module", pkg.ImportPath, dep)
			}
		}
	}
}

// TestArchCoreGoModDoesNotRequireOptionalStacks is the manifest-level
// companion to the source and closure checks. It catches stale requirements
// even when no source currently imports them.
//
// `go list` only reports packages that something imports. A leftover require
// has no import edge to inspect but still pollutes downloads, upgrades and
// security scans, so the module manifest needs its own guard. Parsing through
// `go mod edit -json` avoids comments and formatting creating false matches.
func TestArchCoreGoModDoesNotRequireOptionalStacks(t *testing.T) {
	mod := archReadModuleFile(t, "go.mod")
	for _, req := range mod.Require {
		for _, forbidden := range []string{
			"github.com/gin-gonic/gin",
			"google.golang.org/grpc",
			"github.com/xbcio/xbc/extensions",
			"github.com/xbcio/xbc/transport",
		} {
			if archPathAtOrBelow(req.Path, forbidden) {
				t.Errorf("core's go.mod must not require optional runtime stack or implementation module %q; these dependencies can only be owned by standalone module", req.Path)
			}
		}
	}
}

// ── guard 2: core must never directly import transport plugins ────────────

// TestArchCorePackagesDoNotImportTransportStacks is the package-layout design
// §9 rule 1 guard for every optional transport stack: transports depend on
// core, never the reverse. Guarding the parent namespace means a future
// transport/grpc module is covered as soon as it appears, without
// extending an implementation-name allowlist here.
//
// Checked across every package this module builds (`./...`), not just the root
// package, so a future core subpackage cannot reach sideways into a transport
// while the root package itself stays clean.
func TestArchCorePackagesDoNotImportTransportStacks(t *testing.T) {
	pkgs := archGoList(t, "./...")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if archPathAtOrBelow(dep, "github.com/xbcio/xbc/transport") {
				t.Errorf("%s contains direct import of %q: transport/* is a standalone module, dependency direction must be transport → core, cannot be reversed", pkg.ImportPath, dep)
			}
		}
	}
}

// ── guard 3: the root package must never directly import examples ────────

// TestArchRootPackageDoesNotImportExamples is the package-layout design §4
// guard for the examples module ("examples clearly does not take responsibility: being reverse depended by core or transport/web"):
// the root package is what an example imports, never the other way round.
//
// Scoped to pattern "." (the root package alone), matching the task's own
// framing of this rule ("root package must not import examples") -- unlike guard 2 above,
// this is not a "reachable from anywhere in core" invariant, it is
// specifically about the one package an example's main() actually imports.
func TestArchRootPackageDoesNotImportExamples(t *testing.T) {
	pkgs := archGoList(t, ".")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if dep == "github.com/xbcio/xbc/examples" || strings.HasPrefix(dep, "github.com/xbcio/xbc/examples/") {
				t.Errorf("root package contains direct import of %q: examples is an example module consuming xbc.Run(), cannot be reverse depended by core", dep)
			}
		}
	}
}

// ── lower-layer direction versus public capability owners ──────────────
//
// The root package is a narrow application-facing facade. The root runtime
// package owns lifecycle, process orchestration, and private command parsing.
// The plugin tree owns its typed facade, erased model, assembly,
// and optional autoload infrastructure; it must not pull higher-level process
// orchestration back into that subtree.

// ── guard 5: plugin must not import runtime, root facade, or internal packages ──

// TestArchPluginPackagesDoNotImportHigherLevelOwners checks direct imports from
// production and test files, then checks the production closure. The direct
// check catches a forbidden test-only edge; the closure check prevents an
// indirect bridge to runtime or another internal implementation package.
func TestArchPluginPackagesDoNotImportHigherLevelOwners(t *testing.T) {
	const (
		rootPackage  = "github.com/xbcio/xbc"
		runtimeRoot  = rootPackage + "/runtime"
		internalRoot = rootPackage + "/internal"
	)

	assertAllowed := func(owner, dep, evidence string) {
		t.Helper()
		if dep == rootPackage {
			t.Errorf("%s %s root package: plugin is the protocol-neutral SPI below the application facade", owner, evidence)
			return
		}
		if archPathAtOrBelow(dep, runtimeRoot) {
			t.Errorf("%s %s %q: plugin owns model and assembly infrastructure below the application runtime and must not reverse import it", owner, evidence, dep)
			return
		}
		if archPathAtOrBelow(dep, internalRoot) {
			t.Errorf("%s %s %q: plugin owns model and assembly infrastructure but must not import another internal implementation", owner, evidence, dep)
		}
	}

	for _, pkg := range archGoList(t, "./plugin/...") {
		for _, dep := range archDirectImports(pkg) {
			assertAllowed(pkg.ImportPath, dep, "directly imports")
		}
	}
	for _, dep := range archDeps(t, "./plugin/...") {
		assertAllowed("plugin production closure", dep, "contains")
	}
}

// ── guard 6: config/log must never import the root package ───────────────

// TestArchConfigAndLogDoNotImportRootPackage is the package-layout design
// §6 rule 3 guard ("config and log are independent, and both must not import root package").
// plugin/ordering is covered by the broader plugin subtree direction guard
// above, so it does not need a duplicate direct-import assertion here.
func TestArchConfigAndLogDoNotImportRootPackage(t *testing.T) {
	for _, pattern := range []string{"./config/...", "./log/..."} {
		pkgs := archGoList(t, pattern)
		for _, pkg := range pkgs {
			for _, dep := range archDirectImports(pkg) {
				assert.NotEqual(t, "github.com/xbcio/xbc", dep,
					"%s contains direct import of root package: these packages are within the root package's dependency closure, reverse importing root package will directly form a compile-time cycle", pkg.ImportPath)
			}
		}
	}
}

// ── the closure-judgment guards ───────────────────────────────────────────
//
// The import-direction guards above ask direct-import questions (the
// separate go.mod guard is manifest-level evidence). The three closure guards
// below ask what a package ends up compiling in *however indirectly*, so they
// must read Deps. Using direct imports for these would be toothless in exactly
// the way that matters -- `plugin/ordering` could acquire a third-party dependency
// through a package it legitimately imports, and a direct-import scan would
// never see it.

// archDeps returns the full transitive build closure of pattern's packages,
// deduplicated. Unlike archGoList this uses `go list -deps`, which prints
// the closure as a flat list of import paths rather than package objects.
//
// The closure covers production sources only (no -test flag): a test-only
// dependency on testify is not a violation of any rule here, and including
// test imports would make every "stdlib only" assertion below permanently
// red for reasons that have nothing to do with what the package ships.
func archDeps(t *testing.T, pattern string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command unavailable, skipping dependency closure check")
	}

	out, err := archGoCommand(t, "list", "-deps", pattern).Output()
	require.NoError(t, err, "go list -deps %s failed", pattern)

	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}
	if len(deps) == 0 {
		t.Fatalf("go list -deps %s returned no dependencies, closure check actually did not take effect", pattern)
	}
	return deps
}

// archIsStdlib reports whether path is a standard-library package.
//
// The test is whether the first path segment contains a dot. Every module
// path the go command can resolve starts with a hostname (github.com,
// golang.org, gopkg.in), and no standard-library path does -- this is the
// same rule the go command itself uses to decide what needs resolving.
func archIsStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// TestArchCoreDependencyClosureExcludesOptionalStacks checks the stronger
// production property: core must not compile an optional protocol stack in,
// even through an otherwise innocuous intermediate dependency. Direct-import
// guards remain useful for tests and diagnostics, but only Deps can answer
// this transitive-purity question.
func TestArchCoreDependencyClosureExcludesOptionalStacks(t *testing.T) {
	for _, dep := range archDeps(t, "./...") {
		for _, forbidden := range []string{
			"github.com/gin-gonic/gin",
			"google.golang.org/grpc",
			"github.com/xbcio/xbc/extensions",
			"github.com/xbcio/xbc/transport",
		} {
			if archPathAtOrBelow(dep, forbidden) {
				t.Errorf("core's production dependency closure contains optional runtime stack %q; protocol implementation must remain in standalone module", dep)
			}
		}
	}
}

// ── guard 11: each workspace module answers for its own closure ───────────
//
// Every guard above runs from the repository root, so all of them together
// still describe exactly one module. That was adequate while core was the only
// thing with a dependency-purity rule, and it is not adequate now: the
// repository has grown a protocol-neutral extension namespace whose whole
// point is that a capability module can be depended on without inheriting a
// transport, and `scripts/plugin-snapshots` shows how easily a module that is
// not a plugin accumulates a wide closure without anyone asking about it.
//
// The guards below therefore re-ask the closure question from inside each
// module joined by go.work, where `./...` means that module.

const (
	// archTransportNamespace is the parent namespace every transport --
	// including the Web transport and its engine adapters -- lives under.
	// Guarding the parent rather than one implementation name means a future
	// transport/grpc module is covered as soon as it appears.
	archTransportNamespace = archRootPackage + "/transport"
	// archWebModulePath is the Web transport module itself. A module is
	// treated as composing the Web stack only by importing this (or a package
	// beneath it), which is the Go declaration of that dependency.
	archWebModulePath = archRootPackage + "/transport/web"
	// archGinModulePath is the HTTP engine the Web stack is built on. It is
	// named separately from archTransportNamespace because it is third-party:
	// a module can pull Gin in through an intermediate dependency without ever
	// naming an XBC transport, which is precisely what a closure guard exists
	// to catch.
	archGinModulePath = "github.com/gin-gonic/gin"
	// archExtensionsNamespace is the protocol-neutral extension namespace: the
	// capability modules a service selects for infrastructure, messaging, and
	// jobs. Nothing here is a transport, and nothing here may drag one in.
	archExtensionsNamespace = archRootPackage + "/extensions"
)

// archWorkspaceModuleDirs returns every non-root module joined by go.work, as
// an absolute module root directory. Discovery is delegated to
// archWorkspaceModuleFiles so that this guard and the release-boundary guard
// can never disagree about which modules exist.
func archWorkspaceModuleDirs(t *testing.T) []string {
	t.Helper()
	manifests := archWorkspaceModuleFiles(t)
	dirs := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		dirs = append(dirs, filepath.Dir(manifest))
	}
	require.NotEmpty(t, dirs, "go.work lists no independent sub module, the per-module closure guards are inactive")
	return dirs
}

// archModuleClosure runs `go list -deps -json ./...` from one module's own
// directory and returns every package in that module's production build
// closure, including the module's own packages.
//
// Like archGoList it calls t.Fatal rather than t.Skip when the listing comes
// back empty. An empty listing is the failure mode this whole guard exists
// for: a `go list` that ran in the wrong directory, or against a module that
// stopped compiling, would otherwise report a clean closure because it read
// no closure at all.
func archModuleClosure(t *testing.T, dir string) []archPackageJSON {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command unavailable, skipping per-module dependency closure check")
	}

	out, err := archGoCommandIn(t, dir, "list", "-deps", "-json", "./...").Output()
	require.NoError(t, err, "go list -deps -json ./... failed in module %s", dir)

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []archPackageJSON
	for dec.More() {
		var pkg archPackageJSON
		require.NoError(t, dec.Decode(&pkg), "Parsing go list -deps -json ./... output for %s failed", dir)
		pkgs = append(pkgs, pkg)
	}
	if len(pkgs) == 0 {
		t.Fatalf("go list -deps -json ./... in module %s returned no packages, the guard built on it actually did not take effect", dir)
	}
	return pkgs
}

// archModuleComposesWebStack reports whether any of this module's own packages
// directly imports the Web transport.
//
// The signal is the import statement rather than a name on a list, because an
// import is how Go records the declaration. A manifest cannot carry it: no
// XBC module may require another before there is a real release tag, so
// `require` is empty in every manifest here and `go.work` is what resolves the
// edge. Reading the import keeps the exemption derived from the module itself
// instead of from a list this file would have to keep in step with go.work.
func archModuleComposesWebStack(pkgs []archPackageJSON) bool {
	for _, pkg := range pkgs {
		if pkg.DepOnly {
			continue
		}
		for _, dep := range pkg.Imports {
			if archPathAtOrBelow(dep, archWebModulePath) {
				return true
			}
		}
	}
	return false
}

// archModuleForeignDependencies returns the distinct import paths a module
// pulls in from outside itself, in canonical order.
//
// A module's own packages are excluded deliberately. `go list -deps ./...`
// names them alongside everything they depend on, and a module that lives
// under transport/ would otherwise be reported as violating its own rule for
// no reason other than existing. The question these guards ask is what a
// module *brings in*, and a module is not a dependency of itself.
func archModuleForeignDependencies(pkgs []archPackageJSON) []string {
	seen := make(map[string]bool, len(pkgs))
	deps := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		if !pkg.DepOnly || seen[pkg.ImportPath] {
			continue
		}
		seen[pkg.ImportPath] = true
		deps = append(deps, pkg.ImportPath)
	}
	sort.Strings(deps)
	return deps
}

// archRepositoryRelative renders dir as a slash-separated path relative to the
// repository root, for diagnostics and for the one position-derived rule
// below.
func archRepositoryRelative(t *testing.T, dir string) string {
	t.Helper()
	relative, err := filepath.Rel(archRepositoryRoot(t), dir)
	require.NoError(t, err, "locating module %s within the repository failed", dir)
	return filepath.ToSlash(relative)
}

// TestArchModulesThatDoNotComposeWebDoNotCompileItIn is the per-module
// companion to TestArchCoreDependencyClosureExcludesOptionalStacks. That guard
// answers the question for core; this one answers it for every module go.work
// joins, from that module's own directory.
//
// A module is exempt only when it declares the Web dependency by importing the
// Web module. `examples` and `scripts/plugin-snapshots` are exempt on that
// basis and only on that basis: both are composition aggregates that select
// Web Bundles deliberately, neither sits beneath transport/web, and a rule
// built on directory position alone would either flag them or need a
// hand-maintained exception list that go.work would immediately outdate.
// Everything else -- every capability module, every contract module -- must
// reach the Web stack through nothing at all.
func TestArchModulesThatDoNotComposeWebDoNotCompileItIn(t *testing.T) {
	dirs := archWorkspaceModuleDirs(t)

	var (
		composing     int
		notComposing  int
		foreignNonStd int
	)
	for _, dir := range dirs {
		relative := archRepositoryRelative(t, dir)
		closure := archModuleClosure(t, dir)
		if archModuleComposesWebStack(closure) {
			composing++
			continue
		}
		notComposing++
		for _, dep := range archModuleForeignDependencies(closure) {
			if archIsStdlib(dep) {
				continue
			}
			foreignNonStd++
			for _, forbidden := range []string{archTransportNamespace, archGinModulePath} {
				if archPathAtOrBelow(dep, forbidden) {
					t.Errorf("module %s does not import the Web transport, yet its production closure contains %q; the Web stack and its engine may only be compiled in by a module that declares that dependency",
						relative, dep)
				}
			}
		}
	}

	// Non-vacuity in both directions. A run where every module composed the
	// Web stack would never evaluate the rule; a run where none did would
	// never exercise the exemption. The dependency count is the third leg: the
	// rule above is an absence, so it is only evidence if real closures were
	// read first.
	assert.GreaterOrEqual(t, len(dirs), 20,
		"go.work should join every publishable module; a short list means the per-module closure guards cover less than they claim")
	assert.NotZero(t, notComposing,
		"every workspace module composed the Web stack, so the rule under test was never applied to anything")
	assert.NotZero(t, composing,
		"no workspace module composed the Web stack, so the exemption was never taken and cannot be distinguished from a rule that always fires")
	assert.Greater(t, foreignNonStd, 100,
		"the per-module closures contributed almost no non-standard-library dependencies, so the scan read far less than the modules it claims to cover")
}

// TestArchProtocolNeutralExtensionsNeverComposeTheWebStack states the same
// invariant for the extension namespace without the exemption above.
//
// This is not redundant with the previous guard, it is the half that guard
// cannot provide. There, a module that imports the Web transport exempts
// itself, which is correct for an aggregate that means to compose it and is
// exactly the wrong answer for a capability module that does not: an
// `extensions/*` module that started importing the Web transport would go
// quiet rather than red. The extension namespace is where the plan's original
// property lives -- a protocol-neutral capability must be selectable without
// inheriting a transport -- so it is asserted unconditionally.
func TestArchProtocolNeutralExtensionsNeverComposeTheWebStack(t *testing.T) {
	checked := 0
	for _, dir := range archWorkspaceModuleDirs(t) {
		relative := archRepositoryRelative(t, dir)
		if !strings.HasPrefix(relative, "extensions/") {
			continue
		}
		checked++
		closure := archModuleClosure(t, dir)
		assert.Falsef(t, archModuleComposesWebStack(closure),
			"module %s is part of the protocol-neutral extension namespace but imports the Web transport; a capability module must be selectable by a non-Web service", relative)
		for _, dep := range archModuleForeignDependencies(closure) {
			if archIsStdlib(dep) {
				continue
			}
			for _, forbidden := range []string{archTransportNamespace, archGinModulePath} {
				if archPathAtOrBelow(dep, forbidden) {
					t.Errorf("module %s is part of the protocol-neutral extension namespace, yet its production closure contains %q; protocol-neutral capabilities must compile without a transport",
						relative, dep)
				}
			}
		}
	}
	require.NotZero(t, checked,
		"no workspace module sits in the extensions namespace, so this guard read nothing; the namespace was renamed or go.work stopped listing it")
}

// ── guard 7: designated leaf packages are stdlib-only ──────────────────

// TestArchLeafPackagesDependOnStdlibOnly keeps the shared ordering graph
// independent of every framework layer and third-party module. archDeps includes
// each package itself, so that exact
// package subtree is the only non-stdlib path allowed in its own closure.
func TestArchLeafPackagesDependOnStdlibOnly(t *testing.T) {
	cases := []struct {
		pattern string
		owner   string
	}{
		{pattern: "./plugin/ordering/...", owner: "github.com/xbcio/xbc/plugin/ordering"},
	}

	for _, tc := range cases {
		for _, dep := range archDeps(t, tc.pattern) {
			if archIsStdlib(dep) || archPathAtOrBelow(dep, tc.owner) {
				continue
			}
			assert.Fail(t, "leaf package introduced non-standard library dependency",
				"%s's production dependency closure contains %q; this package must remain stdlib-only", tc.owner, dep)
		}
	}
}

// ── guard 8: config and log must not depend on each other ────────────────

// TestArchConfigAndLogAreMutuallyIndependent is the package-layout design §9
// rule 3 guard. config/arch_test.go already pins one direction from inside
// the config package; this pins *both*, because the rule is symmetric and a
// one-directional check leaves the cheaper mistake unguarded -- reaching for
// a config value from inside the logger is a far more tempting shortcut than
// reaching for the logger from inside config, and it is the direction no
// existing guard covers.
//
// The two are independent so that either can be used without the other: an
// application that wants xbc's config layering but its own logging, or a
// library that wants the logger and nothing else, must not be forced to
// compile in the package it did not ask for.
func TestArchConfigAndLogAreMutuallyIndependent(t *testing.T) {
	assert.NotContains(t, archDeps(t, "./config/..."), "github.com/xbcio/xbc/log",
		"config's dependency closure cannot contain log: both packages must be able to be used independently")
	assert.NotContains(t, archDeps(t, "./log/..."), "github.com/xbcio/xbc/config",
		"log's dependency closure cannot contain config: reading configuration from logger is the direction more likely to violate this rule")
}

// ── guard 9: closures may not grow beyond what their owner explains ─────

// archRootPackage is this repository's module path; everything at or below it
// is first-party.
const archRootPackage = "github.com/xbcio/xbc"

// archClosureCeiling asserts that subject's production closure adds no package
// -- third-party or first-party -- that bases do not already justify.
//
// The ceiling is expressed relative to another package's closure rather than
// as a frozen list of paths, so it stays correct when koanf or zap change
// their own transitive dependencies. What it catches is the subject reaching
// for a dependency of its own -- which belongs in an implementation package.
//
// firstParty names the repository subtrees the subject is allowed to contain
// beyond what bases explain: normally just its own subtree, plus any explicitly
// justified peer. Everything else in this
// repository is held to the same ceiling as foreign code, because a first-party
// addition is exactly how a leaf adapter quietly grows into an orchestrator.
func archClosureCeiling(t *testing.T, subject, owner string, firstParty []string, bases ...string) {
	t.Helper()
	allowed := make(map[string]bool)
	for _, base := range bases {
		for _, dep := range archDeps(t, base) {
			allowed[dep] = true
		}
	}
	exempt := func(dep string) bool {
		for _, prefix := range firstParty {
			if archPathAtOrBelow(dep, prefix) {
				return true
			}
		}
		return false
	}
	for _, dep := range archDeps(t, subject) {
		if allowed[dep] || archIsStdlib(dep) {
			continue
		}
		if archPathAtOrBelow(dep, archRootPackage) {
			if exempt(dep) {
				continue
			}
			t.Errorf("%s's production closure contains repository package %q, which %s does not explain; "+
				"this subject is a leaf over its bases, so reaching further up the repository belongs in an implementation package instead",
				owner, dep, strings.Join(bases, " or "))
			continue
		}
		t.Errorf("%s's production closure contains third-party package %q, which %s does not explain; "+
			"every implementer pays this compile cost, so it belongs in the capability package instead",
			owner, dep, strings.Join(bases, " or "))
	}
}

// TestArchPluginClosureAddsNothingBeyondConfigAndLog closes the gap a
// blacklist structurally cannot cover: a blacklist rejects only the names it
// already knows, so plugin's third-party closure would otherwise be free to
// grow indefinitely. plugin depends on config and log by design, so whatever
// those two already justify may legitimately appear here too.
//
// The sole first-party exemption is the Plugin framework's own subtree,
// including model, assembly, and autoload; guard 5 above pins that the subtree
// imports neither runtime nor an internal package.
func TestArchPluginClosureAddsNothingBeyondConfigAndLog(t *testing.T) {
	archClosureCeiling(t, "./plugin/...", "github.com/xbcio/xbc/plugin",
		[]string{archRootPackage + "/plugin"},
		"./config/...", "./log/...")
}

// TestArchAutoloadClosureIsPluginAndStdlibOnly keeps the optional
// process-global composition adapter a thin leaf over the SPI. It depends on
// plugin, which legitimately pulls in config and log, so the ceiling is stated
// relative to plugin's own closure rather than as a fixed allowlist.
//
// Unlike plugin, autoload has no import-direction guard of its own, so this is
// the only place its outbound first-party closure is constrained: an autoload
// that reached for assembly or runtime would be caught here and nowhere else.
func TestArchAutoloadClosureIsPluginAndStdlibOnly(t *testing.T) {
	archClosureCeiling(t, "./plugin/autoload/...", "github.com/xbcio/xbc/plugin/autoload",
		[]string{archRootPackage + "/plugin/autoload"},
		"./plugin")
}

// ── guard 10: assembly must not read optional process-wide autoload state ─
// TestArchAssemblyDoesNotReadDefaultAutoload keeps explicit Bundle assembly
// deterministic. Only the runtime adapter may select the optional global
// composition; assembly must consume exactly the Bundles in PlanOptions.
func TestArchAssemblyDoesNotReadDefaultAutoload(t *testing.T) {
	root := archRepositoryRoot(t)
	assemblyDir := filepath.Join(root, "plugin", "assembly")
	checked := 0
	for _, name := range archProductionGoFilesInDir(t, assemblyDir) {
		filePath := filepath.Join(assemblyDir, name)
		file, err := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.ImportsOnly)
		require.NoError(t, err, "Parsing %s failed", filePath)
		checked++
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			require.NoError(t, err, "Parsing %s import failed", filePath)
			assert.NotEqual(t, "github.com/xbcio/xbc/plugin/autoload", importPath,
				"assembly must not read optional process-wide autoload state")
		}
	}
	require.NotZero(t, checked, "plugin/assembly did not scan production Go files")
}
