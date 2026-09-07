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

// archWebBuiltinNames is the intentional set of Web-owned plugins compiled as
// packages of the transport/web module. Adding or removing one changes the Web
// runtime's distribution boundary and therefore requires an explicit update.
var archWebBuiltinNames = []string{
	"accesslog",
	"apikey",
	"auditlog",
	"biz",
	"cors",
	"gracefulshutdown",
	"gzip",
	"health",
	"pprof",
	"ratelimit",
	"recovery",
	"requestid",
	"securityheaders",
	"tenant",
	"timeout",
}

// archWebAdapterNames contains transport-only packages that adapt a
// protocol-neutral capability without owning a plugin Definition or autoload.
var archWebAdapterNames = []string{
	"rbac",
}

// TestArchRetiredPathsStayRetired prevents retired packages and repository
// groupings from becoming second owners beside their canonical replacements.
// Process orchestration and its private command parsing stay under the root
// runtime owner, while Plugin model, assembly, and optional autoload infrastructure
// stay together under plugin. Web built-ins and independently
// versioned integrations retain their owners.
func TestArchRetiredPathsStayRetired(t *testing.T) {
	root := archRepositoryRoot(t)
	for _, canonical := range []string{
		"runtime",
		"plugin",
		"plugin/assembly",
		"plugin/autoload",
		"plugin/model",
		"integrations",
		"transport",
		"transport/web",
		"transport/web/prelude",
		"transport/web/integrations",
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(canonical)))
		require.NoError(t, err, "canonical path %s must exist", canonical)
		require.True(t, info.IsDir(), "canonical path %s must be a directory", canonical)
	}

	retiredPaths := []string{
		"internal/runtime",
		"internal/cli",
		"assembly",
		"cli",
		"plugins",
		"plugin/catalog",
		"web",
		"topology",
		"container",
		"integration",
		"management",
		"transport/web/plugins",
		"transport/web/autoload/prelude",
		"transport/web/business",
		"internal/assembly",
		"internal/assembly/inject",
		"internal/autoload",
		"internal/container",
		"internal/inject",
		"internal/plugin",
		"internal/pluginmodel",
		"internal/report",
		"internal/startupreport",
		"internal/architecture",
	}
	for _, retired := range retiredPaths {
		retiredPath := filepath.Join(root, filepath.FromSlash(retired))
		_, err := os.Stat(retiredPath)
		if err == nil {
			t.Errorf("old path %s must not be revived; use the canonical runtime, plugin, transport, or integrations owner", retired)
			continue
		}
		require.ErrorIs(t, err, os.ErrNotExist, "checking old path %s failed", retired)
	}

	archAssertIndependentIntegrationNamespace(t, filepath.Join(root, "integrations"))
	archAssertIndependentIntegrationNamespace(t, filepath.Join(root, "transport", "web", "integrations"))

	webRoot := filepath.Join(root, "transport", "web")
	preludeRoot := filepath.Join(webRoot, "prelude")
	require.NotEmpty(t, archProductionGoFilesInDir(t, preludeRoot), "Web prelude must be a production package")
	_, err := os.Stat(filepath.Join(preludeRoot, "go.mod"))
	require.ErrorIs(t, err, os.ErrNotExist, "Web prelude must belong to the transport/web module")

	ownedPackageSet := make(map[string]bool, len(archWebBuiltinNames)+len(archWebAdapterNames))
	for _, name := range archWebBuiltinNames {
		ownedPackageSet[name] = true
		builtinRoot := filepath.Join(webRoot, name)
		info, err := os.Stat(builtinRoot)
		require.NoError(t, err, "Web built-in package %s must exist", filepath.ToSlash(filepath.Join("transport", "web", name)))
		require.True(t, info.IsDir(), "Web built-in %s must be a directory", builtinRoot)
		require.NotEmpty(t, archProductionGoFilesInDir(t, builtinRoot), "%s must contain a production Go package", builtinRoot)
		_, err = os.Stat(filepath.Join(builtinRoot, "go.mod"))
		require.ErrorIs(t, err, os.ErrNotExist, "Web built-in %s must belong to the transport/web module, not declare its own module", name)
		autoload, err := os.Stat(filepath.Join(builtinRoot, "autoload"))
		require.NoError(t, err, "Web built-in %s must provide an autoload package", name)
		require.True(t, autoload.IsDir(), "Web built-in autoload path %s must be a directory", autoload.Name())
	}
	for _, name := range archWebAdapterNames {
		ownedPackageSet[name] = true
	}

	entries, err := os.ReadDir(webRoot)
	require.NoError(t, err, "reading Web module root failed")
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "autoload" || entry.Name() == "prelude" || entry.Name() == "integrations" {
			continue
		}
		assert.Truef(t, ownedPackageSet[entry.Name()], "unexpected direct package directory transport/web/%s; declare its ownership explicitly", entry.Name())
	}
}

// archAssertIndependentIntegrationNamespace permits documentation at a module
// namespace but requires every direct child directory to be a real module.
func archAssertIndependentIntegrationNamespace(t *testing.T, namespace string) {
	t.Helper()
	entries, err := os.ReadDir(namespace)
	require.NoError(t, err, "reading integration namespace %s failed", namespace)
	modules := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			name := entry.Name()
			if name == "go.mod" || strings.HasSuffix(name, ".go") {
				t.Errorf("%s must not exist: %s is a namespace, not a Go package or module", filepath.Join(namespace, name), namespace)
			}
			continue
		}
		modules++
		manifest := filepath.Join(namespace, entry.Name(), "go.mod")
		info, err := os.Stat(manifest)
		require.NoError(t, err, "integration directory %s must be an independent module", filepath.Join(namespace, entry.Name()))
		require.False(t, info.IsDir(), "integration manifest %s must be a file", manifest)
	}
	require.NotZero(t, modules, "integration namespace %s must contain independent modules", namespace)
}

// TestArchRootPublicAPIIsFrozen keeps the application-facing facade narrow.
// Runtime and assembly implementation stay behind the application-facing
// facade; importing the root package must expose only the six entry-point symbols
// below.
//
// The set below is deliberately tiny and should stay that way: an application
// calls Run, an embedding host calls New and App.Execute, and a test supplies
// its own explicit composition through WithBundles. Adding to it is a real API decision
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
		"WithBundles",
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
