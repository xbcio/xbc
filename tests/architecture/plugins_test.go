package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	archPluginImportPath       = "github.com/xbcio/xbc/plugin"
	archInternalAutoloadPath   = "github.com/xbcio/xbc/internal/autoload"
	archRetiredCatalogPath     = "github.com/xbcio/xbc/plugin/catalog"
	archRepositoryImportPrefix = "github.com/xbcio/xbc/"
)

type archTopValue struct {
	file        *ast.File
	initializer ast.Expr
	kind        token.Token
}

type archFunction struct {
	file        *ast.File
	declaration *ast.FuncDecl
}

// TestArchPluginImplementationsExposeCanonicalComposition keeps every reusable
// implementation on the one public composition model: a package-level
// canonical Definition handle, a trivial Definition accessor, and a
// side-effect-free Bundle accessor. Config, concrete implementation, and New
// are intentionally not uniform requirements; not every Plugin needs them.
func TestArchPluginImplementationsExposeCanonicalComposition(t *testing.T) {
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		implementationRoot := implementationRoot
		t.Run(filepath.ToSlash(implementationRoot), func(t *testing.T) {
			files := archParseProductionGoFiles(t, implementationRoot)
			require.NotEmpty(t, files, "%s must contain production Go files", implementationRoot)

			topValues := archCollectTopValues(files)
			canonical := make(map[string]bool)
			for name, value := range topValues {
				if value.kind == token.VAR && archIsDefinitionConstructor(value.file, value.initializer) {
					canonical[name] = true
				}
			}
			require.NotEmpty(t, canonical, "%s must declare a package-level plugin.Define* handle", implementationRoot)

			var definitionAccessors []*ast.FuncDecl
			var bundleAccessors []archFunction
			for _, file := range files {
				for _, declaration := range file.Decls {
					function, ok := declaration.(*ast.FuncDecl)
					if !ok || function.Recv != nil {
						continue
					}
					switch {
					case function.Name.Name == "Definition" && archReturnsType(file, function, archPluginImportPath, "Definition"):
						definitionAccessors = append(definitionAccessors, function)
					case function.Name.Name == "Bundle" && archReturnsType(file, function, archPluginImportPath, "Bundle"):
						bundleAccessors = append(bundleAccessors, archFunction{file: file, declaration: function})
					}
				}
			}

			require.Len(t, definitionAccessors, 1, "%s must expose exactly one Definition() plugin.Definition accessor", implementationRoot)
			returned, ok := archSingleReturnedExpression(definitionAccessors[0])
			require.True(t, ok, "%s Definition() must contain only one return statement", implementationRoot)
			identifier, ok := returned.(*ast.Ident)
			require.True(t, ok, "%s Definition() must return a package-level identifier, never call plugin.Define*", implementationRoot)
			assert.Truef(t, canonical[identifier.Name], "%s Definition() returns %q, which is not a package-level plugin.Define* handle", implementationRoot, identifier.Name)

			require.Len(t, bundleAccessors, 1, "%s must expose exactly one Bundle() plugin.Bundle accessor", implementationRoot)
			bundleExpression, ok := archSingleReturnedExpression(bundleAccessors[0].declaration)
			require.True(t, ok, "%s Bundle() must contain only one return statement", implementationRoot)
			assert.Truef(t, archIsSideEffectFreeBundleExpression(bundleAccessors[0].file, bundleExpression, topValues, make(map[string]bool)), "%s Bundle() must only return a package-level Bundle or compose canonical handles", implementationRoot)
		})
	}
}

// TestArchPluginAutoloadPackagesAreLeafAdapters keeps import-time mutation out
// of implementation packages. Each optional leaf adapter has exactly one init,
// and that init can only declare its parent's canonical Bundle.
func TestArchPluginAutoloadPackagesAreLeafAdapters(t *testing.T) {
	root := archRepositoryRoot(t)
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		parentImport := archRepositoryImportPrefix + filepath.ToSlash(mustRelativePath(t, root, implementationRoot))
		archAssertAutoloadAdapter(t, filepath.Join(implementationRoot, "autoload"), parentImport)
	}

	// The Web aggregate adapter intentionally declares the side-effect-free
	// Prelude Bundle rather than the server package's narrower Bundle.
	archAssertAutoloadAdapter(
		t,
		filepath.Join(root, "transport", "web", "autoload"),
		"github.com/xbcio/xbc/transport/web/prelude",
	)
}

func archAssertAutoloadAdapter(t *testing.T, directory, parentImport string) {
	t.Helper()
	t.Run(filepath.ToSlash(directory), func(t *testing.T) {
		files := archParseProductionGoFiles(t, directory)
		require.NotEmpty(t, files, "%s must provide an explicit autoload package", directory)

		initCount := 0
		for _, file := range files {
			for _, specification := range file.Imports {
				importPath, err := strconv.Unquote(specification.Path.Value)
				require.NoError(t, err)
				assert.Containsf(t, []string{archInternalAutoloadPath, parentImport}, importPath, "%s leaf adapter may import only internal/autoload and its parent package", directory)
				if specification.Name != nil {
					assert.NotEqual(t, "_", specification.Name.Name, "%s must call the parent Bundle explicitly, not blank-import it", directory)
				}
			}
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.FuncDecl:
					if declaration.Recv != nil || declaration.Name.Name != "init" {
						assert.Failf(t, "autoload implementation logic", "%s may contain no production function except init", directory)
						continue
					}
					initCount++
					assert.Truef(t, archIsCanonicalAutoloadInit(file, declaration, parentImport), "%s init must contain only internal/autoload.Declare(parent.Bundle())", directory)
				case *ast.GenDecl:
					if declaration.Tok != token.IMPORT {
						assert.Failf(t, "autoload state", "%s may contain no production declarations except imports and init", directory)
					}
				}
			}
		}
		assert.Equal(t, 1, initCount, "%s must contain exactly one init adapter", directory)
	})
}

// TestArchAutoloadMutationIsConfined keeps the process-global optional adapter
// private to internal/runtime and leaf autoload packages. Prelude and ordinary
// implementation packages remain safe to import and call directly.
func TestArchAutoloadMutationIsConfined(t *testing.T) {
	root := archRepositoryRoot(t)
	allowedOwners := map[string]bool{
		"internal/runtime":       true,
		"transport/web/autoload": true,
	}
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		relative, err := filepath.Rel(root, filepath.Join(implementationRoot, "autoload"))
		require.NoError(t, err)
		allowedOwners[filepath.ToSlash(relative)] = true
	}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			require.NoError(t, err)
			assert.NotEqual(t, archRetiredCatalogPath, importPath, "%s must not revive the retired public catalog", path)
			if importPath != archInternalAutoloadPath {
				continue
			}
			relative, err := filepath.Rel(root, filepath.Dir(path))
			require.NoError(t, err)
			owner := filepath.ToSlash(relative)
			assert.Truef(t, allowedOwners[owner], "%s imports internal/autoload outside a declared leaf adapter or internal/runtime", path)
		}
		return nil
	})
	require.NoError(t, err)
}

// TestArchWebPreludeIsSideEffectFree pins Prelude as ordinary composition
// content. Merely importing it must not register or activate anything.
func TestArchWebPreludeIsSideEffectFree(t *testing.T) {
	preludeRoot := filepath.Join(archRepositoryRoot(t), "transport", "web", "prelude")
	files := archParseProductionGoFiles(t, preludeRoot)
	require.NotEmpty(t, files, "Web prelude must contain production Go files")
	topValues := archCollectTopValues(files)

	var bundleAccessors []archFunction
	for _, file := range files {
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			require.NoError(t, err)
			assert.NotEqual(t, archInternalAutoloadPath, importPath, "Prelude must not mutate internal autoload")
			assert.NotEqual(t, archRetiredCatalogPath, importPath, "Prelude must not import the retired catalog")
			if specification.Name != nil {
				assert.NotEqual(t, "_", specification.Name.Name, "Prelude must compose explicit Bundles, not blank imports")
			}
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			assert.NotEqual(t, "init", function.Name.Name, "Prelude must have no import-time side effects")
			if function.Recv == nil && function.Name.Name == "Bundle" && archReturnsType(file, function, archPluginImportPath, "Bundle") {
				bundleAccessors = append(bundleAccessors, archFunction{file: file, declaration: function})
			}
		}
	}
	require.Len(t, bundleAccessors, 1, "Prelude must expose exactly one Bundle() plugin.Bundle")
	expression, ok := archSingleReturnedExpression(bundleAccessors[0].declaration)
	require.True(t, ok, "Prelude Bundle() must contain only one return statement")
	assert.True(t, archIsSideEffectFreeBundleExpression(bundleAccessors[0].file, expression, topValues, make(map[string]bool)), "Prelude Bundle() may only combine explicit child Bundles")
}

// TestArchPluginImplementationsDocumentUsage keeps package documentation useful
// as the Plugin ecosystem grows. Every reusable implementation must keep a
// discoverable doc.go package comment with a Usage section and code block.
func TestArchPluginImplementationsDocumentUsage(t *testing.T) {
	for _, moduleRoot := range archPluginImplementationRoots(t) {
		t.Run(filepath.ToSlash(moduleRoot), func(t *testing.T) {
			docPath := filepath.Join(moduleRoot, "doc.go")
			source, err := os.ReadFile(docPath)
			require.NoError(t, err, "%s must provide doc.go", moduleRoot)

			file, err := parser.ParseFile(token.NewFileSet(), docPath, source, parser.ParseComments|parser.SkipObjectResolution)
			require.NoError(t, err, "parse %s", docPath)
			require.NotNil(t, file.Doc, "%s must contain the package comment", docPath)

			usageFound, codeFound := archDocUsage(file.Doc)
			assert.Truef(t, usageFound, "%s package comment must contain a '# Usage' heading", docPath)
			assert.Truef(t, codeFound, "%s '# Usage' section must contain an indented Go code block", docPath)
		})
	}
}

// TestArchPluginMigrationInventoryIsCurrentAndZero turns the generated AST
// inventory into a permanent acceptance gate rather than a one-off migration
// report. It checks both checked-artifact drift and the zero-blocker invariant.
func TestArchPluginMigrationInventoryIsCurrentAndZero(t *testing.T) {
	root := archRepositoryRoot(t)
	command := exec.Command("go", "run", "./scripts/plugin-migration-inventory", "-check", "-require-zero")
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "plugin migration inventory check failed:\n%s", output)
}

func archDocUsage(group *ast.CommentGroup) (usageFound bool, codeFound bool) {
	inUsage := false
	for _, comment := range group.List {
		if !strings.HasPrefix(comment.Text, "//") {
			continue
		}
		line := strings.TrimPrefix(comment.Text, "//")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			inUsage = trimmed == "# Usage"
			usageFound = usageFound || inUsage
			continue
		}
		if inUsage && strings.HasPrefix(line, "\t") && trimmed != "" {
			codeFound = true
		}
	}
	return usageFound, codeFound
}

func archPluginImplementationRoots(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	webRoot := filepath.Join(repositoryRoot, "transport", "web")
	roots := make([]string, 0, len(archWebBuiltinNames))
	for _, name := range archWebBuiltinNames {
		roots = append(roots, filepath.Join(webRoot, name))
	}

	for _, namespace := range []string{
		filepath.Join(repositoryRoot, "integrations"),
		filepath.Join(webRoot, "integrations"),
	} {
		entries, err := os.ReadDir(namespace)
		require.NoError(t, err, "discover integration modules beneath %s", namespace)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			root := filepath.Join(namespace, entry.Name())
			info, err := os.Stat(filepath.Join(root, "go.mod"))
			require.NoError(t, err, "integration %s must declare its module", root)
			require.False(t, info.IsDir(), "integration manifest for %s must be a file", root)
			roots = append(roots, root)
		}
	}
	require.Len(t, roots, 32, "expected 15 Web built-ins and 17 independent integrations")
	sort.Strings(roots)
	return roots
}

func archParseProductionGoFiles(t *testing.T, directory string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(directory)
	require.NoError(t, err, "read Go package %s", directory)
	files := make([]*ast.File, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, file)
	}
	return files
}

func archCollectTopValues(files []*ast.File) map[string]archTopValue {
	values := make(map[string]archTopValue)
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR && general.Tok != token.CONST {
				continue
			}
			for _, raw := range general.Specs {
				specification, ok := raw.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range specification.Names {
					if index < len(specification.Values) {
						values[name.Name] = archTopValue{file: file, initializer: specification.Values[index], kind: general.Tok}
					} else if len(specification.Values) == 1 {
						values[name.Name] = archTopValue{file: file, initializer: specification.Values[0], kind: general.Tok}
					}
				}
			}
		}
	}
	return values
}

func archIsDefinitionConstructor(file *ast.File, expression ast.Expr) bool {
	call, ok := archUnwrapExpression(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	for _, name := range []string{"Define", "DefineConfigured", "DefinePlanned"} {
		if archCallMatches(file, call, archPluginImportPath, name) {
			return true
		}
	}
	return false
}

func archReturnsType(file *ast.File, function *ast.FuncDecl, importPath, name string) bool {
	return function.Type.Results != nil && len(function.Type.Results.List) == 1 && archTypeMatches(file, function.Type.Results.List[0].Type, importPath, name)
}

func archTypeMatches(file *ast.File, expression ast.Expr, importPath, name string) bool {
	expression = archUnwrapExpression(expression)
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && archImportAliases(file, importPath)[identifier.Name]
}

func archSingleReturnedExpression(function *ast.FuncDecl) (ast.Expr, bool) {
	if function.Body == nil || len(function.Body.List) != 1 {
		return nil, false
	}
	statement, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(statement.Results) != 1 {
		return nil, false
	}
	return archUnwrapExpression(statement.Results[0]), true
}

func archIsSideEffectFreeBundleExpression(file *ast.File, expression ast.Expr, topValues map[string]archTopValue, seen map[string]bool) bool {
	expression = archUnwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.Ident:
		if seen[expression.Name] {
			return false
		}
		value, ok := topValues[expression.Name]
		if !ok || value.kind != token.VAR || value.initializer == nil {
			return false
		}
		seen[expression.Name] = true
		return archIsSideEffectFreeBundleExpression(value.file, value.initializer, topValues, seen)
	case *ast.CallExpr:
		if archCallMatches(file, expression, archPluginImportPath, "BundleOf") || archCallMatches(file, expression, archPluginImportPath, "CombineBundles") {
			for _, argument := range expression.Args {
				if !archIsSideEffectFreeBundleArgument(file, argument, topValues, seen) {
					return false
				}
			}
			return true
		}
		// Prelude composes child package Bundle accessors directly.
		selector, ok := archUnwrapExpression(expression.Fun).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Bundle" || len(expression.Args) != 0 {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		return ok && archImportsRepositoryPackage(file, identifier.Name)
	default:
		return false
	}
}

func archIsSideEffectFreeBundleArgument(file *ast.File, expression ast.Expr, topValues map[string]archTopValue, seen map[string]bool) bool {
	expression = archUnwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.Ident:
		value, ok := topValues[expression.Name]
		if !ok || value.initializer == nil {
			return false
		}
		if archIsDefinitionConstructor(value.file, value.initializer) {
			return true
		}
		if archCallMatchesExpression(value.file, value.initializer, archPluginImportPath, "BundleOf") || archCallMatchesExpression(value.file, value.initializer, archPluginImportPath, "CombineBundles") {
			return archIsSideEffectFreeBundleExpression(value.file, expression, topValues, seen)
		}
		return false
	case *ast.CallExpr:
		if archCallMatches(file, expression, archPluginImportPath, "BundleOf") || archCallMatches(file, expression, archPluginImportPath, "CombineBundles") {
			for _, argument := range expression.Args {
				if !archIsSideEffectFreeBundleArgument(file, argument, topValues, seen) {
					return false
				}
			}
			return true
		}
		if identifier, ok := archUnwrapExpression(expression.Fun).(*ast.Ident); ok && identifier.Name == "Definition" && len(expression.Args) == 0 {
			return true
		}
		selector, ok := archUnwrapExpression(expression.Fun).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Bundle" || len(expression.Args) != 0 {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		return ok && archImportsRepositoryPackage(file, identifier.Name)
	default:
		return false
	}
}

func archCallMatchesExpression(file *ast.File, expression ast.Expr, importPath, name string) bool {
	call, ok := archUnwrapExpression(expression).(*ast.CallExpr)
	return ok && archCallMatches(file, call, importPath, name)
}

func archCallMatches(file *ast.File, call *ast.CallExpr, importPath, name string) bool {
	if file == nil {
		return false
	}
	function := archUnwrapExpression(call.Fun)
	switch function := function.(type) {
	case *ast.IndexExpr:
		call = &ast.CallExpr{Fun: function.X}
		return archCallMatches(file, call, importPath, name)
	case *ast.IndexListExpr:
		call = &ast.CallExpr{Fun: function.X}
		return archCallMatches(file, call, importPath, name)
	case *ast.SelectorExpr:
		identifier, ok := function.X.(*ast.Ident)
		return ok && function.Sel.Name == name && archImportAliases(file, importPath)[identifier.Name]
	case *ast.Ident:
		return function.Name == name && archDotImports(file, importPath)
	}
	return false
}

func archIsCanonicalAutoloadInit(file *ast.File, function *ast.FuncDecl, parentImport string) bool {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	expression, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	declare, ok := archUnwrapExpression(expression.X).(*ast.CallExpr)
	if !ok || !archCallMatches(file, declare, archInternalAutoloadPath, "Declare") || len(declare.Args) != 1 {
		return false
	}
	bundle, ok := archUnwrapExpression(declare.Args[0]).(*ast.CallExpr)
	return ok && len(bundle.Args) == 0 && archCallMatches(file, bundle, parentImport, "Bundle")
}

func archImportAliases(file *ast.File, importPath string) map[string]bool {
	aliases := make(map[string]bool)
	for _, specification := range file.Imports {
		actual, err := strconv.Unquote(specification.Path.Value)
		if err != nil || actual != importPath {
			continue
		}
		alias := filepath.Base(importPath)
		if specification.Name != nil {
			alias = specification.Name.Name
		}
		if alias != "." && alias != "_" {
			aliases[alias] = true
		}
	}
	return aliases
}

func archDotImports(file *ast.File, importPath string) bool {
	for _, specification := range file.Imports {
		actual, err := strconv.Unquote(specification.Path.Value)
		if err == nil && actual == importPath && specification.Name != nil && specification.Name.Name == "." {
			return true
		}
	}
	return false
}

func archImportsRepositoryPackage(file *ast.File, alias string) bool {
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil || !strings.HasPrefix(importPath, archRepositoryImportPrefix) {
			continue
		}
		name := filepath.Base(importPath)
		if specification.Name != nil {
			name = specification.Name.Name
		}
		if name == alias {
			return true
		}
	}
	return false
}

func archUnwrapExpression(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

func mustRelativePath(t *testing.T, base, target string) string {
	t.Helper()
	relative, err := filepath.Rel(base, target)
	require.NoError(t, err)
	return relative
}
