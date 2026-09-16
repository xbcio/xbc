package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archRuntimeContextOwner is the one production file allowed to build a plugin
// Context: the runtime adapter that also owns the RuntimeHost behind it.
const archRuntimeContextOwner = "runtime/execute.go"

// TestArchPluginContextIsBuiltOnlyByTheRuntime parses every production Go file
// in the repository and reports each plugin.NewRuntimeContext call site. That
// constructor is exported because assembly and tests must be able to build a
// Context, but it also accepts any RuntimeHost: a plugin calling it could hand
// itself a substitute host and run lifecycle work outside the runtime that owns
// its execution context. Plugins receive a Context from lifecycle methods
// instead, and this guard is what keeps that documented rule checkable.
func TestArchPluginContextIsBuiltOnlyByTheRuntime(t *testing.T) {
	root := archRepositoryRoot(t)
	fset := token.NewFileSet()
	var callers []string

	for _, name := range archAllProductionGoFiles(t, root) {
		file, err := parser.ParseFile(fset, filepath.Join(root, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err, "failed to parse %s", name)

		assert.Falsef(t, archDotImports(file, archPluginImportPath),
			"%s dot-imported %s; the guard on plugin Context construction cannot resolve calls through a dot import",
			name, archPluginImportPath)

		// Inside the plugin package itself the constructor is called without a
		// qualifier, so the shared call matcher cannot see it.
		local := filepath.Dir(name) == "plugin"
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if archCallMatches(file, call, archPluginImportPath, "NewRuntimeContext") {
				callers = append(callers, name)
				return true
			}
			identifier, ok := archUnwrapExpression(call.Fun).(*ast.Ident)
			if local && ok && identifier.Name == "NewRuntimeContext" {
				callers = append(callers, name)
			}
			return true
		})
	}

	sort.Strings(callers)
	assert.Equal(t, []string{archRuntimeContextOwner}, callers,
		"plugin.NewRuntimeContext must be called by %s alone; a plugin receives its Context from lifecycle methods",
		archRuntimeContextOwner)
}

// archAllProductionGoFiles returns every production Go file in the repository
// as a slash-separated path relative to root. Directories that hold fixtures
// rather than compiled product code are skipped, so a guard built on this list
// judges only what ships.
func archAllProductionGoFiles(t *testing.T, root string) []string {
	t.Helper()
	skipped := map[string]bool{".git": true, ".claude": true, "testdata": true}

	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (skipped[name] || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	require.NoError(t, err, "failed to walk repository %s for production Go files", root)
	require.NotEmpty(t, files, "no production Go file was scanned, the guard built on this list is not effective")
	sort.Strings(files)
	return files
}
