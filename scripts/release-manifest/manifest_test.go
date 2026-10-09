package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeReleaseFixture(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}

func TestWorkspaceDirectoriesReadsUseDirectives(t *testing.T) {
	root := t.TempDir()
	absolute := filepath.Join(root, "absolute")
	writeReleaseFixture(t, root, "go.work", fmt.Sprintf(`// Workspace of the release fixture.
go 1.25.0

toolchain go1.25.0

/* A block comment may contain the word use (twice) without joining modules.
   The go command rejects block comments in go.work, so this only pins the
   tokenizer's tolerance of them. */

use (
	.
	./lib // The root and the library module.
	"./quoted path"
)

use ./single
use %s
`, absolute))

	directories, err := workspaceDirectories(filepath.Join(root, "go.work"))
	require.NoError(t, err)

	expected := []string{
		root,
		filepath.Join(root, "lib"),
		filepath.Join(root, "quoted path"),
		filepath.Join(root, "single"),
		absolute,
	}
	assert.Equal(t, expected, directories)
}

func TestWorkspaceDirectoriesRejectsMalformedInput(t *testing.T) {
	for name, contents := range map[string]string{
		"unclosed use block":   "go 1.25.0\n\nuse (\n\t./lib\n",
		"unterminated comment": "go 1.25.0\n\n/* use ./lib\n",
		"bare use":             "go 1.25.0\n\nuse\n",
		"duplicate use":        "go 1.25.0\n\nuse ./lib\nuse ./lib\n",
		"block duplicate use":  "go 1.25.0\n\nuse (\n\t./lib\n\t./lib\n)\n",
		"no module":            "go 1.25.0\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeReleaseFixture(t, root, "go.work", contents)
			_, err := workspaceDirectories(filepath.Join(root, "go.work"))
			assert.Error(t, err)
		})
	}
}

func TestDeclaredModulePathReadsTheModuleDirective(t *testing.T) {
	root := t.TempDir()
	writeReleaseFixture(t, root, "go.mod", `// The module directive comes first.
module example.com/repo

go 1.25.0

require example.com/module v1.2.3
`)

	path, err := declaredModulePath(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, "example.com/repo", path)
}

// TestScanImportsMatchesTheGoCommandsBuildSet pins the import scan against the
// files the go command builds: everything it ignores stays ignored, and
// everything it builds contributes -- including files that merely mention a
// build constraint in a comment the go command does not read as one.
func TestScanImportsMatchesTheGoCommandsBuildSet(t *testing.T) {
	root := t.TempDir()
	moduleDirectory := filepath.Join(root, "mod")

	writeReleaseFixture(t, root, "mod/go.mod", "module example.com/mod\n\ngo 1.25.0\n")
	writeReleaseFixture(t, root, "mod/root.go", "package mod\n\nimport (\n\t\"example.com/one\"\n\t\"fmt\"\n)\n")
	writeReleaseFixture(t, root, "mod/root_test.go", "package mod\n\nimport \"example.com/two\"\n")
	writeReleaseFixture(t, root, "mod/sub/sub.go", "package sub\n\nimport \"example.com/three\"\n")
	writeReleaseFixture(t, root, "mod/vendor/imported.go", "package imported\n\nimport \"example.com/vendor\"\n")
	writeReleaseFixture(t, root, "mod/testdata/fixture.go", "package fixture\n\nimport \"example.com/testdata\"\n")
	writeReleaseFixture(t, root, "mod/_tools/tool.go", "package tool\n\nimport \"example.com/underscore-directory\"\n")
	writeReleaseFixture(t, root, "mod/.hidden/hidden.go", "package hidden\n\nimport \"example.com/dot-directory\"\n")
	writeReleaseFixture(t, root, "mod/_ignored.go", "package mod\n\nimport \"example.com/underscore-file\"\n")
	writeReleaseFixture(t, root, "mod/.hidden.go", "package mod\n\nimport \"example.com/dot-file\"\n")
	writeReleaseFixture(t, root, "mod/generate.go", "//go:build ignore\n\npackage main\n\nimport \"example.com/build-ignored\"\n")
	writeReleaseFixture(t, root, "mod/legacy.go", "// +build ignore\n\npackage main\n\nimport \"example.com/legacy-build-ignored\"\n")
	writeReleaseFixture(t, root, "mod/blocked.go", "/*\n//go:build ignore\n*/\npackage mod\n\nimport \"example.com/block-comment\"\n")
	// Only a constraint before the package clause counts: the go command
	// builds this file, which the toolchain accepts while the `//go:build`
	// spelling of the same comment after the clause is a compile error.
	writeReleaseFixture(t, root, "mod/ported.go", "package mod\n\n// The maintenance program next door still carries its legacy\n// +build ignore\n// header while it is being ported.\n\nimport \"example.com/late-legacy-constraint\"\n")
	writeReleaseFixture(t, root, "mod/nested/go.mod", "module example.com/nested\n\ngo 1.25.0\n")
	writeReleaseFixture(t, root, "mod/nested/nested.go", "package nested\n\nimport \"example.com/nested\"\n")
	writeReleaseFixture(t, root, "mod/stray/go.mod", "module example.com/stray\n\ngo 1.25.0\n")
	writeReleaseFixture(t, root, "mod/stray/stray.go", "package stray\n\nimport \"example.com/stray\"\n")
	writeReleaseFixture(t, root, "outside/outside.go", "package outside\n\nimport \"example.com/symlinked-directory\"\n")
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(moduleDirectory, "linked")); err != nil {
		t.Logf("symbolic links unavailable, the symlink case is skipped: %v", err)
	}

	imports, err := scanImports(moduleDirectory)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"example.com/block-comment",
		"example.com/late-legacy-constraint",
		"example.com/one",
		"example.com/three",
		"example.com/two",
		"fmt",
	}, imports)
}

func TestBuildManifestOrdersModulesIntoDependencyWaves(t *testing.T) {
	modules := []workspaceModule{
		{path: "example.com/root", directory: "/repo", relative: "."},
		{path: "example.com/examples", directory: "/repo/examples", relative: "examples"},
		{path: "example.com/lib", directory: "/repo/lib", relative: "lib"},
		{path: "example.com/tool", directory: "/repo/scripts/tool", relative: "scripts/tool"},
		{path: "example.com/web", directory: "/repo/web", relative: "web"},
	}
	// Declared imports arrive unsorted here: dependencies must come out
	// ordered by module path, never in the order an import was seen.
	declaredImports := map[string][]string{
		"example.com/root":     {"fmt", "example.com/lib"},
		"example.com/examples": {"example.com/web", "example.com/lib"},
		"example.com/lib":      {"os"},
		"example.com/tool":     {},
		"example.com/web":      {"example.com/tool", "example.com/lib"},
	}

	value, err := buildManifest(modules, declaredImports)
	require.NoError(t, err)

	assert.Equal(t, []manifestModule{
		{Path: "example.com/lib", Directory: "lib", TagPrefix: "lib/", Wave: 0, Release: true, Imports: []string{}},
		{Path: "example.com/tool", Directory: "scripts/tool", TagPrefix: "scripts/tool/", Wave: 0, Release: false, Imports: []string{}},
		{Path: "example.com/root", Directory: ".", TagPrefix: "", Wave: 1, Release: true, Imports: []string{"example.com/lib"}},
		{Path: "example.com/web", Directory: "web", TagPrefix: "web/", Wave: 1, Release: true, Imports: []string{"example.com/lib", "example.com/tool"}},
		{Path: "example.com/examples", Directory: "examples", TagPrefix: "examples/", Wave: 2, Release: true, Imports: []string{"example.com/lib", "example.com/web"}},
	}, value.Modules)
}

// TestBuildManifestWaitsForTheDeepestDependency keeps a module on the wave of
// its deepest dependency even when a shallower dependency sorts last, since a
// module may only be tagged after every module it imports.
func TestBuildManifestWaitsForTheDeepestDependency(t *testing.T) {
	modules := []workspaceModule{
		{path: "example.com/alpha", directory: "/repo/alpha", relative: "alpha"},
		{path: "example.com/beta", directory: "/repo/beta", relative: "beta"},
		{path: "example.com/importer", directory: "/repo/importer", relative: "importer"},
		{path: "example.com/zeta", directory: "/repo/zeta", relative: "zeta"},
	}
	declaredImports := map[string][]string{
		"example.com/alpha":    {"example.com/beta"},
		"example.com/beta":     {},
		"example.com/importer": {"example.com/alpha", "example.com/zeta"},
		"example.com/zeta":     {},
	}

	value, err := buildManifest(modules, declaredImports)
	require.NoError(t, err)

	waves := make(map[string]int, len(value.Modules))
	for _, module := range value.Modules {
		waves[module.Path] = module.Wave
	}
	assert.Equal(t, 0, waves["example.com/beta"])
	assert.Equal(t, 1, waves["example.com/alpha"])
	assert.Equal(t, 0, waves["example.com/zeta"])
	assert.Equal(t, 2, waves["example.com/importer"])
}

func TestBuildManifestRejectsAnImportCycle(t *testing.T) {
	modules := []workspaceModule{
		{path: "example.com/a", directory: "/repo/a", relative: "a"},
		{path: "example.com/b", directory: "/repo/b", relative: "b"},
		{path: "example.com/c", directory: "/repo/c", relative: "c"},
	}
	declaredImports := map[string][]string{
		"example.com/a": {"example.com/b"},
		"example.com/b": {"example.com/c"},
		"example.com/c": {"example.com/a"},
	}

	_, err := buildManifest(modules, declaredImports)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
	assert.Contains(t, err.Error(), "example.com/a -> example.com/b -> example.com/c -> example.com/a")
}

func TestModuleOwnerPrefersTheLongestModulePath(t *testing.T) {
	modules := []workspaceModule{
		{path: "example.com/root", directory: "/repo", relative: "."},
		{path: "example.com/root/tree", directory: "/repo/tree", relative: "tree"},
	}

	owner, ok := moduleOwner(modules, "example.com/root/tree/leaf")
	assert.True(t, ok)
	assert.Equal(t, "example.com/root/tree", owner)

	owner, ok = moduleOwner(modules, "example.com/root/other")
	assert.True(t, ok)
	assert.Equal(t, "example.com/root", owner)

	_, ok = moduleOwner(modules, "example.com/elsewhere")
	assert.False(t, ok)
}

func TestReleaseModuleClassifiesRepositoryTooling(t *testing.T) {
	for relative, expected := range map[string]bool{
		".":                 true,
		"lib":               true,
		"examples":          true,
		"tests/composition": true,
		"scripts":           false,
		"scripts/tool":      false,
	} {
		assert.Equalf(t, expected, releaseModule(relative), "releaseModule(%q)", relative)
	}
}

// TestGenerateRefusesModuleShapesItCannotDescribe keeps the tool loud where
// its literal tree scan would otherwise emit a silently wrong order.
func TestGenerateRefusesModuleShapesItCannotDescribe(t *testing.T) {
	t.Run("symlinked module directory", func(t *testing.T) {
		root := t.TempDir()
		writeReleaseFixture(t, root, "go.work", "go 1.25.0\n\nuse (\n\t.\n\t./linked\n)\n")
		writeReleaseFixture(t, root, "go.mod", "module example.com/repo\n\ngo 1.25.0\n")
		writeReleaseFixture(t, root, "real/go.mod", "module example.com/repo/real\n\ngo 1.25.0\n")
		writeReleaseFixture(t, root, "real/real.go", "package real\n")
		if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linked")); err != nil {
			t.Skipf("symbolic links unavailable: %v", err)
		}

		_, _, err := generate(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "symbolic link")
	})

	t.Run("module outside the repository", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "repo")
		writeReleaseFixture(t, root, "go.work", "go 1.25.0\n\nuse (\n\t.\n\t../elsewhere\n)\n")
		writeReleaseFixture(t, root, "go.mod", "module example.com/repo\n\ngo 1.25.0\n")
		writeReleaseFixture(t, parent, "elsewhere/go.mod", "module example.com/elsewhere\n\ngo 1.25.0\n")

		_, _, err := generate(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the repository")
	})

	t.Run("use target without a manifest", func(t *testing.T) {
		root := t.TempDir()
		writeReleaseFixture(t, root, "go.work", "go 1.25.0\n\nuse (\n\t.\n\t./missing\n)\n")
		writeReleaseFixture(t, root, "go.mod", "module example.com/repo\n\ngo 1.25.0\n")

		_, _, err := generate(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing")
	})

	t.Run("workspace without submodules", func(t *testing.T) {
		root := t.TempDir()
		writeReleaseFixture(t, root, "go.work", "go 1.25.0\n\nuse .\n")
		writeReleaseFixture(t, root, "go.mod", "module example.com/repo\n\ngo 1.25.0\n")

		_, _, err := generate(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to emit")
	})
}

func newReleaseRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeReleaseFixture(t, root, "go.work", "go 1.25.0\n\nuse (\n\t.\n\t./lib\n)\n")
	writeReleaseFixture(t, root, "go.mod", "module example.com/repo\n\ngo 1.25.0\n")
	writeReleaseFixture(t, root, "main.go", "package main\n\nimport _ \"example.com/repo/lib\"\n\nfunc main() {}\n")
	writeReleaseFixture(t, root, "lib/go.mod", "module example.com/repo/lib\n\ngo 1.25.0\n")
	writeReleaseFixture(t, root, "lib/lib.go", "package lib\n")
	return root
}

func TestCLIWriteThenCheckSucceeds(t *testing.T) {
	root := newReleaseRepository(t)
	output := filepath.Join(root, "manifest.json")

	code := run([]string{"-root", root, "-write", "-output", output})
	require.Equal(t, 0, code, "-write must succeed against a valid workspace")

	encoded, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(encoded), "\n"), "the committed manifest must end with a newline")

	code = run([]string{"-root", root, "-check", "-output", output})
	assert.Equal(t, 0, code, "-check must accept a manifest written moments earlier")
}

func TestCLICheckFailsWhenTheManifestDrifts(t *testing.T) {
	root := newReleaseRepository(t)
	output := filepath.Join(root, "manifest.json")

	require.Equal(t, 0, run([]string{"-root", root, "-write", "-output", output}))
	writeReleaseFixture(t, root, "manifest.json", `{"modules": []}`+"\n")

	assert.Equal(t, 1, run([]string{"-root", root, "-check", "-output", output}))
}

func TestCLIRequiresExactlyOneMode(t *testing.T) {
	root := newReleaseRepository(t)
	assert.Equal(t, 2, run([]string{"-root", root}))
	assert.Equal(t, 2, run([]string{"-root", root, "-write", "-check"}))
	assert.Equal(t, 2, run([]string{"-root", root, "-stdout", "unexpected"}))
}

// TestGenerateAgainstTheRepository pins the release manifest against the real
// workspace: the hand-written go.work parser must agree with the go command,
// generation must be deterministic, and every module must sit exactly one
// wave deeper than its deepest dependency.
func TestGenerateAgainstTheRepository(t *testing.T) {
	root, err := resolveRepositoryRoot("")
	require.NoError(t, err)

	value, encoded, err := generate(root)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(value.Modules), 2)

	_, regenerated, err := generate(root)
	require.NoError(t, err)
	assert.Equal(t, string(encoded), string(regenerated), "manifest generation must be deterministic")

	// Cross-check the workspace parser against the go command instead of
	// trusting it: a parser that silently drops a use directive would
	// otherwise shrink the release train unnoticed.
	command := exec.Command("go", "work", "edit", "-json", filepath.Join(root, "go.work"))
	output, err := command.Output()
	require.NoError(t, err, "go work edit -json must parse the repository workspace")
	var workspace struct {
		Use []struct {
			DiskPath string
		}
	}
	require.NoError(t, json.Unmarshal(output, &workspace))
	var expected []string
	for _, use := range workspace.Use {
		directory := use.DiskPath
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(root, filepath.FromSlash(directory))
		}
		expected = append(expected, filepath.Clean(directory))
	}
	sort.Strings(expected)

	directories, err := workspaceDirectories(filepath.Join(root, "go.work"))
	require.NoError(t, err)
	sort.Strings(directories)
	assert.Equal(t, expected, directories, "parsed workspace must match go work edit")

	// Every wave is exactly one deeper than the module's deepest dependency,
	// and the release classification separates core from repository tooling.
	waves := make(map[string]int, len(value.Modules))
	released := make(map[string]bool, len(value.Modules))
	for _, module := range value.Modules {
		waves[module.Path] = module.Wave
		released[module.Path] = module.Release
	}
	for _, module := range value.Modules {
		deepest := -1
		for _, imported := range module.Imports {
			importedWave, known := waves[imported]
			require.Truef(t, known, "%s imports %s, which the manifest does not list", module.Path, imported)
			deepest = max(deepest, importedWave)
		}
		assert.Equalf(t, deepest+1, module.Wave, "%s must be tagged exactly one wave after its deepest dependency", module.Path)
	}
	assert.True(t, released["github.com/xbcio/xbc"], "the core module is always released")
	assert.False(t, released["github.com/xbcio/xbc/scripts/plugin-snapshots"], "repository tooling is never released")
	assert.Equal(t, "", value.Modules[0].TagPrefix, "the root module is tagged without a prefix")
}
