package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── guard 12: kit modules stay framework-free ──────────────────────────────

// archKitNamespace is the top-level home of framework-free libraries. kit sits
// beside extensions/ and transport/ rather than beneath either: an extension is
// a capability the framework selects at a composition root, and a transport is
// a protocol runtime, while a kit library is neither -- it is ordinary code a
// service, an extension, or an application may depend on without adopting the
// framework at all. The dependency direction is one-way: an extension may
// import a kit module, a kit module may never import anything beneath
// github.com/xbcio/xbc.
const archKitNamespace = "kit"

// TestArchKitModulesStayFrameworkFree enforces the two rules that make kit a
// boundary rather than another directory beneath extensions/:
//
//  1. Every direct child of kit/ is an independently versioned module (its own
//     go.mod) that contains production code. A shared directory whose leaves
//     were packages of the root module would publish nothing, and a consumer
//     would inherit whatever the root module happens to require.
//
//  2. No production file imports github.com/xbcio/xbc or any package beneath
//     it. This is the checkable definition of "framework-free": the module's
//     published requirement set is exactly what its consumers asked for, and a
//     kit library cannot accidentally acquire a plugin, transport, config,
//     log, or extension dependency and pass it on.
//
// The reverse direction is allowed and expected: an extension plugin may
// depend on a kit module, so a capability can share neutral code without the
// kit gaining a dependency on the capability.
func TestArchKitModulesStayFrameworkFree(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	namespace := filepath.Join(repositoryRoot, archKitNamespace)

	entries, err := os.ReadDir(namespace)
	require.NoError(t, err, "kit/ must exist: it is the home of framework-free libraries")
	require.NotEmpty(t, entries, "kit/ is empty, so the framework-free rule has nothing to govern")

	scanned := 0
	for _, entry := range entries {
		if archIgnoresDirectoryEntry(entry.Name()) {
			continue
		}
		if !entry.IsDir() {
			name := entry.Name()
			if name == "go.mod" || strings.HasSuffix(name, ".go") {
				t.Errorf("%s must not exist: kit is a namespace of independently versioned modules, not a Go package or module", filepath.Join(namespace, name))
			}
			continue
		}

		moduleRoot := filepath.Join(namespace, entry.Name())
		manifest := filepath.Join(moduleRoot, "go.mod")
		info, err := os.Stat(manifest)
		require.NoErrorf(t, err, "kit module kit/%s must be independently versioned", entry.Name())
		require.False(t, info.IsDir(), "kit module manifest %s must be a file", manifest)

		// archAllProductionGoFiles already fails when a module contains no
		// production file, which would leave the import rule below with
		// nothing to inspect while still reporting a pass.
		relativeFiles := archAllProductionGoFiles(t, moduleRoot)
		parsed := make([]*ast.File, 0, len(relativeFiles))
		fset := token.NewFileSet()
		for _, relative := range relativeFiles {
			source := filepath.Join(moduleRoot, filepath.FromSlash(relative))
			file, err := parser.ParseFile(fset, source, nil, parser.SkipObjectResolution)
			require.NoError(t, err, "failed to parse %s", source)
			parsed = append(parsed, file)
		}
		scanned++

		violations := archKitFrameworkImportViolations(parsed)
		sort.Strings(violations)
		assert.Emptyf(t, violations, "kit module kit/%s must stay framework-free: production code may not import github.com/xbcio/xbc or any package beneath it, got %v", entry.Name(), violations)
	}
	require.NotZero(t, scanned, "kit/ contains no module, so the framework-free guard proved nothing")
}

// TestArchKitFrameworkImportDetectorRejectsEveryShape is the positive control
// for the guard above, in the style of
// TestArchContractModuleDetectorRejectsEveryShape: a detector that reported
// nothing for every input would accept a clean module and a framework-coupled
// one alike, so each shape the guard claims to catch -- and the lookalike that
// must stay accepted -- is driven over synthetic source here.
func TestArchKitFrameworkImportDetectorRejectsEveryShape(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		source     string
		violations []string
	}{
		"standard library and third-party imports only": {
			source: `package fixture

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
)

var (
	_ = context.Background
	_ = json.Unmarshal
	_ = redis.NewClient
)
`,
		},
		"a kit module importing the framework root": {
			source: `package fixture

import "github.com/xbcio/xbc"

var _ = xbc.New
`,
			violations: []string{"imports github.com/xbcio/xbc"},
		},
		"a kit module importing a framework package": {
			source: `package fixture

import "github.com/xbcio/xbc/plugin"

var _ xbcplugin.Definition
`,
			violations: []string{"imports github.com/xbcio/xbc/plugin"},
		},
		"a deeper framework package under an alias": {
			source: `package fixture

import storage "github.com/xbcio/xbc/extensions/storage/redis"

var _ = storage.NewClient
`,
			violations: []string{"imports github.com/xbcio/xbc/extensions/storage/redis"},
		},
		"a dot import cannot hide the path": {
			source: `package fixture

import . "github.com/xbcio/xbc/log"

var _ Logger
`,
			violations: []string{"imports github.com/xbcio/xbc/log"},
		},
		"a path merely prefixed by the module path is not a framework import": {
			source: `package fixture

import _ "github.com/xbcio/xbcx/plugin"
`,
		},
		"a path containing the module path is not a framework import": {
			source: `package fixture

import _ "example.com/vendor/github.com/xbcio/xbc/plugin"
`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", testCase.source, parser.SkipObjectResolution)
			require.NoError(t, err, "parsing the fixture source failed")

			violations := archKitFrameworkImportViolations([]*ast.File{file})
			if len(testCase.violations) == 0 {
				assert.Emptyf(t, violations, "a framework-free kit module must be accepted, got %v", violations)
				return
			}
			require.NotEmptyf(t, violations, "the guard failed to reject a kit module that imports the framework")
			for _, expected := range testCase.violations {
				assert.Containsf(t, strings.Join(violations, "\n"), expected,
					"violation report must name %q, got %v", expected, violations)
			}
		})
	}
}

// archKitFrameworkImportViolations reports every import in files that names
// github.com/xbcio/xbc or a package beneath it.
//
// It is a pure function over parsed files so that the repository guard and its
// positive control judge the same code, and so that the control can present
// imports the repository does not have.
func archKitFrameworkImportViolations(files []*ast.File) []string {
	var violations []string
	for _, file := range files {
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				continue
			}
			if importPath == archRootPackage || strings.HasPrefix(importPath, archRootPackage+"/") {
				violations = append(violations, "imports "+importPath)
			}
		}
	}
	return violations
}
