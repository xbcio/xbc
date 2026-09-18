package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workloadImportPrefix is where this example's workload packages live. The
// guard reads main.go's imports rather than guessing at their local names, so a
// renamed import alias is judged by the package it actually names.
const workloadImportPrefix = "github.com/xbcio/xbc/examples/workloads/internal/"

// TestWorkloadExampleNeverSelectsItsRoleInGo is what makes this example's claim
// verifiable rather than merely stated. The example exists to show one binary
// assembling different subsets of itself through placement, so a later edit that
// reaches for os.Getenv or a flag to pick a role would quietly turn it into an
// ordinary single-role service that happens to mention workloads in a comment,
// and would leave the placement path undemonstrated again.
//
// Environment variables are not forbidden in general -- they are a complete
// configuration layer, and the roles are documented as
//
//	XBC_WORKLOADS_TRANSCODE_ENABLED=true ...
//
// The rule is narrower and load-bearing: the *Go code* must not read them. The
// runtime's own configuration layer reads them and hands placement its answer,
// which is the indirection this guard protects.
func TestWorkloadExampleNeverSelectsItsRoleInGo(t *testing.T) {
	fset := token.NewFileSet()
	inspected := 0

	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr, "Parsing %s failed", path)
		inspected++

		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch qualifier.Name {
			case "os":
				assert.NotContainsf(t, []string{"Getenv", "LookupEnv", "Environ"}, selector.Sel.Name,
					"%s calls os.%s: this example's role comes from placement, which reads configuration; "+
						"reading the environment here would make the Go code the decision point instead",
					path, selector.Sel.Name)
			case "flag":
				assert.Failf(t, "flag usage in the example", "%s calls flag.%s: the example takes no role flag",
					path, selector.Sel.Name)
			}
			return true
		})
		return nil
	})

	require.NoError(t, err)
	require.NotZero(t, inspected, "no production Go file was scanned; the guard checked nothing")
}

// TestWorkloadExampleSelectsEveryDeclaredWorkload is the other half of the same
// property. A composition root that decided a role by importing only the Bundle
// it wanted would be exactly the hard-coding the previous guard forbids, only
// spelled in the import block instead of in an if. So every package beneath
// internal/ that declares a workload must be imported by the example's main
// package and selected there through a plain Bundle() call.
//
// The declared set is discovered rather than listed: a workload added to this
// example is covered by this guard the moment it exists, which is what keeps the
// guard from passing while the example drifts.
func TestWorkloadExampleSelectsEveryDeclaredWorkload(t *testing.T) {
	declared := declaredWorkloadPackages(t)
	require.NotEmpty(t, declared, "no workload-declaring package found under internal/; this guard would pass without checking anything")

	fset := token.NewFileSet()
	mainFile, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err, "Parsing main.go failed")

	aliases := importAliases(t, mainFile)
	selected, conditioning := selectedBundleQualifiers(t, mainFile)

	assert.Empty(t, conditioning,
		"main.go contains a control-flow statement; the composition root must select every Bundle unconditionally and let placement decide what is built")

	for _, workload := range declared {
		alias, imported := aliases[workload.importPath]
		if !assert.Truef(t, imported,
			"main.go does not import %s, which declares workload %q; a role is selected by configuration, never by leaving a workload out of the composition",
			workload.importPath, workload.key) {
			continue
		}
		assert.Truef(t, selected[alias],
			"main.go imports %s as %s but never calls %s.Bundle(); every declared workload is selected unconditionally",
			workload.importPath, alias, alias)
	}
}

type declaredWorkload struct {
	packageName string
	importPath  string
	key         string
}

// declaredWorkloadPackages returns every internal package that declares a
// workload.
//
// A package declares one by holding a constant of type plugin.WorkloadKey,
// which is the only shape plugin.WorkloadOf's first argument accepts. Nothing
// here reads the value the constant carries for its decision -- the declaration
// is what matters -- but the key is carried along so a failure names the
// workload rather than only a path.
func declaredWorkloadPackages(t *testing.T) []declaredWorkload {
	t.Helper()
	entries, err := os.ReadDir("internal")
	require.NoError(t, err, "reading internal/ failed")

	var found []declaredWorkload
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join("internal", entry.Name())

		names, err := os.ReadDir(directory)
		require.NoError(t, err)
		for _, name := range names {
			if name.IsDir() || !strings.HasSuffix(name.Name(), ".go") || strings.HasSuffix(name.Name(), "_test.go") {
				continue
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, name.Name()), nil, parser.SkipObjectResolution)
			require.NoError(t, parseErr)
			for _, workload := range declaredWorkloadsIn(file) {
				workload.packageName = entry.Name()
				workload.importPath = workloadImportPrefix + entry.Name()
				found = append(found, workload)
			}
		}
	}
	return found
}

// declaredWorkloadsIn returns the workloads one parsed file declares, matched on
// `const <name> plugin.WorkloadKey = "<key>"`.
func declaredWorkloadsIn(file *ast.File) []declaredWorkload {
	var found []declaredWorkload
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok || len(value.Values) != 1 || !isSelectorNamed(value.Type, "plugin", "WorkloadKey") {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			key, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr != nil {
				continue
			}
			found = append(found, declaredWorkload{key: key})
		}
	}
	return found
}

// importAliases maps each imported path to the name main.go reaches it by: an
// explicit alias when one is written, and otherwise the package's own declared
// name. Blank and dot imports produce no entry, because neither can be the
// qualifier of a Bundle() call this guard looks for.
//
// Only this example's own packages are resolved from their source. A third-party
// path's local name is read off the last path segment, which is all this guard
// needs: the names it goes on to look for are the workload packages' own.
func importAliases(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	aliases := make(map[string]string, len(file.Imports))
	for _, specification := range file.Imports {
		path, err := strconv.Unquote(specification.Path.Value)
		require.NoError(t, err, "Parsing an import path in main.go failed")

		if specification.Name != nil {
			if specification.Name.Name == "_" || specification.Name.Name == "." {
				continue
			}
			aliases[path] = specification.Name.Name
			continue
		}
		if !strings.HasPrefix(path, workloadImportPrefix) {
			aliases[path] = filepath.Base(path)
			continue
		}
		aliases[path] = packageClauseName(t, path)
	}
	return aliases
}

// packageClauseName reads the package name a local import path declares, rather
// than assuming the directory name: the two are conventionally equal and are not
// required to be.
func packageClauseName(t *testing.T, importPath string) string {
	t.Helper()
	directory := filepath.Join("internal", strings.TrimPrefix(importPath, workloadImportPrefix))
	names, err := os.ReadDir(directory)
	require.NoError(t, err, "Reading the sources of imported package %s failed", importPath)
	for _, candidate := range names {
		if candidate.IsDir() || !strings.HasSuffix(candidate.Name(), ".go") || strings.HasSuffix(candidate.Name(), "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, candidate.Name()), nil, parser.PackageClauseOnly)
		require.NoError(t, parseErr)
		return parsed.Name.Name
	}
	require.Failf(t, "package has no production source", "%s declares no production Go file", importPath)
	return ""
}

// selectedBundleQualifiers returns the qualifiers of every `<qualifier>.Bundle()`
// call in main, and a description of any control-flow statement in the file that
// could make a selection conditional. It fails the caller when main has no body
// to read.
//
// Selection is read from main's own body rather than from the whole file: a
// Bundle() call anywhere else -- a package-level list, an init, an unused
// helper -- is not a selection, because only what main hands to xbc.Run decides
// what this process can be.
func selectedBundleQualifiers(t *testing.T, file *ast.File) (map[string]bool, []string) {
	t.Helper()
	selected := make(map[string]bool)

	var statements []ast.Stmt
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "main" || function.Recv != nil || function.Body == nil {
			continue
		}
		statements = function.Body.List
		break
	}
	require.NotEmpty(t, statements, "main.go declares no main function body; this guard would pass without checking anything")
	for _, statement := range statements {
		ast.Inspect(statement, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Bundle" {
				return true
			}
			if qualifier, ok := selector.X.(*ast.Ident); ok {
				selected[qualifier.Name] = true
			}
			return true
		})
	}

	var conditioning []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.IfStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.ForStmt, *ast.RangeStmt:
			conditioning = append(conditioning, "a control-flow statement")
		}
		return true
	})
	return selected, conditioning
}

// isSelectorNamed reports whether expression is exactly qualifier.name, which is
// the shape a cross-package type reference has when it is written out in full
// rather than dot-imported.
func isSelectorNamed(expression ast.Expr, qualifier, name string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == qualifier
}
