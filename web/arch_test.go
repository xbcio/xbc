// arch_test.go
package web

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// packageJSON is the subset of `go list -json`'s output this guard reads.
// Imports/TestImports/XTestImports are each package's own *direct* import
// list -- as declared in its own source files' import blocks -- as opposed
// to Deps, which is the full transitive closure reachable from the package.
//
// This guard deliberately checks Imports/TestImports/XTestImports, not
// Deps, per package-layout design §9.1: the rule under test is "web's own
// source files must never write an import of the root package or
// internal/*", a statement about what web's authors wrote, not about what
// ends up on disk once gin or any other dependency's own transitive graph
// is flattened. Using Deps here would produce a false negative the moment
// gin (or any future dependency) happens to import something that in turn
// imports the root package -- Deps would report that unrelated edge as if
// web itself had written the import, when web's own source never did.
// Conversely, a hypothetical "web must never depend on Gin, even
// transitively" guard would need the opposite judgment (Deps, not
// Imports) -- the two kinds of guard are not interchangeable, and this
// package only ever needs the direct-import kind.
type packageJSON struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// forbiddenDirectImport reports why importing dep from web would violate a
// package-layout design §9 guard, or "" if dep is fine.
func forbiddenDirectImport(dep string) string {
	switch {
	case dep == "github.com/xbcio/xbc":
		return "web 不得反向 import 根包（design §9 guard #11）"
	case dep == "github.com/xbcio/xbc/internal" || strings.HasPrefix(dep, "github.com/xbcio/xbc/internal/"):
		return "web 不得 import internal/*（design §9 guard #10）"
	default:
		return ""
	}
}

// TestWebDoesNotDirectlyImportRootOrInternal is the package-layout design
// §9 architecture guard for the complete web module: neither web, autoload,
// nor a future subpackage's production/test sources may write an import of
// github.com/xbcio/xbc or github.com/xbcio/xbc/internal/.... See
// packageJSON's doc comment for why this reads direct imports rather than
// Deps.
func TestWebDoesNotDirectlyImportRootOrInternal(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}

	out, err := exec.Command("go", "list", "-json", "./...").Output()
	require.NoError(t, err, "go list -json ./... 失败")

	dec := json.NewDecoder(strings.NewReader(string(out)))
	checked := 0
	for dec.More() {
		var pkg packageJSON
		require.NoError(t, dec.Decode(&pkg), "解析 go list -json 输出失败")
		checked++

		checks := []struct {
			label   string
			imports []string
		}{
			{"生产代码 import", pkg.Imports},
			{"包内 _test.go import", pkg.TestImports},
			{"外部测试包 import", pkg.XTestImports},
		}
		for _, c := range checks {
			for _, dep := range c.imports {
				if reason := forbiddenDirectImport(dep); reason != "" {
					assert.Fail(t, "禁止的直接依赖",
						"%s 的%s中出现了 %q：%s", pkg.ImportPath, c.label, dep, reason)
				}
			}
		}
	}
	require.NotZero(t, checked, "go list -json ./... 没有返回任何 web package，依赖方向检查实际未生效")
}
