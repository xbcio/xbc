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
	require.NotEmpty(t, sourcePaths, "runtime/ 没有扫描到生产 Go 文件，进程设施守卫实际未生效")

	fset := token.NewFileSet()
	for _, filePath := range sourcePaths {
		relative, err := filepath.Rel(root, filePath)
		require.NoError(t, err, "计算 %s 的仓库相对路径失败", filePath)
		name := filepath.ToSlash(relative)

		file, err := parser.ParseFile(fset, filePath, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "解析 %s 失败", name)

		aliases := make(map[string]string, len(file.Imports))
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err, "解析 %s 的 import path 失败", name)
			if importPath == "os/signal" && name != owner {
				assert.Fail(t, "进程 signal owner 漂移",
					"%s import 了 os/signal；所有 signal 操作只能由 %s 拥有", name, owner)
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
						assert.Fail(t, "进程设施不得使用 dot import",
							"%s dot-import 了 %s，架构守卫无法可靠确认 owner", name, importPath)
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
				assert.Fail(t, "进程退出 hook owner 漂移",
					"%s 第 %d 行使用 %s；包级退出 hook 只能由 %s 持有和调用",
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
				assert.Fail(t, "进程设施 owner 漂移",
					"%s 第 %d 行使用 %s；os.Args、signal、os.Stderr、log.Sync 与 os.Exit 只能由 %s 拥有，App.Execute 必须可嵌入",
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
		assert.True(t, seenInOwner[label], "%s 必须继续实际持有 %s，否则 owner 守卫可能在空跑", owner, label)
	}
}
