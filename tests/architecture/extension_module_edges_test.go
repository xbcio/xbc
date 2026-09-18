package architecture_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archExtensionSharedVocabularyModules is the complete, intentional set of
// modules beneath extensions/ that another extension module's production code
// may import, as slash-separated paths relative to the repository root.
//
// Everything else in the namespace is a capability plugin module, and one
// capability may not depend on another. An application selects capabilities one
// at a time at its composition root; a plugin that imports a sibling plugin
// makes that choice on the application's behalf, because Go's unit of
// compilation is the package. A single call into a sibling's package pulls in
// every Definition, configuration section, and health contributor that package
// declares -- the importing plugin now ships a plugin nobody selected. The edge
// is also unpublishable: no manifest here may require an unreleased sibling, so
// such an import resolves through go.work alone and breaks the moment the module
// is consumed from outside this repository.
//
// The two members are the shared vocabularies that make the rest of the
// namespace composable, and they earn the exemption on the same ground: a
// consumer depends on their types, never on a runtime they provide.
//
//   - coordination/lease is a contract module (see archContractModules): two
//     interfaces over the standard library, owning no Definition, Config, or
//     Bundle. TestArchContractModulesOwnNoDefinitionConfigOrBundle is what makes
//     that a checked claim rather than a description.
//   - reliability/health is a plugin module, and is listed anyway. It publishes
//     the probe vocabulary -- Contributor, NamedChecker, CheckFunc, Readiness --
//     that every capability implements in order to be observable at all, and its
//     production closure is the standard library alone, so importing it adds no
//     transitive weight. Its Definition is an aggregator a service configures
//     separately; a contributor names the types and never the Definition. This
//     is the one place where a plugin module is a dependency target, so it is
//     spelled out here rather than derived.
//
// extensions/authentication is deliberately absent. It is a contract module, so
// it would qualify, but today only the Web namespace consumes it; listing an
// unexercised target is what TestArchExtensionSharedVocabularyIsExercised
// refuses. Adding a member here is an architecture decision, and a one-line one.
var archExtensionSharedVocabularyModules = []string{
	"extensions/coordination/lease",
	"extensions/reliability/health",
}

// archExtensionEdge is one production import from inside one extension module
// into a different one.
type archExtensionEdge struct {
	file       string
	line       int
	importPath string
	// source and target are the owning modules, as slash paths relative to the
	// repository root.
	source string
	target string
}

func (e archExtensionEdge) String() string {
	return fmt.Sprintf("%s line %d: %s imports %s from module %s", e.file, e.line, e.source, e.importPath, e.target)
}

// TestArchExtensionModulesDependOnlyOnSharedVocabulary scans every production Go
// file beneath extensions/ and fails on any import that crosses from one
// capability plugin module into another.
//
// The scan is by syntax rather than by build closure. tests/architecture is part
// of the root module and must not import anything beneath extensions/ or
// transport/ (see TestArchCorePackagesDoNotImportTransportStacks and
// TestArchCoreDependencyClosureExcludesOptionalStacks), so it cannot reason
// about these modules by type. It does not need to: the question is which module
// a file's import path names, which the path answers directly. An alias cannot
// hide the edge, because the path is still written out -- that is the difference
// from a guard that must resolve a qualifier before it recognises a call.
//
// The closure guards in dependencies_test.go do not cover this. They ask whether
// an extension module compiles in a transport, which says nothing about an
// extension compiling in another extension: both endpoints are protocol-neutral
// and both closures stay clean, while the module is no longer independently
// selectable or independently publishable.
//
// Test files are deliberately out of scope, and coordination/placement's tests
// are the case that makes the exclusion a decision rather than an oversight:
// they drive the real Redis locker, so they hold the one sibling import this
// guard would otherwise catch. Of the two reasons the rule exists, only one
// reaches a test file. Nothing is smuggled into an application -- a test binary
// builds no deployment, so no Definition anyone declined can arrive through it,
// and that is the reason the rule is mainly about. The publishability reason does
// reach it: the manifest cannot require the sibling either, so `go test` on the
// published module would not resolve. That is a real debt, and it cannot be paid
// by widening this guard: the fix is a require line, which no manifest here may
// carry until core has a release tag, and the alternative -- swapping the real
// store for a double -- would delete the coverage of the Lua owner comparison
// that the placement design names as the thing that must not regress. Coverage
// of a real backend is worth more than uniformity here, so the edge stays and is
// revisited when a require line becomes possible.
func TestArchExtensionModulesDependOnlyOnSharedVocabulary(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	moduleRoots := archExtensionModulePaths(t)
	allowed := make(map[string]bool, len(archExtensionSharedVocabularyModules))
	for _, module := range archExtensionSharedVocabularyModules {
		allowed[module] = true
	}

	fset := token.NewFileSet()
	scanned := 0
	reached := make(map[string]int, len(allowed))
	for _, name := range archAllProductionGoFiles(t, repositoryRoot) {
		if !strings.HasPrefix(name, "extensions/") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, filepath.Join(repositoryRoot, filepath.FromSlash(name)), nil, parser.SkipObjectResolution)
		require.NoError(t, err, "failed to parse %s", name)

		for _, edge := range archExtensionCrossModuleEdges(moduleRoots, name, file, fset) {
			if allowed[edge.target] {
				reached[edge.target]++
				continue
			}
			assert.Fail(t, "capability plugin module depends on a sibling capability", "%s", edge.String())
		}
	}

	// The rule above is an absence, so it is only evidence once the scan is
	// shown to have read the namespace and to have recognised real edges in it.
	assert.GreaterOrEqual(t, scanned, 80,
		"only %d production Go file(s) beneath extensions/ were scanned; the namespace holds far more, so this guard read less than it claims", scanned)
	assert.NotEmpty(t, reached,
		"the scan found no cross-module import beneath extensions/ at all; every capability contributes a health probe, so an empty result means the edge detector stopped recognising imports rather than that the namespace became edge-free")
}

// TestArchExtensionSharedVocabularyIsExercised keeps the exemption list from
// accumulating entries nothing depends on. An unexercised member is an
// exemption granted in advance: it widens what the guard above permits while
// proving nothing, and the next capability module to reach for it inherits the
// permission without anyone deciding to give it.
func TestArchExtensionSharedVocabularyIsExercised(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	moduleRoots := archExtensionModulePaths(t)
	declared := make(map[string]bool, len(moduleRoots))
	for _, module := range moduleRoots {
		declared[module] = true
	}

	fset := token.NewFileSet()
	reached := make(map[string]bool, len(archExtensionSharedVocabularyModules))
	for _, name := range archAllProductionGoFiles(t, repositoryRoot) {
		if !strings.HasPrefix(name, "extensions/") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(repositoryRoot, filepath.FromSlash(name)), nil, parser.SkipObjectResolution)
		require.NoError(t, err, "failed to parse %s", name)
		for _, edge := range archExtensionCrossModuleEdges(moduleRoots, name, file, fset) {
			reached[edge.target] = true
		}
	}

	for _, module := range archExtensionSharedVocabularyModules {
		assert.Truef(t, declared[module],
			"%s is exempted from the capability-isolation rule but is not an extension module; a stale path silently exempts nothing and hides the rename that caused it", module)
		assert.Truef(t, reached[module],
			"%s is exempted from the capability-isolation rule but no extension module imports it in production; remove the entry or the exemption stands unearned", module)
	}
}

// TestArchExtensionEdgeDetection is the positive control. A detector that
// reported nothing would pass a clean namespace and a violated one alike, so
// each shape the guard depends on is driven over synthetic source: the sibling
// import it exists to catch, the same import written under an alias and one
// level deeper, and the three shapes that must keep working -- a module
// importing itself from its own autoload adapter, a shared vocabulary, and core.
func TestArchExtensionEdgeDetection(t *testing.T) {
	t.Parallel()
	moduleRoots := []string{
		"extensions/coordination/lease",
		"extensions/jobs/cron",
		"extensions/reliability/health",
		"extensions/storage/redis",
	}

	for name, testCase := range map[string]struct {
		file   string
		source string
		want   []string
	}{
		"the regression this guard exists for": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import redislocker "github.com/xbcio/xbc/extensions/storage/redis"
var _ = redislocker.NewLocker
`,
			want: []string{"extensions/storage/redis"},
		},
		"a subpackage of a sibling counts as the sibling": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import _ "github.com/xbcio/xbc/extensions/storage/redis/autoload"
`,
			want: []string{"extensions/storage/redis"},
		},
		"a dot import cannot hide the path": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import . "github.com/xbcio/xbc/extensions/storage/redis"
`,
			want: []string{"extensions/storage/redis"},
		},
		"an autoload adapter importing its own module is not an edge": {
			file: "extensions/jobs/cron/autoload/register.go",
			source: `package autoload
import "github.com/xbcio/xbc/extensions/jobs/cron"
var _ = cron.Bundle
`,
		},
		"the lease contract is reported so the allowlist can accept it": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import "github.com/xbcio/xbc/extensions/coordination/lease"
var _ lease.Locker
`,
			want: []string{"extensions/coordination/lease"},
		},
		"core and third-party imports are not this guard's business": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import (
	redis "github.com/redis/go-redis/v9"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/log"
)

var (
	_ = redis.NewClient
	_ plugin.Key
	_ log.Logger
)
`,
		},
		"a path merely prefixed by the namespace is not an extension module": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import _ "github.com/xbcio/xbc/extensionsx/storage/redis"
`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, fset := archParseSyntheticFile(t, testCase.source)
			var targets []string
			for _, edge := range archExtensionCrossModuleEdges(moduleRoots, testCase.file, file, fset) {
				targets = append(targets, edge.target)
			}
			assert.Equal(t, testCase.want, targets)
		})
	}
}

// TestArchExtensionModuleOwnership pins the path arithmetic the detector rests
// on. Nesting is what makes it non-obvious: extensions/coordination holds two
// modules and is not one itself, so a resolver that stopped at the first path
// segment beneath the namespace would call lease and placement the same module
// and go blind to every edge between them.
func TestArchExtensionModuleOwnership(t *testing.T) {
	t.Parallel()
	moduleRoots := []string{
		"extensions/coordination/lease",
		"extensions/coordination/placement",
		"extensions/jobs/cron",
	}

	for _, testCase := range []struct {
		path string
		want string
	}{
		{path: "extensions/jobs/cron", want: "extensions/jobs/cron"},
		{path: "extensions/jobs/cron/autoload", want: "extensions/jobs/cron"},
		{path: "extensions/coordination/lease", want: "extensions/coordination/lease"},
		{path: "extensions/coordination/placement/autoload", want: "extensions/coordination/placement"},
		{path: "extensions/coordination", want: ""},
		{path: "extensions", want: ""},
		{path: "extensions/jobs/cronx", want: ""},
	} {
		assert.Equalf(t, testCase.want, archExtensionOwningModule(moduleRoots, testCase.path),
			"owning module of %s", testCase.path)
	}
}

// archExtensionCrossModuleEdges reports every import in one production file that
// leaves the file's own extension module and lands in another one. Imports of
// core, of a transport, and of third-party code are not its concern -- other
// guards own those directions -- and neither is an import that stays inside the
// module, which is how an autoload adapter reaches its own parent.
//
// It is a pure function over a parsed file and a list of module paths so that
// the repository scan and its controls judge the same code, and so that the
// controls can present module layouts and imports the repository does not have.
func archExtensionCrossModuleEdges(moduleRoots []string, name string, file *ast.File, fset *token.FileSet) []archExtensionEdge {
	source := archExtensionOwningModule(moduleRoots, path.Dir(name))
	if source == "" {
		return nil
	}

	var edges []archExtensionEdge
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil || !archPathAtOrBelow(importPath, archExtensionsNamespace) {
			continue
		}
		target := archExtensionOwningModule(moduleRoots, strings.TrimPrefix(importPath, archRootPackage+"/"))
		if target == "" || target == source {
			continue
		}
		edges = append(edges, archExtensionEdge{
			file:       name,
			line:       fset.Position(specification.Pos()).Line,
			importPath: importPath,
			source:     source,
			target:     target,
		})
	}
	return edges
}

// archExtensionOwningModule returns the module in moduleRoots that owns a
// slash-separated repository path, or "" when no module does. The longest match
// wins, because a module may be nested below another directory that also carries
// a manifest.
func archExtensionOwningModule(moduleRoots []string, candidate string) string {
	owner := ""
	for _, module := range moduleRoots {
		if archPathAtOrBelow(candidate, module) && len(module) > len(owner) {
			owner = module
		}
	}
	return owner
}

// archExtensionModulePaths returns every module beneath extensions/ as a
// slash-separated path relative to the repository root. Discovery is delegated
// to archExtensionModuleRoots so that this guard and the plugin-implementation
// guards can never disagree about which modules exist.
func archExtensionModulePaths(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	roots := archExtensionModuleRoots(t, filepath.Join(repositoryRoot, "extensions"))
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, archRepositoryRelative(t, root))
	}
	sort.Strings(paths)
	require.Greater(t, len(paths), 1,
		"the extensions namespace resolved to %d module(s), so a cross-module rule cannot be violated and cannot be tested", len(paths))
	return paths
}
