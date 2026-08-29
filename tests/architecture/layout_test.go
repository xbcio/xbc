package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── guard 10: retired paths and semantic owners stay canonical ──────────

// archProductionGoFilesInDir returns production Go file names directly in dir.
// Every path passed here is absolute and rooted at archRepositoryRoot.
func archProductionGoFilesInDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "Reading production source code directory %s failed", dir)

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

// TestArchRetiredPathsStayRetired prevents retired packages from becoming a
// second owner beside their canonical replacements.
//
// topology moved under plugin/ordering. runtime and cli retained their package
// boundaries while moving from internal/ to the repository root; the former
// internal/container became assembly, and assembly/inject moved with it. The
// Web module moved from web/ to transport/web/, while transport/ itself remains
// a repository namespace rather than a Go package or module. Neither a former
// owner nor a package at the namespace root may coexist with the canonical
// paths. internal/architecture was a test-only package; tests/architecture is
// its replacement.
func TestArchRetiredPathsStayRetired(t *testing.T) {
	root := archRepositoryRoot(t)
	for _, canonical := range []string{"runtime", "assembly", "assembly/inject", "cli", "transport/web"} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(canonical)))
		require.NoError(t, err, "canonical package %s must exist", canonical)
		require.True(t, info.IsDir(), "canonical package %s must be a directory", canonical)
	}

	retiredPaths := []string{
		"web",
		"topology",
		"container",
		"internal/runtime",
		"internal/container",
		"internal/cli",
		"internal/inject",
		"internal/report",
		"internal/startupreport",
		"internal/architecture",
	}
	for _, retired := range retiredPaths {
		retiredPath := filepath.Join(root, filepath.FromSlash(retired))
		_, err := os.Stat(retiredPath)
		if err == nil {
			t.Errorf("old path %s must not be revived; please use current canonical owner", retired)
			continue
		}
		require.ErrorIs(t, err, os.ErrNotExist, "Checking old path %s failed", retired)
	}

	transportDir := filepath.Join(root, "transport")
	entries, err := os.ReadDir(transportDir)
	require.NoError(t, err, "Reading transport namespace failed")
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "go.mod" || strings.HasSuffix(name, ".go") {
			t.Errorf("transport/%s must not exist: transport/ is only for repository-level namespace, does not provide Go package or module", name)
		}
	}
}

// TestArchRootPublicAPIIsFrozen keeps the application-facing facade narrow.
// runtime, assembly and cli have their own package APIs, but importing the root
// package must still expose only the six entry-point symbols below.
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
		"Root package's public API has drifted; new exports must be explicit modifications to the public manifest")
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
		require.NoError(t, err, "Parsing %s failed", base)

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
	require.NotEmpty(t, names, "Root package did not resolve any exported symbols, API freeze guard actually did not take effect")
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
