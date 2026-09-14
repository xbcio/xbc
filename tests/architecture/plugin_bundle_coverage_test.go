package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchPluginBundlesCoverAllLocalCanonicalDefinitions ensures that a
// reusable plugin package cannot leave a package-level plugin.Define* handle
// unreachable from its public Bundle(). Definition() deliberately remains the
// package's primary Definition accessor; Bundle() may, and in Web and
// gracefulshutdown does, include additional independently assembled
// Definitions.
func TestArchPluginBundlesCoverAllLocalCanonicalDefinitions(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	for _, implementationRoot := range archBundleCoverageImplementationRoots(t) {
		implementationRoot := implementationRoot
		relative := filepath.ToSlash(mustRelativePath(t, repositoryRoot, implementationRoot))
		t.Run(relative, func(t *testing.T) {
			files := archParseProductionGoFiles(t, implementationRoot)
			topValues := archCollectTopValues(files)
			canonical := archBundleCoverageCanonicalDefinitions(topValues)
			require.NotEmpty(t, canonical, "%s must declare a package-level plugin.Define* handle", relative)

			bundleAccessors := archBundleCoverageAccessors(files, "Bundle", "Bundle")
			require.Len(t, bundleAccessors, 1, "%s must expose exactly one Bundle() plugin.Bundle accessor", relative)
			bundleExpression, ok := archSingleReturnedExpression(bundleAccessors[0].declaration)
			require.True(t, ok, "%s Bundle() must contain only one return statement", relative)
			require.Truef(t,
				archIsSideEffectFreeBundleExpression(bundleAccessors[0].file, bundleExpression, topValues, make(map[string]bool)),
				"%s Bundle() must use statically analyzable bundle composition", relative,
			)

			definitionFile, definitionExpression, hasDefinitionAccessor := archBundleCoverageDefinitionAccessor(files)
			covered := make(map[string]bool, len(canonical))
			archBundleCoverageCollectBundleDefinitions(
				bundleAccessors[0].file,
				bundleExpression,
				topValues,
				canonical,
				definitionFile,
				definitionExpression,
				hasDefinitionAccessor,
				covered,
				make(map[string]bool),
				make(map[string]bool),
			)

			names := make([]string, 0, len(canonical))
			for name := range canonical {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				assert.Truef(t, covered[name],
					"%s Bundle() does not reach local canonical Definition %q; every package-level plugin.Define* handle must be included in the public Bundle()",
					relative, name,
				)
			}
		})
	}
}

// archBundleCoverageImplementationRoots extends the existing reusable plugin
// leaf set with Web itself. Web is a reusable plugin implementation whose
// Bundle intentionally includes both its server and error-boundary
// Definitions, so it is the important multi-Definition control for this
// invariant.
func archBundleCoverageImplementationRoots(t *testing.T) []string {
	t.Helper()

	root := archRepositoryRoot(t)
	roots := map[string]bool{
		filepath.Join(root, "transport", "web"): true,
	}
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		roots[implementationRoot] = true
	}

	result := make([]string, 0, len(roots))
	for implementationRoot := range roots {
		result = append(result, implementationRoot)
	}
	sort.Strings(result)
	return result
}

func archBundleCoverageCanonicalDefinitions(topValues map[string]archTopValue) map[string]bool {
	canonical := make(map[string]bool)
	for name, value := range topValues {
		if value.kind == token.VAR && archIsDefinitionConstructor(value.file, value.initializer) {
			canonical[name] = true
		}
	}
	return canonical
}

func archBundleCoverageAccessors(files []*ast.File, functionName, resultName string) []archFunction {
	var accessors []archFunction
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Name.Name != functionName {
				continue
			}
			if archReturnsType(file, function, archPluginImportPath, resultName) {
				accessors = append(accessors, archFunction{file: file, declaration: function})
			}
		}
	}
	return accessors
}

func archBundleCoverageDefinitionAccessor(files []*ast.File) (*ast.File, ast.Expr, bool) {
	accessors := archBundleCoverageAccessors(files, "Definition", "Definition")
	if len(accessors) != 1 {
		return nil, nil, false
	}
	expression, ok := archSingleReturnedExpression(accessors[0].declaration)
	if !ok {
		return nil, nil, false
	}
	return accessors[0].file, expression, true
}

// archBundleCoverageCollectBundleDefinitions follows the static forms allowed
// for public Bundle composition. It only marks package-local canonical handles;
// child package Bundles and Definitions are intentionally outside this package's
// coverage obligation.
func archBundleCoverageCollectBundleDefinitions(
	file *ast.File,
	expression ast.Expr,
	topValues map[string]archTopValue,
	canonical map[string]bool,
	definitionFile *ast.File,
	definitionExpression ast.Expr,
	hasDefinitionAccessor bool,
	covered map[string]bool,
	visitingBundles map[string]bool,
	visitingDefinitions map[string]bool,
) {
	expression = archUnwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.Ident:
		value, ok := topValues[expression.Name]
		if !ok || value.kind != token.VAR || value.initializer == nil || visitingBundles[expression.Name] {
			return
		}
		visitingBundles[expression.Name] = true
		archBundleCoverageCollectBundleDefinitions(
			value.file,
			value.initializer,
			topValues,
			canonical,
			definitionFile,
			definitionExpression,
			hasDefinitionAccessor,
			covered,
			visitingBundles,
			visitingDefinitions,
		)
		delete(visitingBundles, expression.Name)
	case *ast.CallExpr:
		switch {
		case archCallMatches(file, expression, archPluginImportPath, "BundleOf"):
			for _, argument := range expression.Args {
				archBundleCoverageCollectDefinitionArgument(
					file,
					argument,
					topValues,
					canonical,
					definitionFile,
					definitionExpression,
					hasDefinitionAccessor,
					covered,
					visitingDefinitions,
				)
			}
		case archCallMatches(file, expression, archPluginImportPath, "CombineBundles"):
			for _, argument := range expression.Args {
				archBundleCoverageCollectBundleDefinitions(
					file,
					argument,
					topValues,
					canonical,
					definitionFile,
					definitionExpression,
					hasDefinitionAccessor,
					covered,
					visitingBundles,
					visitingDefinitions,
				)
			}
		}
	}
}

func archBundleCoverageCollectDefinitionArgument(
	file *ast.File,
	expression ast.Expr,
	topValues map[string]archTopValue,
	canonical map[string]bool,
	definitionFile *ast.File,
	definitionExpression ast.Expr,
	hasDefinitionAccessor bool,
	covered map[string]bool,
	visitingDefinitions map[string]bool,
) {
	expression = archUnwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.Ident:
		if canonical[expression.Name] {
			covered[expression.Name] = true
			return
		}
		value, ok := topValues[expression.Name]
		if !ok || value.kind != token.VAR || value.initializer == nil || visitingDefinitions[expression.Name] {
			return
		}
		visitingDefinitions[expression.Name] = true
		archBundleCoverageCollectDefinitionArgument(
			value.file,
			value.initializer,
			topValues,
			canonical,
			definitionFile,
			definitionExpression,
			hasDefinitionAccessor,
			covered,
			visitingDefinitions,
		)
		delete(visitingDefinitions, expression.Name)
	case *ast.CallExpr:
		if !hasDefinitionAccessor || len(expression.Args) != 0 {
			return
		}
		function, ok := archUnwrapExpression(expression.Fun).(*ast.Ident)
		if !ok || function.Name != "Definition" {
			return
		}
		archBundleCoverageCollectDefinitionArgument(
			definitionFile,
			definitionExpression,
			topValues,
			canonical,
			definitionFile,
			definitionExpression,
			hasDefinitionAccessor,
			covered,
			visitingDefinitions,
		)
	}
}
