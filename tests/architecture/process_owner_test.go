package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchProcessConcernsRemainInProcessAdapter parses every runtime-package
// production file, resolving import aliases before inspecting selectors. Test
// files are deliberately excluded. runtime/process.go is the sole owner of
// process arguments, signals, diagnostics, logger flushing, and termination;
// keeping these concerns out of App.Execute is what makes it embeddable.
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
		owner         = "runtime/process.go"
		exitHook      = "osExit"
		exitHookLabel = "package-level osExit"
	)
	seenInOwner := map[string]bool{}

	root := archRepositoryRoot(t)
	runtimeDir := filepath.Join(root, "runtime")
	var sourcePaths []string
	for _, name := range archProductionGoFilesInDir(t, runtimeDir) {
		sourcePaths = append(sourcePaths, filepath.Join(runtimeDir, name))
	}
	require.NotEmpty(t, sourcePaths, "runtime/ no production Go files scanned, process facility guard actually not effective")

	fset := token.NewFileSet()
	for _, filePath := range sourcePaths {
		relative, err := filepath.Rel(root, filePath)
		require.NoError(t, err, "failed to calculate relative path of repository for %s", filePath)
		name := filepath.ToSlash(relative)

		file, err := parser.ParseFile(fset, filePath, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "failed to parse %s", name)

		aliases := make(map[string]string, len(file.Imports))
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err, "failed to parse import path of %s", name)
			if importPath == "os/signal" && name != owner {
				assert.Fail(t, "process signal owner drift",
					"%s imported os/signal; all signal operations must be owned by %s", name, owner)
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
						assert.Fail(t, "process facility must not use dot import",
							"%s dot-imported %s, architecture guard cannot reliably confirm owner", name, importPath)
						break
					}
				}
				continue
			}
			aliases[alias] = importPath
		}

		// osExit is deliberately package-scoped so same-package tests can replace
		// process termination. Reserve that identifier to process.go throughout
		// package runtime; otherwise a sibling file could invoke or alias the
		// hook without producing an imported selector for the guard below to inspect.
		if path.Dir(name) == path.Dir(owner) && name != owner {
			ast.Inspect(file, func(node ast.Node) bool {
				ident, ok := node.(*ast.Ident)
				if !ok || ident.Name != exitHook {
					return true
				}
				assert.Fail(t, "process exit hook owner drift",
					"%s line %d uses %s; the package-level exit hook can only be owned and called by %s",
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
				assert.Fail(t, "Process facility owner drift",
					"%s line %d uses %s; os.Args, signal, os.Stderr, log.Sync and os.Exit can only be owned by %s, App.Execute must be embeddable",
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
		assert.True(t, seenInOwner[label], "%s must continue to actually hold %s, otherwise the owner guard may run empty", owner, label)
	}
}
