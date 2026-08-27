// architecture_test.go
package architecture_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/topology"
)

// archPackageJSON mirrors the subset of `go list -json` used by this
// file's direction/reverse-dependency guards: each package's own *direct*
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
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}

	out, err := archGoCommand(t, "list", "-json", pattern).Output()
	require.NoError(t, err, "go list -json %s 失败", pattern)

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []archPackageJSON
	for dec.More() {
		var pkg archPackageJSON
		require.NoError(t, dec.Decode(&pkg), "解析 go list -json %s 输出失败", pattern)
		pkgs = append(pkgs, pkg)
	}
	if len(pkgs) == 0 {
		t.Fatalf("go list -json %s 没有返回任何包，依赖方向检查实际未生效", pattern)
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
// repository root resolves only within the core module even under go.work -- web
// and examples each carry their own go.mod and their own arch_test.go).
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
				t.Errorf("%s 中直接 import 了 %q：core 是协议无关运行时，Gin 只能存在于独立的 web module 里", pkg.ImportPath, dep)
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
			"github.com/xbcio/xbc/web",
			"github.com/xbcio/xbc/grpc",
			"github.com/xbcio/xbc/management",
			"github.com/xbcio/xbc/integration",
		} {
			if archPathAtOrBelow(req.Path, forbidden) {
				t.Errorf("core 的 go.mod 不得 require 可选运行栈或实现 module %q；这些依赖只能由独立 module 拥有", req.Path)
			}
		}
	}
}

// ── guard 2: core must never directly import web ──────────────────────────

// TestArchCorePackagesDoNotImportWeb is the package-layout design §9 rule 1
// guard for the web module specifically: the dependency direction is
// web -> core, never the reverse, and nothing in this module is allowed to
// reach for github.com/xbcio/xbc/web or any of its subpackages.
//
// Checked across every package this module builds (`./...`), not just the
// root package: the invariant being protected is "web is unreachable from
// anywhere inside core", and restricting the scan to the root package alone
// would miss a hypothetical future core subpackage reaching for web
// directly while the root package itself stays clean.
func TestArchCorePackagesDoNotImportWeb(t *testing.T) {
	pkgs := archGoList(t, "./...")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if archPathAtOrBelow(dep, "github.com/xbcio/xbc/web") {
				t.Errorf("%s 中出现了对 %q 的直接 import：web 是独立 module，依赖方向必须是 web → core，不能反过来", pkg.ImportPath, dep)
			}
		}
	}
}

// ── guard 3: the root package must never directly import examples ────────

// TestArchRootPackageDoesNotImportExamples is the package-layout design §4
// guard for the examples module ("examples 明确不负责：被 core 或 web 反向依赖"):
// the root package is what an example imports, never the other way round.
//
// Scoped to pattern "." (the root package alone), matching the task's own
// framing of this rule ("根包不得 import examples") -- unlike guard 2 above,
// this is not a "reachable from anywhere in core" invariant, it is
// specifically about the one package an example's main() actually imports.
func TestArchRootPackageDoesNotImportExamples(t *testing.T) {
	pkgs := archGoList(t, ".")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if dep == "github.com/xbcio/xbc/examples" || strings.HasPrefix(dep, "github.com/xbcio/xbc/examples/") {
				t.Errorf("根包中出现了对 %q 的直接 import：examples 是消费 xbc.Run() 的示例 module，不能被核心反向依赖", dep)
			}
		}
	}
}

// ── why there is no "root package must not import internal/*" guard ──────
//
// web's and examples' own architecture guards forbid direct imports of
// github.com/xbcio/xbc/internal/..., which is correct for independent modules:
// their only sanctioned entry is core's public API. The root package is inside
// this module and is now the runtime itself, so it necessarily imports
// internal/cli and internal/container directly; forbidding every root ->
// internal edge would reject the intended structure. What keeps that from
// becoming a licence to sprawl is a different pair of guards: the exact
// root-file allowlist and the frozen public-API set below. Go's import-cycle
// check prevents anything under internal/ from importing the root back.
//
// plugin remains different: it is the protocol-neutral SPI below the runtime
// and has no legitimate reason to reach into either the root package or
// internal/*. Guard 5 enforces that reverse boundary, and guard 7 keeps the
// designated leaf packages free of third-party dependencies.

// ── guard 5: plugin must never import the root package or internal/* ─────

// TestArchPluginPackageDoesNotImportRootOrInternal is the package-layout
// design §9 rule 2 guard, walked directly here (over every package under
// ./plugin/...) rather than trusted solely to plugin/arch_test.go's own
// closure-based guard.
//
// This adds real coverage plugin/arch_test.go's Deps-based check cannot:
// goListDeps there calls `go list -json <pkg>` without -test, so an import
// written only inside a _test.go file in `plugin` never appears in that
// Deps list at all -- Deps is the production build graph, and Go does not
// compile _test.go files into it. Reading Imports/TestImports/XTestImports
// here (as `go list -json` reports them even without -test) is what makes
// a forbidden import hidden inside plugin's own tests visible instead of
// silently passing.
func TestArchPluginPackageDoesNotImportRootOrInternal(t *testing.T) {
	pkgs := archGoList(t, "./plugin/...")
	for _, pkg := range pkgs {
		for _, dep := range archDirectImports(pkg) {
			if dep == "github.com/xbcio/xbc" {
				t.Errorf("%s 中出现了对根包的直接 import：plugin 是协议无关 SPI，反向依赖根包会与根包 import plugin 形成循环", pkg.ImportPath)
			}
			if dep == "github.com/xbcio/xbc/internal" || strings.HasPrefix(dep, "github.com/xbcio/xbc/internal/") {
				t.Errorf("%s 中出现了对 %q 的直接 import：plugin 是所有插件依赖的底座，不得绕过自己的公开 API 反向伸进 internal/*", pkg.ImportPath, dep)
			}
		}
	}
}

// ── guard 6: config/log/topology must never import the root package ──────

// TestArchConfigLogTopologyDoNotImportRootPackage is the package-layout
// design §6 rule 3 guard ("config 与 log 彼此独立，且都不得 import 根包"),
// extended to topology for the same reason: topology is documented as a
// zero-dependency stable leaf (design §4, §9 rule 4) and the root package
// is exactly the kind of upward dependency a leaf package must never
// acquire -- root already imports internal/container, which imports
// topology, so a topology -> root edge would be a real import cycle, not
// just a layering smell.
func TestArchConfigLogTopologyDoNotImportRootPackage(t *testing.T) {
	for _, pattern := range []string{"./config/...", "./log/...", "./topology/..."} {
		pkgs := archGoList(t, pattern)
		for _, pkg := range pkgs {
			for _, dep := range archDirectImports(pkg) {
				assert.NotEqual(t, "github.com/xbcio/xbc", dep,
					"%s 中出现了对根包的直接 import：这些包都在根包的依赖闭包之下，反向 import 根包会直接形成编译期循环", pkg.ImportPath)
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
// the way that matters -- `topology` could acquire a third-party dependency
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
		t.Skip("go 命令不可用，跳过依赖闭包检查")
	}

	out, err := archGoCommand(t, "list", "-deps", pattern).Output()
	require.NoError(t, err, "go list -deps %s 失败", pattern)

	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}
	if len(deps) == 0 {
		t.Fatalf("go list -deps %s 没有返回任何依赖，闭包检查实际未生效", pattern)
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
			"github.com/xbcio/xbc/web",
			"github.com/xbcio/xbc/grpc",
			"github.com/xbcio/xbc/management",
			"github.com/xbcio/xbc/integration",
		} {
			if archPathAtOrBelow(dep, forbidden) {
				t.Errorf("core 的生产依赖闭包包含可选运行栈 %q；协议实现必须留在独立 module", dep)
			}
		}
	}
}

// ── guard 7: the leaf packages must compile in nothing but the stdlib ────

// TestArchLeafPackagesDependOnStdlibOnly is the package-layout design §9
// rule 4 guard ("叶子纯净性：topology、internal/container/inject、
// internal/cli 只允许标准库").
//
// All three packages are documented as zero-dependency leaves, and that claim
// is load-bearing rather than decorative: topology is what lets a future grpc
// or management module do dependency ordering without taking on any of
// core's own dependencies, internal/container/inject is the reflection-only
// implementation detail underneath the container, and internal/cli is the
// argument parser -- a place where
// reaching for a third-party flag library (cobra, pflag, urfave/cli) is the
// single most tempting shortcut in the whole module, and one the design
// explicitly rejected. A third-party dependency acquired in any of them
// propagates to every consumer of core, so this is checked as a closure --
// the violation that actually happens in practice is not `import
// "github.com/spf13/cast"` written into topology directly, it is topology
// importing some innocuous helper package that drags cast in behind it.
//
// Note what this does NOT check: xbc's own packages are skipped, so this
// says nothing about the internal import direction. internal/cli importing
// the root package is prevented by Go itself (the root package imports
// internal/cli, so the reverse edge is a compile-time import cycle), not by
// this guard.
func TestArchLeafPackagesDependOnStdlibOnly(t *testing.T) {
	for _, pattern := range []string{"./topology/...", "./internal/container/inject/...", "./internal/cli/..."} {
		for _, dep := range archDeps(t, pattern) {
			if archIsStdlib(dep) || strings.HasPrefix(dep, "github.com/xbcio/xbc/") {
				continue
			}
			assert.Fail(t, "叶子包引入了第三方依赖",
				"%s 的依赖闭包里出现了 %q。topology、internal/container/inject 与 internal/cli 被设计为零第三方依赖的稳定叶子，"+
					"它们的依赖会传递给 core 的每一个使用者", pattern, dep)
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
		"config 的依赖闭包里不能出现 log：两个包必须能各自独立使用")
	assert.NotContains(t, archDeps(t, "./log/..."), "github.com/xbcio/xbc/config",
		"log 的依赖闭包里不能出现 config：从 logger 里去读配置是这条规则更容易被违反的方向")
}

// ── guard 9: the container must not read the process-wide default catalog ─

// TestArchContainerDoesNotReadDefaultCatalog is the package-layout design §9
// rule 5 guard, in the one form that catches the violation it is about.
//
// internal/container legitimately imports plugin/catalog -- it needs the
// catalog.Snapshot *type*, since a Snapshot is what a Container is built
// from. So an import-based check cannot express this rule at all: the import
// is required, and what must never happen is the container calling the
// package-level accessors that reach the one process-wide default catalog
// (catalog.Declare / catalog.Freeze). Those are exactly the calls that would
// let an App assemble plugins nobody handed it, silently defeating
// WithDefinitions' isolation guarantee (see xbc_test.go).
//
// This is therefore a source-level guard rather than an import-graph one,
// and it walks the parsed AST rather than the file text. Scanning for the
// string "catalog.Freeze(" would fire on a *comment* mentioning the call --
// expand.go already carries one that misses only by a parenthesis -- turning
// the next person's accurate documentation into a build failure. Matching
// call expressions instead means the guard fires on calls and only on calls.
//
// The remaining limitation is that a call made through a local alias would
// go unseen. That is worth accepting: aliasing the catalog package to sneak
// past a named architecture guard is not the mistake this protects against.
func TestArchContainerDoesNotReadDefaultCatalog(t *testing.T) {
	const relativeDir = "internal/container"
	dir := filepath.Join(archRepositoryRoot(t), filepath.FromSlash(relativeDir))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "读取 %s 目录失败", relativeDir)

	// The forbidden set is exactly plugin/catalog's package-level accessors
	// to the one process-wide default catalog. Everything else the package
	// exports (notably the Snapshot type) is legitimate for the container.
	forbidden := map[string]bool{"Declare": true, "Freeze": true}

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "解析 %s 失败", path)
		checked++

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
			if !ok || ident.Name != "catalog" || !forbidden[sel.Sel.Name] {
				return true
			}
			assert.Fail(t, "容器读取了进程级默认 catalog",
				"%s 第 %d 行调用了 catalog.%s：容器只能装配调用方交给它的那份已冻结 Snapshot，"+
					"读取进程级默认 catalog 会让 WithDefinitions 的隔离保证失效",
				path, fset.Position(call.Pos()).Line, sel.Sel.Name)
			return true
		})
	}
	if checked == 0 {
		t.Fatalf("%s 下没有扫描到任何生产源文件，该守卫实际未生效", relativeDir)
	}
}

// ── guard 10: retired paths and responsibility owners stay canonical ─────

// archProductionGoFilesInDir returns production Go file names directly in dir.
// Every path passed here is absolute and rooted at archRepositoryRoot.
func archProductionGoFilesInDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "读取生产源码目录 %s 失败", dir)

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

// archProductionGoFilesUnder returns slash-normalized production Go paths
// relative to dir, including nested directories. This prevents a new runtime
// subpackage from silently bypassing the canonical-file and process-owner
// checks.
func archProductionGoFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(dir, filePath)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	require.NoError(t, err, "遍历生产源码目录 %s 失败", dir)
	sort.Strings(files)
	return files
}

// TestArchLegacyInternalPathsStayRetired prevents retired packages from
// becoming a second owner beside their canonical replacements.
//
// Two generations are listed. internal/inject and internal/report were broad
// utility packages superseded by internal/container/inject and the root's
// startup_report.go. internal/runtime, internal/startupreport and
// internal/architecture are the pseudo-boundaries removed when the runtime was
// merged into the root package: each had a single consumer, and none of the
// three bought anything the compiler was not already giving (internal/runtime
// had a dependency closure byte-identical to the root's, internal/startupreport
// only forced its test fakes to be duplicated, and internal/architecture was a
// test-only package living under a tree that means "implementation"). Their
// replacements are the root package itself, root startup_report.go, and
// tests/architecture -- this file.
func TestArchLegacyInternalPathsStayRetired(t *testing.T) {
	root := archRepositoryRoot(t)
	retiredPaths := []string{
		"internal/inject",
		"internal/report",
		"internal/runtime",
		"internal/startupreport",
		"internal/architecture",
	}
	for _, retired := range retiredPaths {
		retiredPath := filepath.Join(root, filepath.FromSlash(retired))
		_, err := os.Stat(retiredPath)
		if err == nil {
			t.Errorf("旧路径 %s 不得复活；请使用当前 canonical owner", retired)
			continue
		}
		require.ErrorIs(t, err, os.ErrNotExist, "检查旧路径 %s 失败", retired)
	}
}

// TestArchRootResponsibilitiesHaveCanonicalFiles pins the root package's file
// set. Test files are intentionally excluded; production files are both
// required and allowlisted so an extra owner cannot quietly reappear.
//
// The root used to be a three-file facade (app.go / doc.go / run.go) wrapping
// an internal/runtime package. That split was removed: the two packages had
// byte-identical dependency closures, so it isolated nothing, and its cost was
// a pass-through App type whose Execute forwarded verbatim plus four symbols
// exported solely to cross the boundary. What the split did enforce -- that a
// capitalized identifier added to the runtime cannot reach the public API --
// is now enforced directly by TestArchRootPublicAPIIsFrozen below. That guard
// is load-bearing: without it this merge would have traded a compiler-checked
// boundary for a convention.
func TestArchRootResponsibilitiesHaveCanonicalFiles(t *testing.T) {
	root := archRepositoryRoot(t)
	wantRoot := []string{
		"app.go",
		"bootstrap.go",
		"doc.go",
		"execute.go",
		"host.go",
		"lifecycle.go",
		"process.go",
		"run.go",
		"settings.go",
		"shutdown.go",
		"startup_report.go",
		"task.go",
	}
	assert.Equal(t, wantRoot, archProductionGoFilesInDir(t, root),
		"根包的生产职责必须由 canonical files 唯一拥有；"+
			"新增文件需同步更新包结构设计文档的目录树")

	for _, retired := range []string{"xbc.go", "report.go", "render.go"} {
		retiredPath := filepath.Join(root, retired)
		_, err := os.Stat(retiredPath)
		if err == nil {
			t.Errorf("根包旧职责聚合文件 %s 不得复活", retired)
			continue
		}
		require.ErrorIs(t, err, os.ErrNotExist, "检查根包旧文件 %s 失败", retired)
	}

	stageFiles, err := filepath.Glob(filepath.Join(root, "stage_*.go"))
	require.NoError(t, err, "匹配根包 stage 文件失败")
	productionStages := stageFiles[:0]
	for _, name := range stageFiles {
		if !strings.HasSuffix(name, "_test.go") {
			productionStages = append(productionStages, name)
		}
	}
	assert.Empty(t, productionStages,
		"根包生产代码不得按时间阶段重新拆出 stage_*.go；运行时职责应按 canonical owner 归档")
}

// TestArchRootPublicAPIIsFrozen is the guard that replaced the internal/
// boundary when the runtime moved into the root package.
//
// Before the merge, "what is public" was answered by the compiler: everything
// in internal/runtime was unreachable from outside the module no matter how it
// was spelled, and the root facade was three small files a reviewer could hold
// in their head. After the merge, twelve production files sit in the package
// whose namespace IS the public API, and a single capitalized letter is the
// entire difference between an implementation detail and a promise. That is
// too thin a margin to leave to review, so it is pinned here instead.
//
// The set below is deliberately tiny and should stay that way: an application
// calls Run, an embedding host calls New and App.Execute, and a test supplies
// its own catalog through WithDefinitions. Adding to it is a real API decision
// and must be a deliberate edit to this list, not a side effect of a rename.
//
// Scope note: this collects exported top-level declarations plus exported
// methods on exported types. An exported method on an unexported type is not
// counted -- it is only reachable if the value escapes through an interface,
// which the RuntimeHost shape guard already pins separately.
func TestArchRootPublicAPIIsFrozen(t *testing.T) {
	want := []string{
		"App",
		"App.Execute",
		"New",
		"Option",
		"Run",
		"WithDefinitions",
	}

	root := archRepositoryRoot(t)
	names := archExportedNamesInDir(t, root)
	assert.Equal(t, want, names,
		"根包的公开 API 已漂移。runtime 并入根包后，'是否公开' 不再由 internal/ 边界保证，"+
			"只差一个首字母大小写；新增导出必须是对这份清单的明确修改，而不是重命名的副作用")
}

// archExportedNamesInDir returns the sorted exported API surface of the
// package rooted at dir: top-level types, funcs, vars and consts, plus
// methods on exported types rendered as "Type.Method".
func archExportedNamesInDir(t *testing.T, dir string) []string {
	t.Helper()

	fset := token.NewFileSet()
	var names []string
	for _, base := range archProductionGoFilesInDir(t, dir) {
		file, err := parser.ParseFile(fset, filepath.Join(dir, base), nil, parser.SkipObjectResolution)
		require.NoError(t, err, "解析 %s 失败", base)

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					names = append(names, d.Name.Name)
					continue
				}
				if recv := archReceiverTypeName(d.Recv); recv != "" && ast.IsExported(recv) {
					names = append(names, recv+"."+d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							names = append(names, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, ident := range s.Names {
							if ident.IsExported() {
								names = append(names, ident.Name)
							}
						}
					}
				}
			}
		}
	}
	require.NotEmpty(t, names, "根包没有解析到任何导出符号，API 冻结守卫实际未生效")
	sort.Strings(names)
	return names
}

// archReceiverTypeName unwraps a method receiver down to its bare type name,
// handling both value and pointer receivers as well as generic type
// parameters (App[T] / *App[T]). Returns "" when the shape is unrecognized,
// which the caller treats as "not an exported method".
func archReceiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		if ident, ok := e.X.(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.IndexListExpr:
		if ident, ok := e.X.(*ast.Ident); ok {
			return ident.Name
		}
	}
	return ""
}

// TestArchContainerHasCanonicalFiles gives internal/container the same
// allowlist the root package has above. Without it, container was the one
// assembly package whose file set could drift from the layout design with
// nothing failing: scan.go reached production while the design document still
// listed eight files, and it went unnoticed for the additional reason that
// container/inject holds a scan.go too, so grepping the design for "scan.go"
// finds a match that is a different file.
//
// The walk is recursive, so the inject subpackage is pinned by the same list
// and a new container subpackage cannot bypass the check by nesting.
func TestArchContainerHasCanonicalFiles(t *testing.T) {
	root := archRepositoryRoot(t)
	containerDir := filepath.Join(root, "internal", "container")
	wantContainer := []string{
		"bind.go",
		"callback.go",
		"container.go",
		"declarations.go",
		"expand.go",
		"initialize.go",
		"inject/scan.go",
		"instance.go",
		"registry.go",
		"resolve.go",
		"scan.go",
	}
	assert.Equal(t, wantContainer, archProductionGoFilesUnder(t, containerDir),
		"internal/container 的生产职责必须由 canonical files 唯一拥有；"+
			"新增文件需同步更新包结构设计文档的目录树")
}

// TestArchProcessConcernsRemainInProcessAdapter parses every production file
// in the root package and the complete internal tree, resolving import aliases
// before inspecting selectors. Test files are deliberately excluded by the
// production-file walkers. process.go is the sole owner of process arguments,
// signals, diagnostics, logger flushing, and termination -- that is what keeps
// App.Execute embeddable, and it matters more now that process.go is a sibling
// of the other runtime files rather than being fenced off in its own package.
func TestArchProcessConcernsRemainInProcessAdapter(t *testing.T) {
	type concernKey struct {
		importPath string
		selector   string
	}
	concerns := map[concernKey]string{
		{importPath: "os", selector: "Args"}:                       "os.Args",
		{importPath: "os", selector: "Exit"}:                       "os.Exit",
		{importPath: "os", selector: "Stderr"}:                     "os.Stderr",
		{importPath: "os/signal", selector: "Notify"}:              "signal.Notify/NotifyContext",
		{importPath: "os/signal", selector: "NotifyContext"}:       "signal.Notify/NotifyContext",
		{importPath: "github.com/xbcio/xbc/log", selector: "Sync"}: "log.Sync",
	}
	const (
		owner         = "process.go"
		exitHook      = "osExit"
		exitHookLabel = "package-level osExit"
	)
	seenInOwner := map[string]bool{}

	root := archRepositoryRoot(t)
	internalDir := filepath.Join(root, "internal")
	var sourcePaths []string
	for _, name := range archProductionGoFilesInDir(t, root) {
		sourcePaths = append(sourcePaths, filepath.Join(root, name))
	}
	for _, name := range archProductionGoFilesUnder(t, internalDir) {
		sourcePaths = append(sourcePaths, filepath.Join(internalDir, filepath.FromSlash(name)))
	}
	require.NotEmpty(t, sourcePaths, "根包和 internal/** 没有扫描到生产 Go 文件，进程设施守卫实际未生效")

	fset := token.NewFileSet()
	for _, filePath := range sourcePaths {
		relative, err := filepath.Rel(root, filePath)
		require.NoError(t, err, "计算 %s 的仓库相对路径失败", filePath)
		name := filepath.ToSlash(relative)

		file, err := parser.ParseFile(fset, filePath, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "解析 %s 失败", name)

		aliases := make(map[string]string, len(file.Imports))
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err, "解析 %s 的 import path 失败", name)
			if importPath == "os/signal" && name != owner {
				assert.Fail(t, "进程 signal owner 漂移",
					"%s import 了 os/signal；所有 signal 操作只能由 %s 拥有", name, owner)
			}
			alias := path.Base(importPath)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "_" {
				continue
			}
			if alias == "." {
				for key := range concerns {
					if key.importPath == importPath {
						assert.Fail(t, "进程设施不得使用 dot import",
							"%s dot-import 了 %s，架构守卫无法可靠确认 owner", name, importPath)
						break
					}
				}
				continue
			}
			aliases[alias] = importPath
		}

		// osExit is deliberately package-scoped so same-package tests can replace
		// process termination. Reserve that identifier to process.go throughout
		// the root package; otherwise a sibling file could invoke or alias the hook
		// without producing an imported selector for the guard below to inspect.
		if path.Dir(name) == path.Dir(owner) && name != owner {
			ast.Inspect(file, func(node ast.Node) bool {
				ident, ok := node.(*ast.Ident)
				if !ok || ident.Name != exitHook {
					return true
				}
				assert.Fail(t, "进程退出 hook owner 漂移",
					"%s 第 %d 行使用 %s；包级退出 hook 只能由 %s 持有和调用",
					name, fset.Position(ident.Pos()).Line, exitHook, owner)
				return true
			})
		}

		// Require a real package-level var declaration in the owner, rather than
		// letting incidental identifier text make the owner-presence check pass.
		if name == owner {
			for _, declaration := range file.Decls {
				gen, ok := declaration.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, declaredName := range valueSpec.Names {
						if declaredName.Name == exitHook {
							seenInOwner[exitHookLabel] = true
						}
					}
				}
			}
		}

		ast.Inspect(file, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, ok := aliases[ident.Name]
			if !ok {
				return true
			}
			label, guarded := concerns[concernKey{importPath: importPath, selector: sel.Sel.Name}]
			if !guarded {
				return true
			}
			if name != owner {
				assert.Fail(t, "进程设施 owner 漂移",
					"%s 第 %d 行使用 %s；os.Args、signal、os.Stderr、log.Sync 与 os.Exit 只能由 %s 拥有，App.Execute 必须可嵌入",
					name, fset.Position(sel.Pos()).Line, label, owner)
				return true
			}
			seenInOwner[label] = true
			return true
		})
	}
	for _, label := range []string{
		"os.Args",
		"os.Exit",
		"os.Stderr",
		"signal.Notify/NotifyContext",
		"log.Sync",
		exitHookLabel,
	} {
		assert.True(t, seenInOwner[label], "%s 必须继续实际持有 %s，否则 owner 守卫可能在空跑", owner, label)
	}
}

// archModuleFile mirrors the go.mod fields used by the release-boundary
// guard. Parsing through `go mod edit -json` delegates go.mod syntax and
// replace shapes to the Go tool instead of maintaining a fragile line parser.
type archModuleFile struct {
	Require []struct {
		Path    string
		Version string
	}
	Replace []json.RawMessage
}

// archRepositoryRoot locates this checked-in test file, ascends to the
// repository root, and never trusts the process working directory. Architecture
// tests are also run with
// GOWORK=off during release checks, and tooling may invoke a test binary from
// another directory, so neither cwd nor go's workspace discovery is a stable
// way to find repository manifests.
func archRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok, "无法定位 architecture_test.go，不能可靠读取仓库")

	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	absoluteRoot, err := filepath.Abs(root)
	require.NoError(t, err, "无法将仓库根转换为绝对路径：%s", root)
	root = absoluteRoot
	for _, marker := range []string{"go.mod", "go.work"} {
		info, err := os.Stat(filepath.Join(root, marker))
		require.NoError(t, err, "architecture_test.go 推导的仓库根 %s 缺少 %s", root, marker)
		require.False(t, info.IsDir(), "仓库根 %s 必须是文件", marker)
	}
	return root
}

// archReadModuleFile parses one manifest with the Go tool so replace forms,
// comments and block formatting are handled by the same parser as builds.
// Passing the manifest as go mod edit's explicit target also avoids inheriting
// workspace selection from cwd or GOWORK.
func archReadModuleFile(t *testing.T, manifest string) archModuleFile {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过 module manifest 检查")
	}

	if !filepath.IsAbs(manifest) {
		manifest = filepath.Join(archRepositoryRoot(t), manifest)
	}
	manifest = filepath.Clean(manifest)
	out, err := archGoCommand(t, "mod", "edit", "-json", manifest).Output()
	require.NoError(t, err, "解析 %s 失败", manifest)

	var mod archModuleFile
	require.NoError(t, json.Unmarshal(out, &mod), "解析 %s 的 go mod edit -json 输出失败", manifest)
	return mod
}

type archWorkspaceFile struct {
	Use []struct {
		DiskPath string
	}
}

// archWorkspaceModuleFiles returns every non-root module joined by the
// checked-in repository go.work. The explicit absolute workspace path makes
// this independent of cwd and an inherited GOWORK value (including "off").
// Discovering modules keeps this publication guard effective when a grpc,
// management or integration module is added without editing this test.
func archWorkspaceModuleFiles(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	workspace := filepath.Join(repositoryRoot, "go.work")
	out, err := archGoCommand(t, "work", "edit", "-json", workspace).Output()
	require.NoError(t, err, "解析仓库 workspace %s 失败", workspace)

	var work archWorkspaceFile
	require.NoError(t, json.Unmarshal(out, &work), "解析 go work edit -json 输出失败")
	var manifests []string
	for _, use := range work.Use {
		moduleRoot := use.DiskPath
		if !filepath.IsAbs(moduleRoot) {
			moduleRoot = filepath.Join(repositoryRoot, moduleRoot)
		}
		moduleRoot = filepath.Clean(moduleRoot)
		if moduleRoot == repositoryRoot {
			continue
		}
		manifest := filepath.Join(moduleRoot, "go.mod")
		info, err := os.Stat(manifest)
		require.NoError(t, err, "workspace module %s 缺少 go.mod", moduleRoot)
		require.False(t, info.IsDir(), "workspace module manifest %s 必须是文件", manifest)
		manifests = append(manifests, manifest)
	}
	require.NotEmpty(t, manifests, "go.work 没有列出任何独立子 module，发布边界守卫实际未生效")
	sort.Strings(manifests)
	return manifests
}

// TestArchWorkspaceModuleDiscoveryIgnoresCWDAndGOWORK is the regression for
// release-mode `GOWORK=off go test -mod=readonly ./...`. Discovery must still
// inspect this repository's checked-in workspace, not ask the go command to
// discover a workspace from process state.
func TestArchWorkspaceModuleDiscoveryIgnoresCWDAndGOWORK(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	t.Setenv("GOWORK", "off")
	t.Chdir(t.TempDir())

	manifests := archWorkspaceModuleFiles(t)
	assert.Contains(t, manifests, filepath.Join(repositoryRoot, "web", "go.mod"))
	assert.Contains(t, manifests, filepath.Join(repositoryRoot, "examples", "go.mod"))
	for _, manifest := range manifests {
		assert.True(t, filepath.IsAbs(manifest), "workspace manifest 必须解析为绝对路径：%s", manifest)
	}
}

// archStableReleaseVersion accepts an exact release-tag-shaped semantic
// version only. Pseudo-versions, prereleases, build metadata and the common
// v0.0.0 placeholder are intentionally rejected. Whether that tag exists on
// the remote is then proven by the release job with GOWORK=off; an ordinary
// architecture test must not depend on network availability.
func archStableReleaseVersion(version string) bool {
	if version == "v0.0.0" || !strings.HasPrefix(version, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// TestArchWorkspaceModulesRejectLocalReplaceAndSyntheticVersions protects the
// publication contract for independently consumable modules. Missing xbc
// requirements are valid before the corresponding module has a real tag;
// once a requirement is added it must name an exact release, never a local
// replace, zero placeholder or generated pseudo-version.
func TestArchWorkspaceModulesRejectLocalReplaceAndSyntheticVersions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过 module manifest 检查")
	}

	for _, manifest := range archWorkspaceModuleFiles(t) {
		mod := archReadModuleFile(t, manifest)
		assert.Empty(t, mod.Replace,
			"%s 不得包含 replace；仓库内联调用 go.work，发布验证必须解析真实 tag", manifest)

		for _, req := range mod.Require {
			if req.Path != "github.com/xbcio/xbc" && !strings.HasPrefix(req.Path, "github.com/xbcio/xbc/") {
				continue
			}
			assert.Truef(t, archStableReleaseVersion(req.Version),
				"%s 对 %s 的 require 必须使用真实发布 tag 形状 vX.Y.Z；禁止 v0.0.0、伪版本、预发布或占位版本，得到 %q",
				manifest, req.Path, req.Version)
		}
	}
}

// TestArchTopologyPublicAPIShape locks the small cross-module surface that
// replaced internal/graph. Sort behavior has focused tests in topology; this
// guard only protects the public names and signatures other modules compile
// against, including the deliberate removal of AddEdge(..., hard bool).
func TestArchTopologyPublicAPIShape(t *testing.T) {
	graphType := reflect.TypeOf((*topology.Graph)(nil))
	wantEdgeMethod := reflect.TypeOf((func(*topology.Graph, string, string))(nil))
	for _, name := range []string{"AddHardEdge", "AddSoftEdge"} {
		method, ok := graphType.MethodByName(name)
		require.True(t, ok, "topology.Graph.%s 必须存在", name)
		assert.Equal(t, wantEdgeMethod, method.Type, "topology.Graph.%s 签名必须是 (string, string)", name)
	}
	_, hasLegacyAddEdge := graphType.MethodByName("AddEdge")
	assert.False(t, hasLegacyAddEdge, "含义模糊的 topology.Graph.AddEdge(..., hard bool) 不得复活")

	var after topology.Direction = topology.After
	var before topology.Direction = topology.Before
	directionType := reflect.TypeOf(after)
	assert.Equal(t, "Direction", directionType.Name(), "After/Before 必须属于命名类型 topology.Direction")
	assert.Equal(t, "github.com/xbcio/xbc/topology", directionType.PkgPath())
	assert.NotEqual(t, after, before, "After 与 Before 必须是不同的 typed direction")
}

// TestArchRuntimeHostPublicAPIShape locks the deliberately narrow reverse
// port between plugin.Context and the root package's hostAdapter. Test doubles would
// catch many signature changes at compile time, but reflection makes the
// architectural rule explicit and also catches a method being removed or a
// fifth convenience method being added.
func TestArchRuntimeHostPublicAPIShape(t *testing.T) {
	hostType := reflect.TypeOf((*plugin.RuntimeHost)(nil)).Elem()
	require.Equal(t, reflect.Interface, hostType.Kind())
	require.Equal(t, 4, hostType.NumMethod(), "plugin.RuntimeHost 必须保持四方法窄端口")

	want := map[string]reflect.Type{
		"ProvideValue":       reflect.TypeOf((func(reflect.Type, string, any))(nil)),
		"LookupValue":        reflect.TypeOf((func(reflect.Type, string) (any, error))(nil)),
		"InitializedPlugins": reflect.TypeOf((func() ([]plugin.Extension[any], error))(nil)),
		"GoManaged":          reflect.TypeOf((func(plugin.Identity, func(context.Context), bool))(nil)),
	}
	for name, signature := range want {
		method, ok := hostType.MethodByName(name)
		require.True(t, ok, "plugin.RuntimeHost.%s 必须存在", name)
		assert.Equal(t, signature, method.Type, "plugin.RuntimeHost.%s 签名漂移", name)
	}
}
