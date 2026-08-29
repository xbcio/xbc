package architecture_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archModuleFile mirrors the go.mod fields used by the release-boundary
// guard. Parsing through `go mod edit -json` delegates go.mod syntax and
// replace shapes to the Go tool instead of maintaining a fragile line parser.
type archModuleFile struct {
	Require []struct {
		Path    string
		Version string
	}
	Replace []json.RawMessage
}

// archRepositoryRoot locates this checked-in test source, ascends to the
// repository root, and never trusts the process working directory. Architecture
// tests are also run with
// GOWORK=off during release checks, and tooling may invoke a test binary from
// another directory, so neither cwd nor go's workspace discovery is a stable
// way to find repository manifests.
func archRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok, "无法定位 architecture 测试源码，不能可靠读取仓库")

	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	absoluteRoot, err := filepath.Abs(root)
	require.NoError(t, err, "无法将仓库根转换为绝对路径：%s", root)
	root = absoluteRoot
	for _, marker := range []string{"go.mod", "go.work"} {
		info, err := os.Stat(filepath.Join(root, marker))
		require.NoError(t, err, "architecture 测试推导的仓库根 %s 缺少 %s", root, marker)
		require.False(t, info.IsDir(), "仓库根 %s 必须是文件", marker)
	}
	return root
}

// archReadModuleFile parses one manifest with the Go tool so replace forms,
// comments and block formatting are handled by the same parser as builds.
// Passing the manifest as go mod edit's explicit target also avoids inheriting
// workspace selection from cwd or GOWORK.
func archReadModuleFile(t *testing.T, manifest string) archModuleFile {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过 module manifest 检查")
	}

	if !filepath.IsAbs(manifest) {
		manifest = filepath.Join(archRepositoryRoot(t), manifest)
	}
	manifest = filepath.Clean(manifest)
	out, err := archGoCommand(t, "mod", "edit", "-json", manifest).Output()
	require.NoError(t, err, "解析 %s 失败", manifest)

	var mod archModuleFile
	require.NoError(t, json.Unmarshal(out, &mod), "解析 %s 的 go mod edit -json 输出失败", manifest)
	return mod
}

type archWorkspaceFile struct {
	Use []struct {
		DiskPath string
	}
}

// archWorkspaceModuleFiles returns every non-root module joined by the
// checked-in repository go.work. The explicit absolute workspace path makes
// this independent of cwd and an inherited GOWORK value (including "off").
// Discovering modules keeps this publication guard effective when a grpc,
// management or integration module is added without editing this test.
func archWorkspaceModuleFiles(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	workspace := filepath.Join(repositoryRoot, "go.work")
	out, err := archGoCommand(t, "work", "edit", "-json", workspace).Output()
	require.NoError(t, err, "解析仓库 workspace %s 失败", workspace)

	var work archWorkspaceFile
	require.NoError(t, json.Unmarshal(out, &work), "解析 go work edit -json 输出失败")
	var manifests []string
	for _, use := range work.Use {
		moduleRoot := use.DiskPath
		if !filepath.IsAbs(moduleRoot) {
			moduleRoot = filepath.Join(repositoryRoot, moduleRoot)
		}
		moduleRoot = filepath.Clean(moduleRoot)
		if moduleRoot == repositoryRoot {
			continue
		}
		manifest := filepath.Join(moduleRoot, "go.mod")
		info, err := os.Stat(manifest)
		require.NoError(t, err, "workspace module %s 缺少 go.mod", moduleRoot)
		require.False(t, info.IsDir(), "workspace module manifest %s 必须是文件", manifest)
		manifests = append(manifests, manifest)
	}
	require.NotEmpty(t, manifests, "go.work 没有列出任何独立子 module，发布边界守卫实际未生效")
	sort.Strings(manifests)
	return manifests
}

// TestArchWorkspaceModuleDiscoveryIgnoresCWDAndGOWORK is the regression for
// release-mode `GOWORK=off go test -mod=readonly ./...`. Discovery must still
// inspect this repository's checked-in workspace, not ask the go command to
// discover a workspace from process state.
func TestArchWorkspaceModuleDiscoveryIgnoresCWDAndGOWORK(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	t.Setenv("GOWORK", "off")
	t.Chdir(t.TempDir())

	manifests := archWorkspaceModuleFiles(t)
	assert.Contains(t, manifests, filepath.Join(repositoryRoot, "transport", "web", "go.mod"))
	assert.Contains(t, manifests, filepath.Join(repositoryRoot, "examples", "go.mod"))
	for _, manifest := range manifests {
		assert.True(t, filepath.IsAbs(manifest), "workspace manifest 必须解析为绝对路径：%s", manifest)
	}
}

// archStableReleaseVersion accepts an exact release-tag-shaped semantic
// version only. Pseudo-versions, prereleases, build metadata and the common
// v0.0.0 placeholder are intentionally rejected. Whether that tag exists on
// the remote is then proven by the release job with GOWORK=off; an ordinary
// architecture test must not depend on network availability.
func archStableReleaseVersion(version string) bool {
	if version == "v0.0.0" || !strings.HasPrefix(version, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// TestArchWorkspaceModulesRejectLocalReplaceAndSyntheticVersions protects the
// publication contract for independently consumable modules. Missing xbc
// requirements are valid before the corresponding module has a real tag;
// once a requirement is added it must name an exact release, never a local
// replace, zero placeholder or generated pseudo-version.
func TestArchWorkspaceModulesRejectLocalReplaceAndSyntheticVersions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 命令不可用，跳过 module manifest 检查")
	}

	for _, manifest := range archWorkspaceModuleFiles(t) {
		mod := archReadModuleFile(t, manifest)
		assert.Empty(t, mod.Replace,
			"%s 不得包含 replace；仓库内联调用 go.work，发布验证必须解析真实 tag", manifest)

		for _, req := range mod.Require {
			if req.Path != "github.com/xbcio/xbc" && !strings.HasPrefix(req.Path, "github.com/xbcio/xbc/") {
				continue
			}
			assert.Truef(t, archStableReleaseVersion(req.Version),
				"%s 对 %s 的 require 必须使用真实发布 tag 形状 vX.Y.Z；禁止 v0.0.0、伪版本、预发布或占位版本，得到 %q",
				manifest, req.Path, req.Version)
		}
	}
}
