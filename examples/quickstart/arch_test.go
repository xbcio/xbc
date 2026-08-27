// arch_test.go
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
// The distinction is the entire point of this guard, and it is the one place
// in the repository where the two must not be confused. An example
// application legitimately *depends* on core's internal packages: it imports
// the root package, the root package imports internal/container, so
// internal/container is unavoidably in this module's Deps. That is fine and
// says nothing about the example. What must never happen is an example
// writing `import "github.com/xbcio/xbc/internal/container"` itself, reaching
// past the public API into machinery that carries no compatibility promise.
// Only the direct-import lists can tell those two situations apart; checking
// Deps here would fail on the first, which is correct behaviour, and would
// therefore have to be deleted rather than fixed.
type packageJSON struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// forbiddenDirectImport reports why importing dep from an example would
// violate a package-layout design §9 guard, or "" if dep is fine.
//
// Note that Go's own internal-package rule does not cover this case: examples
// is a separate module, and the compiler would already reject the import.
// This guard exists anyway because the compiler's rejection is a property of
// the current module layout -- move examples inside the core module for
// convenience and the language-level protection silently evaporates, while
// this test keeps failing.
func forbiddenDirectImport(dep string) string {
	const internalPrefix = "github.com/xbcio/xbc/internal"
	if dep == internalPrefix || strings.HasPrefix(dep, internalPrefix+"/") {
		return "示例只能使用公开 API，不得直接 import core 的 internal/*（design §9 guard #10）"
	}
	return ""
}

// TestExamplesDoNotDirectlyImportCoreInternal is the package-layout design §9
// guard #10 as it applies to the examples module: an example demonstrates what
// an outside user can actually write, so anything it imports must be something
// an outside user could also import. See packageJSON's doc comment for why
// this reads the direct-import lists rather than Deps.
//
// It walks every package in the module, not just this one, so adding a second
// example or a helper package under examples/ is covered without touching
// this file.
func TestExamplesDoNotDirectlyImportCoreInternal(t *testing.T) {
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
