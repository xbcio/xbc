package xbc

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInternalPackagesDoNotImportRootOrEachOther is the other half of the
// automated guard for Global Constraints' hard rule that internal/ stays
// pure -- the log/ half is already implemented in log/integration_test.go's
// TestLogPackageHasNoFrameworkDependency.
//
// Uses go list -test -deps instead of a string grep: grep gets polluted by
// package names that happen to appear in comments or string constants,
// producing false positives or false negatives; go list walks the real
// compile-time import graph, and -test makes sure it does not miss an import
// that only appears in a _test.go file.
func TestInternalPackagesDoNotImportRootOrEachOther(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}
	pkgs := []string{
		"github.com/xbcio/xbc/internal/graph",
		"github.com/xbcio/xbc/internal/conf",
		"github.com/xbcio/xbc/internal/inject",
	}
	for _, pkg := range pkgs {
		out, err := exec.Command("go", "list", "-test", "-deps", pkg).Output()
		require.NoError(t, err, "go list -deps %s 失败", pkg)

		for _, line := range strings.Split(string(out), "\n") {
			dep := strings.TrimSpace(line)
			if dep == "" || isInternalPackageItself(dep, pkg) {
				continue
			}
			assert.NotEqual(t, "github.com/xbcio/xbc", dep,
				"%s 不得 import 根包，否则会跟根包 import internal/* 形成循环", pkg)
			for _, other := range pkgs {
				if other == pkg {
					continue
				}
				assert.False(t, dep == other, "%s 不得 import 兄弟包 %s，三个 internal 包彼此独立", pkg, other)
			}
		}
	}
}

// isInternalPackageItself filters out the synthetic entries "go list -test
// -deps" produces for the package under test itself: the bare package name,
// the test-instrumented variant "pkg [pkg.test]", and the compiled test
// binary "pkg.test".
func isInternalPackageItself(dep, pkg string) bool {
	switch {
	case dep == pkg, dep == pkg+".test":
		return true
	case strings.HasPrefix(dep, pkg+" ["), strings.HasPrefix(dep, pkg+"_test"):
		return true
	default:
		return false
	}
}
