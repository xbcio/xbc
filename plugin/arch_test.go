package plugin

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
)

// listResult mirrors just the two fields of `go list -json`'s output this
// guard needs. Deps is the full transitive dependency closure of the
// package -- deliberately NOT limited to the package's own direct Imports
// -- because these checks are the "does this package ever compile something
// forbidden in" kind (package-layout design §9.1), which must be answered
// with the closure, not the direct import list.
type listResult struct {
	ImportPath string
	Deps       []string
}

func goListDeps(t *testing.T, pkg string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过依赖方向检查")
	}
	out, err := exec.Command("go", "list", "-json", pkg).Output()
	require.NoError(t, err, "go list -json %s 失败", pkg)

	var res listResult
	require.NoError(t, json.Unmarshal(out, &res), "解析 go list -json 输出失败")
	return res.Deps
}

// isStdlib approximates "is this a standard-library import path" by
// checking whether its first path segment contains a dot. Every non-stdlib
// module path in this repo's dependency graph is rooted at a domain
// (github.com/..., go.uber.org/..., golang.org/...), so a first segment
// without a dot reliably means stdlib ("fmt", "os/exec", "context", ...).
func isStdlib(pkg string) bool {
	first := pkg
	if i := strings.Index(pkg, "/"); i >= 0 {
		first = pkg[:i]
	}
	return !strings.Contains(first, ".")
}

// TestPluginPackageDependencyClosureIsClean is the automated guard for the
// package-layout design's hardest constraint on `plugin` (§6, §9 rule 2):
// it must never pull in the root facade, catalog, higher-level public owner
// packages, private implementation packages, or a specific protocol runtime.
// Any of those edges would make the protocol-neutral SPI depend upward on a
// consumer or concrete capability layer.
func TestPluginPackageDependencyClosureIsClean(t *testing.T) {
	deps := goListDeps(t, "github.com/xbcio/xbc/plugin")
	forbiddenTrees := []struct {
		prefix string
		reason string
	}{
		{"github.com/xbcio/xbc/plugin/catalog", "plugin 不得反向依赖默认目录"},
		{"github.com/xbcio/xbc/runtime", "plugin 不得反向依赖运行编排实现"},
		{"github.com/xbcio/xbc/assembly", "plugin 不得反向依赖实例装配实现"},
		{"github.com/xbcio/xbc/cli", "plugin 不得反向依赖命令解析实现"},
		{"github.com/xbcio/xbc/internal", "plugin 不得依赖 core internal 包"},
		{"github.com/xbcio/xbc/transport", "plugin 是协议无关的 SPI，不得依赖任何 transport 实现 module"},
		{"github.com/gin-gonic/gin", "plugin 是协议无关的 SPI，不得依赖 Gin"},
		{"google.golang.org/grpc", "plugin 是协议无关的 SPI，不得依赖 Google gRPC"},
	}

	for _, dep := range deps {
		require.NotEqual(t, "github.com/xbcio/xbc", dep,
			"plugin 不得依赖根门面，否则会与消费它的上层形成反向依赖")
		for _, forbidden := range forbiddenTrees {
			require.Falsef(t,
				dep == forbidden.prefix || strings.HasPrefix(dep, forbidden.prefix+"/"),
				"%s：%s", forbidden.reason, dep)
		}
	}
}

// TestPluginDependencyClosureAddsNothingBeyondConfigAndLog closes the gap the
// blacklist above structurally cannot cover. A blacklist only rejects the
// specific names it already knows, so plugin's third-party closure was free to
// grow indefinitely without any test noticing. That closure is not cosmetic:
// every package in it is a compile-time cost paid by anyone who implements the
// SPI, so it needs a ceiling, not just a list of forbidden names.
//
// The ceiling is expressed relative to config's and log's own closures -- the
// same technique the catalog guard below uses -- rather than as a frozen list
// of package paths. plugin depends on config and log by design (§6), so
// whatever those two already justify may legitimately appear here as well, and
// stating it relatively keeps the guard correct when koanf or zap change their
// own transitive dependencies. What it does catch is plugin acquiring a
// third-party dependency that neither config nor log explains: that is plugin
// reaching for something of its own, and it belongs in an implementation
// package instead.
func TestPluginDependencyClosureAddsNothingBeyondConfigAndLog(t *testing.T) {
	bases := []string{"github.com/xbcio/xbc/config", "github.com/xbcio/xbc/log"}

	allowed := make(map[string]bool)
	for _, base := range bases {
		allowed[base] = true
		for _, dep := range goListDeps(t, base) {
			allowed[dep] = true
		}
	}

	for _, dep := range goListDeps(t, "github.com/xbcio/xbc/plugin") {
		if allowed[dep] || isStdlib(dep) {
			continue
		}
		t.Errorf("plugin 引入了 config 与 log 都无法解释的第三方依赖 %q，"+
			"每个实现 SPI 的插件作者都要为它付编译代价；该依赖应留在具体能力包里", dep)
	}
}

// TestPluginCatalogDependencyClosureIsPluginAndStdlibOnly is the automated
// guard for §9 rule 2's second half: plugin/catalog may depend on `plugin`
// and the standard library, and nothing else.
//
// This is checked relative to plugin's OWN closure, not against a fixed
// allowlist of package names: plugin legitimately depends on config and
// log (§6), which in turn pull in koanf, zap, otel and friends -- all of
// that is unavoidable and allowed to show up in catalog's closure too,
// purely because catalog depends on plugin. What must never happen is
// catalog's closure containing a third-party package that ISN'T already
// somewhere in plugin's own closure -- that would mean catalog reached for
// a dependency of its own, which is exactly what "only depends on plugin
// and stdlib" forbids.
func TestPluginCatalogDependencyClosureIsPluginAndStdlibOnly(t *testing.T) {
	pluginDeps := goListDeps(t, "github.com/xbcio/xbc/plugin")
	allowed := make(map[string]bool, len(pluginDeps)+1)
	allowed["github.com/xbcio/xbc/plugin"] = true
	for _, d := range pluginDeps {
		allowed[d] = true
	}

	catalogDeps := goListDeps(t, "github.com/xbcio/xbc/plugin/catalog")
	for _, dep := range catalogDeps {
		if dep == "github.com/xbcio/xbc/plugin/catalog" {
			continue
		}
		if allowed[dep] {
			continue
		}
		if isStdlib(dep) {
			continue
		}
		t.Errorf("plugin/catalog 引入了 plugin 自身没有的第三方依赖 %q，"+
			"违反“plugin/catalog 只依赖 plugin 和标准库”的约束", dep)
	}
}

// TestArchPluginSPIHasCanonicalShape protects the final, intentionally small
// public contract. It checks only canonical fields and explicitly retired
// names rather than freezing Definition's complete field count, so adding an
// unrelated piece of static metadata does not create a false architecture
// failure.
func TestArchPluginSPIHasCanonicalShape(t *testing.T) {
	pluginType := reflect.TypeOf((*Plugin)(nil)).Elem()
	require.Equal(t, reflect.Interface, pluginType.Kind())
	assert.Zero(t, pluginType.NumMethod(), "Plugin 必须保持零方法 marker；身份只来自 Definition.Key")
	var marker Plugin = struct{}{}
	assert.IsType(t, struct{}{}, marker, "不嵌入 Base、没有方法的值也必须是合法 marker Plugin")

	definitionType := reflect.TypeOf(Definition{})
	keyField, ok := definitionType.FieldByName("Key")
	require.True(t, ok, "Definition.Key 必须存在")
	assert.Equal(t, reflect.TypeOf(Key("")), keyField.Type, "Definition.Key 必须使用命名类型 plugin.Key")

	instancesField, ok := definitionType.FieldByName("Instances")
	require.True(t, ok, "Definition.Instances 必须存在")
	assert.Equal(t, reflect.TypeOf(Cardinality(0)), instancesField.Type,
		"Definition.Instances 必须使用命名类型 plugin.Cardinality")
	_, hasLegacyName := definitionType.FieldByName("Name")
	assert.False(t, hasLegacyName, "Definition.Name 不得复活；Key 是唯一身份")

	var zero Cardinality
	assert.Equal(t, SingleInstance, zero, "SingleInstance 必须保持 Cardinality 零值")
	assert.NotEqual(t, SingleInstance, MultipleInstances, "单实例与多实例策略必须不同")

	configMethod, ok := reflect.TypeOf((*Context)(nil)).MethodByName("Config")
	require.True(t, ok, "(*plugin.Context).Config 必须存在")
	require.Equal(t, 1, configMethod.Type.NumIn(), "Context.Config 不接受额外参数")
	require.Equal(t, 1, configMethod.Type.NumOut(), "Context.Config 只返回一个只读 view")
	viewType := reflect.TypeOf((*config.View)(nil)).Elem()
	assert.Equal(t, viewType, configMethod.Type.Out(0),
		"Context.Config 必须精确返回 config.View，不能泄露可绑定、可变的 *config.Environment")
}
