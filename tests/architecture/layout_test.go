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

// archWebPackageExtensionPaths is the intentional set of lightweight Web
// plugins grouped beneath extensions while remaining packages of the
// transport/web module. Adding or removing one changes the Web runtime's
// distribution boundary and therefore requires an explicit update.
var archWebPackageExtensionPaths = []string{
	"authentication/apikey",
	"authorization/tenant",
	"observability/accesslog",
	"observability/auditlog",
	"observability/pprof",
	"observability/requestid",
	"response/biz",
	"response/gzip",
	"reliability/gracefulshutdown",
	"reliability/health",
	"reliability/ratelimit",
	"reliability/recovery",
	"reliability/timeout",
	"security/cors",
	"security/securityheaders",
}

var archProtocolNeutralExtensionGroups = []string{
	"authorization",
	"coordination",
	"storage",
	"jobs",
	"messaging",
}

// archProtocolNeutralContractModules is the intentional set of contract-only
// modules that sit directly beneath extensions rather than inside a capability
// group. A contract module owns no Definition, Config, or Bundle: it publishes
// the protocol-neutral vocabulary that transports and their plugins implement.
// It cannot live in core, because core's dependency closure must exclude
// everything beneath extensions, and it cannot be a capability group leaf,
// because it is not a plugin. Adding one is an architecture decision.
var archProtocolNeutralContractModules = []string{
	"authentication",
}

var archWebExtensionGroups = []string{
	"authentication",
	"authorization",
	"openapi",
	"observability",
	"response",
	"reliability",
	"security",
}

// TestArchRetiredPathsStayRetired prevents retired packages and repository
// groupings from becoming second owners beside their canonical replacements.
// Process orchestration and its private command parsing stay under the root
// runtime owner, while Plugin model, assembly, and optional autoload infrastructure
// stay together under plugin. Web package extensions and independently
// versioned extensions retain their capability-grouped owners. Engine adapters
// are a distinct, non-plugin category: each implements the neutral web.Engine
// port rather than a Web capability, so they sit beside extensions under their
// own transport/web/engines namespace instead of inside it.
func TestArchRetiredPathsStayRetired(t *testing.T) {
	root := archRepositoryRoot(t)
	for _, canonical := range []string{
		"extensions/authentication",
		"runtime",
		"plugin",
		"plugin/assembly",
		"plugin/autoload",
		"plugin/model",
		"extensions",
		"transport",
		"transport/web",
		"transport/web/prelude",
		"transport/web/extensions",
		"transport/web/engines",
		"transport/web/engines/gin",
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(canonical)))
		require.NoError(t, err, "canonical path %s must exist", canonical)
		require.True(t, info.IsDir(), "canonical path %s must be a directory", canonical)
	}

	retiredPaths := []string{
		"authentication",
		"authn",
		"extensions/data",
		"transport/web/extensions/presentation",
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
		"integrations",
		"management",
		"security",
		"transport/web/plugins",
		"transport/web/integrations",
		"transport/web/extensions/documentation",
		"transport/web/extensions/openapi/swagger",
		"transport/web/rbac",
		"transport/web/accesslog",
		"transport/web/apikey",
		"transport/web/auditlog",
		"transport/web/biz",
		"transport/web/cors",
		"transport/web/gracefulshutdown",
		"transport/web/gzip",
		"transport/web/health",
		"transport/web/pprof",
		"transport/web/ratelimit",
		"transport/web/recovery",
		"transport/web/requestid",
		"transport/web/securityheaders",
		"transport/web/tenant",
		"transport/web/timeout",
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
			t.Errorf("old path %s must not be revived; use the canonical extensions, runtime, plugin, or transport owner", retired)
			continue
		}
		require.ErrorIs(t, err, os.ErrNotExist, "checking old path %s failed", retired)
	}

	archAssertGroupedExtensionNamespace(t, filepath.Join(root, "extensions"), archProtocolNeutralExtensionGroups, nil, archProtocolNeutralContractModules)
	archAssertGroupedExtensionNamespace(t, filepath.Join(root, "transport", "web", "extensions"), archWebExtensionGroups, archWebPackageExtensionPaths, nil)

	webRoot := filepath.Join(root, "transport", "web")
	preludeRoot := filepath.Join(webRoot, "prelude")
	require.NotEmpty(t, archProductionGoFilesInDir(t, preludeRoot), "Web prelude must be a production package")
	_, err := os.Stat(filepath.Join(preludeRoot, "go.mod"))
	require.ErrorIs(t, err, os.ErrNotExist, "Web prelude must belong to the transport/web module")

	entries, err := os.ReadDir(webRoot)
	require.NoError(t, err, "reading Web module root failed")
	allowedDirectories := map[string]bool{
		"autoload":   true,
		"extensions": true,
		"prelude":    true,
		"engines":    true,
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		assert.Truef(t, allowedDirectories[entry.Name()], "unexpected direct package directory transport/web/%s; Web plugins belong beneath transport/web/extensions, engine adapters beneath transport/web/engines", entry.Name())
	}

	enginesRoot := filepath.Join(webRoot, "engines")
	engineEntries, err := os.ReadDir(enginesRoot)
	require.NoError(t, err, "reading Web engines root failed")
	for _, entry := range engineEntries {
		assert.Truef(t, entry.IsDir(), "unexpected non-directory transport/web/engines/%s; every engine adapter is its own directory", entry.Name())
		_, err := os.Stat(filepath.Join(enginesRoot, entry.Name(), "go.mod"))
		assert.NoErrorf(t, err, "engine adapter transport/web/engines/%s must be its own publishable module", entry.Name())
	}
}

// archAssertGroupedExtensionNamespace requires the namespace and its declared
// capability groups to remain organization-only directories. Every direct
// child of a group is either an independently versioned module or one of the
// explicitly declared package leaves owned by the parent module. A declared
// contract module is the one exception that may sit directly in the namespace
// instead of inside a group, because it publishes a shared vocabulary rather
// than a plugin.
func archAssertGroupedExtensionNamespace(t *testing.T, namespace string, expectedGroups, packageLeaves, contractModules []string) {
	t.Helper()
	entries, err := os.ReadDir(namespace)
	require.NoError(t, err, "reading extension namespace %s failed", namespace)

	contractModuleSet := make(map[string]bool, len(contractModules))
	for _, name := range contractModules {
		contractModuleSet[name] = true
	}

	var actualGroups []string
	var actualContractModules []string
	for _, entry := range entries {
		if !entry.IsDir() {
			name := entry.Name()
			if name == "go.mod" || strings.HasSuffix(name, ".go") {
				t.Errorf("%s must not exist: %s is a namespace, not a Go package or module", filepath.Join(namespace, name), namespace)
			}
			continue
		}
		if contractModuleSet[entry.Name()] {
			actualContractModules = append(actualContractModules, entry.Name())
			continue
		}
		actualGroups = append(actualGroups, entry.Name())
	}

	sort.Strings(actualContractModules)
	wantContractModules := append([]string(nil), contractModules...)
	sort.Strings(wantContractModules)
	require.Equal(t, wantContractModules, actualContractModules, "contract modules directly beneath %s changed; update the explicit ownership map", namespace)
	for _, name := range actualContractModules {
		moduleRoot := filepath.Join(namespace, name)
		require.NotEmpty(t, archProductionGoFilesInDir(t, moduleRoot), "contract module %s must contain production Go files", moduleRoot)
		info, err := os.Stat(filepath.Join(moduleRoot, "go.mod"))
		require.NoError(t, err, "contract module %s must be independently versioned", moduleRoot)
		require.False(t, info.IsDir(), "contract module manifest %s must be a file", filepath.Join(moduleRoot, "go.mod"))
	}

	sort.Strings(actualGroups)
	wantGroups := append([]string(nil), expectedGroups...)
	sort.Strings(wantGroups)
	require.Equal(t, wantGroups, actualGroups, "extension capability groups beneath %s changed; update the explicit ownership map", namespace)

	wantPackageLeaves := append([]string(nil), packageLeaves...)
	sort.Strings(wantPackageLeaves)
	packageLeafSet := make(map[string]bool, len(wantPackageLeaves))
	for _, leaf := range wantPackageLeaves {
		packageLeafSet[leaf] = true
	}
	var actualPackageLeaves []string

	for _, group := range actualGroups {
		groupRoot := filepath.Join(namespace, group)
		groupEntries, err := os.ReadDir(groupRoot)
		require.NoError(t, err, "reading extension group %s failed", groupRoot)

		leaves := 0
		for _, entry := range groupEntries {
			if !entry.IsDir() {
				name := entry.Name()
				if name == "go.mod" || strings.HasSuffix(name, ".go") {
					t.Errorf("%s must not exist: %s is a capability namespace, not a Go package or module", filepath.Join(groupRoot, name), groupRoot)
				}
				continue
			}

			leaves++
			leafRoot := filepath.Join(groupRoot, entry.Name())
			leafPath := filepath.ToSlash(filepath.Join(group, entry.Name()))
			if packageLeafSet[leafPath] {
				actualPackageLeaves = append(actualPackageLeaves, leafPath)
				require.NotEmpty(t, archProductionGoFilesInDir(t, leafRoot), "Web package extension %s must contain production Go files", leafRoot)
				_, err := os.Stat(filepath.Join(leafRoot, "go.mod"))
				require.ErrorIs(t, err, os.ErrNotExist, "Web package extension %s must belong to the transport/web module", leafPath)
				autoload, err := os.Stat(filepath.Join(leafRoot, "autoload"))
				require.NoError(t, err, "Web package extension %s must provide an autoload package", leafPath)
				require.True(t, autoload.IsDir(), "Web package extension autoload path %s must be a directory", autoload.Name())
				continue
			}

			manifest := filepath.Join(leafRoot, "go.mod")
			info, err := os.Stat(manifest)
			require.NoError(t, err, "extension leaf %s must be an independent module or an explicitly declared package extension", leafRoot)
			require.False(t, info.IsDir(), "extension manifest %s must be a file", manifest)
		}
		require.NotZero(t, leaves, "extension group %s must contain extension leaves", groupRoot)
	}
	sort.Strings(actualPackageLeaves)
	require.Equal(t, wantPackageLeaves, actualPackageLeaves, "Web package extension ownership beneath %s changed", namespace)
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
