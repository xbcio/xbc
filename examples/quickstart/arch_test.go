package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// packageJSON is the subset of `go list -json`'s output this guard reads.
//
// Imports/TestImports/XTestImports are each package's own *direct* import
// list -- what its source files literally wrote -- as opposed to Deps, the
// full transitive closure.
//
// The distinction is the entire point of this guard. What must never happen
// is an example bypassing the root application facade to import runtime,
// assembly, cli, or a core internal package directly. A transitive dependency
// could still be legitimate if the facade or another public owner used it, so
// only the direct-import lists answer the question this guard asks.
type packageJSON struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// forbiddenDirectImport reports why importing dep from an example would
// violate a package-layout design §9 guard, or "" if dep is fine.
//
// Go's internal-package rule does not protect this boundary: visibility is
// based on the import-path directory tree, not module boundaries, and this
// module's github.com/xbcio/xbc/examples/... paths are still beneath
// github.com/xbcio/xbc. The architecture guard is therefore the enforcement
// mechanism rather than a duplicate of a compiler error.
func forbiddenDirectImport(dep string) string {
	for _, forbidden := range []struct {
		prefix string
		reason string
	}{
		{"github.com/xbcio/xbc/runtime", "示例应通过根门面启动，不得直接编排 runtime"},
		{"github.com/xbcio/xbc/assembly", "示例应通过根门面启动，不得直接依赖 assembly"},
		{"github.com/xbcio/xbc/cli", "示例应通过根门面启动，不得直接调用 cli"},
		{"github.com/xbcio/xbc/internal", "示例只能使用公开 API，不得直接 import core internal/*"},
	} {
		if dep == forbidden.prefix || strings.HasPrefix(dep, forbidden.prefix+"/") {
			return forbidden.reason + "（design §9 guard #10）"
		}
	}
	return ""
}

// TestExamplesDoNotDirectlyImportCoreImplementationPackages is the
// package-layout design §9 guard #10 as it applies to the examples module: an
// example may consume the root facade, plugin-owned contracts, and optional
// modules, but this quickstart must not bypass the facade for lower-level core APIs. See
// packageJSON's doc comment for why this reads direct imports rather than Deps.
//
// It walks every package in the module, not just this one, so adding a second
// example or a helper package under examples/ is covered without touching
// this file.
func TestExamplesDoNotDirectlyImportCoreImplementationPackages(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}

	out, err := exec.Command("go", "list", "-json", "./...").Output()
	if err != nil {
		t.Fatalf("go list -json ./... 失败：%v", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	checked := 0
	for dec.More() {
		var pkg packageJSON
		if err := dec.Decode(&pkg); err != nil {
			t.Fatalf("解析 go list -json 输出失败：%v", err)
		}
		checked++

		for _, c := range []struct {
			label   string
			imports []string
		}{
			{"生产代码 import", pkg.Imports},
			{"包内 _test.go import", pkg.TestImports},
			{"外部测试包 import", pkg.XTestImports},
		} {
			for _, dep := range c.imports {
				if reason := forbiddenDirectImport(dep); reason != "" {
					t.Errorf("%s 的%s中出现了 %q：%s", pkg.ImportPath, c.label, dep, reason)
				}
			}
		}
	}

	// Without this, a `go list` that silently returned nothing -- a build
	// failure, a bad working directory -- would leave the loop body unentered
	// and the test passing while having checked no imports at all.
	if checked == 0 {
		t.Fatal("go list 没有返回任何包，依赖方向检查实际未生效")
	}
}
