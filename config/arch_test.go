package config

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigDoesNotDependOnLogOrRoot is the automated guard for this
// package's hard constraint: config must stay protocol-agnostic and
// log-independent, so it must never import the root package (that would
// create an import cycle the moment the root package imports config back)
// nor github.com/xbcio/xbc/log (log config belongs to the core lifecycle /
// future web module, not here).
//
// Uses go list -test -deps instead of a string grep: grep gets polluted by
// package names that happen to appear in comments or string constants,
// producing false positives or false negatives; go list walks the real
// compile-time import graph, and -test makes sure it does not miss an import
// that only appears in a _test.go file.
func TestConfigDoesNotDependOnLogOrRoot(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}
	out, err := exec.Command("go", "list", "-test", "-deps", "github.com/xbcio/xbc/config").Output()
	require.NoError(t, err, "go list -deps github.com/xbcio/xbc/config 失败")

	for _, line := range strings.Split(string(out), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "" || isConfigPackageItself(dep) {
			continue
		}
		assert.NotEqual(t, "github.com/xbcio/xbc", dep,
			"config 包不得 import 根包，否则会跟根包 import config 形成循环")
		assert.NotEqual(t, "github.com/xbcio/xbc/log", dep,
			"config 包不得 import log 包，log 配置归核心生命周期/未来 web module 所有")
	}
}

// isConfigPackageItself filters out the synthetic entries "go list -test
// -deps" produces for the package under test itself: the bare package name,
// the test-instrumented variant "pkg [pkg.test]", and the compiled test
// binary "pkg.test".
func isConfigPackageItself(dep string) bool {
	const self = "github.com/xbcio/xbc/config"
	switch {
	case dep == self, dep == self+".test":
		return true
	case strings.HasPrefix(dep, self+" ["), strings.HasPrefix(dep, self+"_test"):
		return true
	default:
		return false
	}
}
