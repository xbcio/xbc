package architecture_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path"
	"path/filepath"
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
}

// archGoCommand runs every Go command from the repository root. Test binaries
// normally start in their package directory, and callers may choose any other
// cwd, so relative package patterns must never inherit process cwd.
func archGoCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = archRepositoryRoot(t)
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
			"github.com/xbcio/xbc/transport",
			"github.com/xbcio/xbc/management",
			"github.com/xbcio/xbc/integration",
		} {
			if archPathAtOrBelow(req.Path, forbidden) {
				t.Errorf("core's go.mod must not require optional runtime stack or implementation module %q; these dependencies can only be owned by standalone module", req.Path)
			}
		}
	}
}

// ── guard 2: core must never directly import transport stacks ─────────────

// TestArchCorePackagesDoNotImportTransportStacks is the package-layout design
// §9 rule 1 guard for every optional transport module: transports depend on
// core, never the reverse. Guarding the parent namespace means a future
// transport/grpc module is covered as soon as it appears, without extending an
// implementation-name allowlist here.
//
// Checked across every package this module builds (`./...`), not just the root
// package, so a future core subpackage cannot reach sideways into a transport
// while the root package itself stays clean.
func TestArchCorePackagesDoNotImportTransportStacks(t *testing.T) {
	pkgs := archGoList(t, "./...")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if archPathAtOrBelow(dep, "github.com/xbcio/xbc/transport") {
				t.Errorf("%s contains direct import of %q: transport/* is standalone module, dependency direction must be transport → core, cannot be reversed", pkg.ImportPath, dep)
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
// The root package is a narrow application-facing facade. Package runtime owns
// lifecycle and process orchestration, assembly owns plugin assembly, and cli owns
// command parsing. plugin remains the protocol-neutral SPI beneath all three;
// it must not reach upward into the facade or higher-level owners.

// ── guard 5: plugin must not import the facade or higher-level owners ──

// TestArchPluginPackagesDoNotImportHigherLevelOwners walks every package
// under ./plugin/... and checks all three direct-import lists. The production
// closure guard in plugin/arch_test.go cannot see a forbidden import written
// only in a _test.go file, so this direct check is deliberately complementary.
func TestArchPluginPackagesDoNotImportHigherLevelOwners(t *testing.T) {
	pkgs := archGoList(t, "./plugin/...")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if dep == "github.com/xbcio/xbc" {
				t.Errorf("%s contains direct import of root package: plugin is protocol-agnostic SPI, must not be reverse depended by application entry", pkg.ImportPath)
			}
			for _, forbidden := range []string{
				"github.com/xbcio/xbc/runtime",
				"github.com/xbcio/xbc/assembly",
				"github.com/xbcio/xbc/cli",
				"github.com/xbcio/xbc/internal",
			} {
				if archPathAtOrBelow(dep, forbidden) {
					t.Errorf("%s contains direct import of %q: plugin must be located under runtime/assembly/cli, must not be reverse depended by upper layer capability owner", pkg.ImportPath, dep)
				}
			}
		}
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
			"github.com/xbcio/xbc/transport",
			"github.com/xbcio/xbc/management",
			"github.com/xbcio/xbc/integration",
		} {
			if archPathAtOrBelow(dep, forbidden) {
				t.Errorf("core's production dependency closure contains optional runtime stack %q; protocol implementation must remain in standalone module", dep)
			}
		}
	}
}

// ── guard 7: designated leaf packages are stdlib-only ──────────────────

// TestArchLeafPackagesDependOnStdlibOnly keeps the shared ordering graph,
// reflection scanner, and command parser independent of every framework layer
// and third-party module. archDeps includes each package itself, so that exact
// package subtree is the only non-stdlib path allowed in its own closure.
func TestArchLeafPackagesDependOnStdlibOnly(t *testing.T) {
	cases := []struct {
		pattern string
		owner   string
	}{
		{pattern: "./plugin/ordering/...", owner: "github.com/xbcio/xbc/plugin/ordering"},
		{pattern: "./assembly/inject/...", owner: "github.com/xbcio/xbc/assembly/inject"},
		{pattern: "./cli/...", owner: "github.com/xbcio/xbc/cli"},
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

// ── guard 9: assembly must not read the process-wide default catalog ─

// TestArchAssemblyDoesNotReadDefaultCatalog checks the API-level distinction
// imports alone cannot express. Package assembly may consume catalog.Snapshot,
// but it must never call catalog.Declare or catalog.Freeze and thereby reach the
// process-wide default catalog. Only runtime chooses and freezes a definition
// set; assembly assembles exactly the snapshot supplied in Options.
func TestArchAssemblyDoesNotReadDefaultCatalog(t *testing.T) {
	root := archRepositoryRoot(t)
	assemblyDir := filepath.Join(root, "assembly")
	forbidden := map[string]bool{"Declare": true, "Freeze": true}

	fset := token.NewFileSet()
	checked := 0
	for _, name := range archProductionGoFilesInDir(t, assemblyDir) {
		filePath := filepath.Join(assemblyDir, name)
		file, err := parser.ParseFile(fset, filePath, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "Parsing %s failed", filePath)
		checked++

		catalogAliases := make(map[string]bool)
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err, "Parsing %s's import path failed", filePath)
			if importPath != "github.com/xbcio/xbc/plugin/catalog" {
				continue
			}
			alias := path.Base(importPath)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "." {
				assert.Fail(t, "assembly must not dot-import catalog",
					"%s dot-imported plugin/catalog, default Catalog guard cannot reliably identify caller", filePath)
				continue
			}
			if alias != "_" {
				catalogAliases[alias] = true
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || !catalogAliases[ident.Name] || !forbidden[sel.Sel.Name] {
				return true
			}
			assert.Fail(t, "assembly layer read process-level default catalog",
				"%s line %d called %s.%s: assembly can only assemble calls made to its frozen Snapshot",
				filePath, fset.Position(call.Pos()).Line, ident.Name, sel.Sel.Name)
			return true
		})
	}
	require.NotZero(t, checked, "assembly/ did not scan production Go files, default Catalog guard actually did not take effect")
}
