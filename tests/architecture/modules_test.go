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
	require.True(t, ok, "Unable to locate architecture test source code, cannot reliably read repository")

	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	absoluteRoot, err := filepath.Abs(root)
	require.NoError(t, err, "Unable to convert repository root to absolute path: %s", root)
	root = absoluteRoot
	for _, marker := range []string{"go.mod", "go.work"} {
		info, err := os.Stat(filepath.Join(root, marker))
		require.NoError(t, err, "Architecture test inferred repository root %s lacks %s", root, marker)
		require.False(t, info.IsDir(), "Repository root %s must be a file", marker)
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
		t.Skip("go command unavailable, skipping module manifest check")
	}

	if !filepath.IsAbs(manifest) {
		manifest = filepath.Join(archRepositoryRoot(t), manifest)
	}
	manifest = filepath.Clean(manifest)
	out, err := archGoCommand(t, "mod", "edit", "-json", manifest).Output()
	require.NoError(t, err, "Parsing %s failed", manifest)

	var mod archModuleFile
	require.NoError(t, json.Unmarshal(out, &mod), "Failed to parse go mod edit -json output for %s", manifest)
	return mod
}

type archWorkspaceFile struct {
	Use []struct {
		DiskPath string
	}
}

// archRepositoryModuleFiles discovers every checked-in module manifest. It
// intentionally does not trust go.work: this is the source side of the guard
// that catches a newly created plugin module that was never joined to the
// workspace and would therefore be skipped by local checks and CI.
func archRepositoryModuleFiles(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	var manifests []string
	err := filepath.WalkDir(repositoryRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != repositoryRoot && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "go.mod" {
			manifests = append(manifests, filepath.Clean(path))
		}
		return nil
	})
	require.NoError(t, err, "Failed to discover repository module manifests")
	require.NotEmpty(t, manifests, "repository contains no go.mod, module coverage guard is inactive")
	sort.Strings(manifests)
	return manifests
}

// archWorkspaceModuleFiles returns every non-root module joined by the
// checked-in repository go.work. The explicit absolute workspace path makes
// this independent of cwd and an inherited GOWORK value (including "off").
// Discovering modules keeps publication guards effective as optional modules
// are added without maintaining a hard-coded list in this test.
func archWorkspaceModuleFiles(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	workspace := filepath.Join(repositoryRoot, "go.work")
	out, err := archGoCommand(t, "work", "edit", "-json", workspace).Output()
	require.NoError(t, err, "Failed to parse workspace repository %s", workspace)

	var work archWorkspaceFile
	require.NoError(t, json.Unmarshal(out, &work), "Failed to parse go work edit -json output")
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
		require.NoError(t, err, "workspace module %s lacks go.mod", moduleRoot)
		require.False(t, info.IsDir(), "workspace module manifest %s must be a file", manifest)
		manifests = append(manifests, manifest)
	}
	require.NotEmpty(t, manifests, "go.work lists no independent sub module, release boundary guard is effectively inactive")
	sort.Strings(manifests)
	return manifests
}

// TestArchEveryRepositoryModuleBelongsToWorkspace ensures the Makefile and CI
// actually validate every independently publishable module. A go.mod on disk
// that is absent from go.work would otherwise be silently skipped.
func TestArchEveryRepositoryModuleBelongsToWorkspace(t *testing.T) {
	repositoryRoot := archRepositoryRoot(t)
	workspaceManifests := append(
		[]string{filepath.Join(repositoryRoot, "go.mod")},
		archWorkspaceModuleFiles(t)...,
	)
	sort.Strings(workspaceManifests)

	require.Equal(t, archRepositoryModuleFiles(t), workspaceManifests,
		"every repository go.mod must appear exactly once in go.work so make check and make test-race cover it")
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
		assert.True(t, filepath.IsAbs(manifest), "workspace manifest must resolve to an absolute path: %s", manifest)
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
		t.Skip("go command is unavailable, skipping module manifest check")
	}

	for _, manifest := range archWorkspaceModuleFiles(t) {
		mod := archReadModuleFile(t, manifest)
		assert.Empty(t, mod.Replace,
			"%s must not contain replace; inline repository call to go.work, release validation must resolve real tag", manifest)

		for _, req := range mod.Require {
			if req.Path != "github.com/xbcio/xbc" && !strings.HasPrefix(req.Path, "github.com/xbcio/xbc/") {
				continue
			}
			assert.Truef(t, archStableReleaseVersion(req.Version),
				"require for %s in %s must use real release tag shape vX.Y.Z; prohibit v0.0.0, pseudo versions, pre-releases or placeholder versions, got %q",
				manifest, req.Path, req.Version)
		}
	}
}
