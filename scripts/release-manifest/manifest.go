package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// workspaceModule is one module joined by the repository go.work.
type workspaceModule struct {
	path      string // module path declared by the module's go.mod
	directory string // absolute module directory
	relative  string // module directory relative to the repository root, "." for the root module
}

// manifest is the release-train order: every module joined by go.work, the
// workspace modules it imports, and the wave its tag belongs to. Modules is
// ordered by wave and then module path so releases proceed top to bottom.
type manifest struct {
	Modules []manifestModule `json:"modules"`
}

// manifestModule describes one module's place in the release train. Imports
// lists the workspace modules the module imports: these are exactly the
// sibling requirements it may only gain once the imported modules have
// published tags. TagPrefix is the directory prefix the module's release tag
// carries ("" for the root module, "transport/web/" for that submodule), and
// Release is false for repository tooling that is never tagged.
type manifestModule struct {
	Path      string   `json:"path"`
	Directory string   `json:"directory"`
	TagPrefix string   `json:"tag_prefix"`
	Wave      int      `json:"wave"`
	Release   bool     `json:"release"`
	Imports   []string `json:"imports"`
}

// generate builds the release manifest for the repository rooted at root and
// returns it with its canonical encoding.
func generate(root string) (manifest, []byte, error) {
	directories, err := workspaceDirectories(filepath.Join(root, "go.work"))
	if err != nil {
		return manifest{}, nil, err
	}
	if len(directories) < 2 {
		return manifest{}, nil, fmt.Errorf("go.work joins %d modules; refusing to emit a release manifest that would deactivate the release order guard", len(directories))
	}

	modules := make([]workspaceModule, 0, len(directories))
	modulePaths := make(map[string]bool, len(directories))
	for _, directory := range directories {
		// The scan reads module trees literally, so a module that reaches the
		// workspace through a symbolic link would silently lose every import
		// edge: refuse it loudly instead of emitting an incomplete order.
		info, err := os.Lstat(directory)
		if err != nil {
			return manifest{}, nil, fmt.Errorf("inspect workspace module %s: %w", directory, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return manifest{}, nil, fmt.Errorf("go.work joins %s, a symbolic link; the release manifest reads module trees literally, so use the directory it points at", directory)
		}
		relative := relativeDirectory(root, directory)
		if relative == ".." || strings.HasPrefix(relative, "../") {
			return manifest{}, nil, fmt.Errorf("go.work joins %s outside the repository root; the release manifest covers only modules this repository tags", directory)
		}
		declared, err := declaredModulePath(filepath.Join(directory, "go.mod"))
		if err != nil {
			return manifest{}, nil, err
		}
		if modulePaths[declared] {
			return manifest{}, nil, fmt.Errorf("go.work joins two modules that both declare module path %s", declared)
		}
		modulePaths[declared] = true
		modules = append(modules, workspaceModule{
			path:      declared,
			directory: directory,
			relative:  relative,
		})
	}
	sort.Slice(modules, func(left, right int) bool { return modules[left].path < modules[right].path })

	imports := make(map[string][]string, len(modules))
	for _, module := range modules {
		declared, err := scanImports(module.directory)
		if err != nil {
			return manifest{}, nil, fmt.Errorf("scan imports of %s: %w", module.path, err)
		}
		imports[module.path] = declared
	}

	result, err := buildManifest(modules, imports)
	if err != nil {
		return manifest{}, nil, err
	}
	encoded, err := marshalManifest(result)
	if err != nil {
		return manifest{}, nil, err
	}
	return result, encoded, nil
}

// buildManifest assembles the release manifest from the discovered modules
// and each module's declared import paths. It is a pure function of those two
// inputs so the ordering rules are testable without a repository.
func buildManifest(modules []workspaceModule, declaredImports map[string][]string) (manifest, error) {
	edges := make(map[string][]string, len(modules))
	for _, module := range modules {
		seen := make(map[string]bool)
		dependencies := []string{}
		for _, importPath := range declaredImports[module.path] {
			owner, ok := moduleOwner(modules, importPath)
			if !ok || owner == module.path || seen[owner] {
				continue
			}
			seen[owner] = true
			dependencies = append(dependencies, owner)
		}
		sort.Strings(dependencies)
		edges[module.path] = dependencies
	}

	waves, err := assignWaves(modules, edges)
	if err != nil {
		return manifest{}, err
	}

	result := manifest{Modules: make([]manifestModule, 0, len(modules))}
	for _, module := range modules {
		result.Modules = append(result.Modules, manifestModule{
			Path:      module.path,
			Directory: filepath.ToSlash(module.relative),
			TagPrefix: tagPrefix(module.relative),
			Wave:      waves[module.path],
			Release:   releaseModule(module.relative),
			Imports:   edges[module.path],
		})
	}
	sort.Slice(result.Modules, func(left, right int) bool {
		if result.Modules[left].Wave != result.Modules[right].Wave {
			return result.Modules[left].Wave < result.Modules[right].Wave
		}
		return result.Modules[left].Path < result.Modules[right].Path
	})
	return result, nil
}

// releaseModule reports whether a module is part of the release train. Every
// workspace module is released except repository tooling under scripts/: it
// exists to validate the repository, has no consumable API, and nothing
// outside the repository imports it. Keeping the rule structural means a new
// tool module is classified without editing a list.
func releaseModule(relative string) bool {
	return relative != "scripts" && !strings.HasPrefix(relative, "scripts/")
}

// tagPrefix returns the tag prefix a module's release version carries. The
// root module is tagged vX.Y.Z; a submodule is tagged with its directory path
// as prefix, so transport/web is tagged transport/web/vX.Y.Z. Recording it
// keeps the manifest a complete release checklist.
func tagPrefix(relative string) string {
	if relative == "." {
		return ""
	}
	return filepath.ToSlash(relative) + "/"
}

// moduleOwner returns the workspace module whose path owns an import path.
// The longest matching module path wins so the root module does not swallow
// its own submodules.
func moduleOwner(modules []workspaceModule, importPath string) (string, bool) {
	owner, found := "", false
	for _, module := range modules {
		if importPath != module.path && !strings.HasPrefix(importPath, module.path+"/") {
			continue
		}
		if !found || len(module.path) > len(owner) {
			owner, found = module.path, true
		}
	}
	return owner, found
}

// assignWaves computes each module's release wave: zero for a module that
// imports no other workspace module, otherwise one more than the deepest wave
// it imports. A module may only be tagged after every module it imports, so a
// wave is the longest dependency path. Modules are visited in the order given
// so a detected cycle reads as a dependency chain.
func assignWaves(modules []workspaceModule, edges map[string][]string) (map[string]int, error) {
	const (
		visiting = 1
		finished = 2
	)
	waves := make(map[string]int, len(modules))
	state := make(map[string]int, len(modules))
	var stack []string
	var visit func(string) (int, error)
	visit = func(path string) (int, error) {
		switch state[path] {
		case finished:
			return waves[path], nil
		case visiting:
			return 0, fmt.Errorf("module import cycle: %s", strings.Join(append(stack, path), " -> "))
		}
		state[path] = visiting
		stack = append(stack, path)
		wave := 0
		for _, dependency := range edges[path] {
			dependencyWave, err := visit(dependency)
			if err != nil {
				return 0, err
			}
			wave = max(wave, dependencyWave+1)
		}
		stack = stack[:len(stack)-1]
		state[path] = finished
		waves[path] = wave
		return wave, nil
	}
	for _, module := range modules {
		if _, err := visit(module.path); err != nil {
			return nil, err
		}
	}
	return waves, nil
}

// workspaceDirectories returns the module directories a go.work file joins as
// absolute cleaned paths. It reads the workspace manifest itself instead of
// asking the go command, so the tool also works under the `GOWORK=off`
// release checks the release plan runs per module.
func workspaceDirectories(workspacePath string) ([]string, error) {
	contents, err := os.ReadFile(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", workspacePath, err)
	}
	tokens, err := manifestTokens(contents)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", workspacePath, err)
	}

	root := filepath.Dir(workspacePath)
	joined := make(map[string]bool)
	var directories []string
	for index := 0; index < len(tokens); index++ {
		if tokens[index] != "use" {
			continue
		}
		index++
		if index >= len(tokens) {
			return nil, fmt.Errorf("%s ends after a bare use directive", workspacePath)
		}
		if tokens[index] == "(" {
			for {
				index++
				if index >= len(tokens) {
					return nil, fmt.Errorf("%s has an unclosed use block", workspacePath)
				}
				if tokens[index] == ")" {
					break
				}
				directory := resolveWorkspaceDirectory(root, tokens[index])
				if joined[directory] {
					return nil, fmt.Errorf("%s joins %s twice", workspacePath, tokens[index])
				}
				joined[directory] = true
				directories = append(directories, directory)
			}
			continue
		}
		directory := resolveWorkspaceDirectory(root, tokens[index])
		if joined[directory] {
			return nil, fmt.Errorf("%s joins %s twice", workspacePath, tokens[index])
		}
		joined[directory] = true
		directories = append(directories, directory)
	}
	if len(directories) == 0 {
		return nil, fmt.Errorf("%s joins no module", workspacePath)
	}
	return directories, nil
}

// resolveWorkspaceDirectory resolves one go.work use path against the
// workspace directory it is written in.
func resolveWorkspaceDirectory(root, use string) string {
	if !filepath.IsAbs(use) {
		use = filepath.Join(root, filepath.FromSlash(use))
	}
	return filepath.Clean(use)
}

// declaredModulePath returns the module path a go.mod declares.
func declaredModulePath(manifestPath string) (string, error) {
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", manifestPath, err)
	}
	tokens, err := manifestTokens(contents)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", manifestPath, err)
	}
	for index, token := range tokens {
		// A bare module token can only be the module directive: a module path
		// in a require, exclude, or replace directive always contains a slash.
		if token != "module" || index+1 >= len(tokens) {
			continue
		}
		return tokens[index+1], nil
	}
	return "", fmt.Errorf("%s declares no module directive", manifestPath)
}

// manifestTokens splits a go.work or go.mod file into tokens the way the go
// command reads them: comments are dropped, quoted paths are unquoted, and
// parentheses stand alone so directive blocks tokenize uniformly.
func manifestTokens(contents []byte) ([]string, error) {
	var tokens []string
	for index := 0; index < len(contents); {
		character := contents[index]
		switch {
		case character == ' ' || character == '\t' || character == '\r' || character == '\n':
			index++
		case character == '/' && index+1 < len(contents) && contents[index+1] == '/':
			for index < len(contents) && contents[index] != '\n' {
				index++
			}
		case character == '/' && index+1 < len(contents) && contents[index+1] == '*':
			end := bytes.Index(contents[index+2:], []byte("*/"))
			if end < 0 {
				return nil, fmt.Errorf("unterminated block comment")
			}
			index += 2 + end + 2
		case character == '(' || character == ')':
			tokens = append(tokens, string(character))
			index++
		case character == '"' || character == '`':
			quote := character
			index++
			var token strings.Builder
			for {
				if index >= len(contents) {
					return nil, fmt.Errorf("unterminated quoted string")
				}
				if contents[index] == quote {
					index++
					break
				}
				if quote == '"' && contents[index] == '\\' && index+1 < len(contents) {
					index++
				}
				token.WriteByte(contents[index])
				index++
			}
			tokens = append(tokens, token.String())
		default:
			start := index
			for index < len(contents) && !isManifestSeparator(contents[index]) {
				index++
			}
			tokens = append(tokens, string(contents[start:index]))
		}
	}
	return tokens, nil
}

func isManifestSeparator(character byte) bool {
	switch character {
	case ' ', '\t', '\r', '\n', '(', ')':
		return true
	}
	return false
}

// scanImports returns every import path declared by the Go files of one
// module, test files included: a module's tests must resolve their sibling
// requirements under `GOWORK=off` exactly like its packages. Directories the
// go command does not build are skipped -- every nested module (a directory
// holding its own go.mod, whether or not go.work lists it), vendor/,
// testdata/, and directories or files whose names begin with "." or "_" --
// together with files whose header excludes them from every build. The scan
// is deliberately conservative where it cannot be exact: a file excluded by a
// platform-specific constraint still counts, so the order never depends on
// the machine generating it, while symlinked directories are not followed,
// matching the go command's package matching.
func scanImports(moduleDirectory string) ([]string, error) {
	files := token.NewFileSet()
	seen := make(map[string]bool)
	var imports []string
	err := filepath.WalkDir(moduleDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == moduleDirectory {
				return nil
			}
			name := entry.Name()
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(files, path, source, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		if ignoresBuild(file) {
			return nil
		}
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if seen[value] {
				continue
			}
			seen[value] = true
			imports = append(imports, value)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(imports)
	return imports, nil
}

// ignoresBuild reports whether a file's header excludes it from every build
// with a `//go:build ignore` or `// +build ignore` constraint, the
// conventional shape for maintenance programs that must not contribute
// release edges. Only comments the parser reports as header comments count:
// the same text inside a block comment is not a constraint and must not hide
// the file's imports.
func ignoresBuild(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, comment := range group.List {
			if constraint, ok := strings.CutPrefix(comment.Text, "//go:build"); ok && strings.TrimSpace(constraint) == "ignore" {
				return true
			}
			if constraint, ok := strings.CutPrefix(comment.Text, "// +build"); ok && slices.Contains(strings.Fields(constraint), "ignore") {
				return true
			}
		}
	}
	return false
}

// relativeDirectory returns a module directory relative to the repository
// root, "." for the root module.
func relativeDirectory(root, directory string) string {
	relative, err := filepath.Rel(root, directory)
	if err != nil {
		return filepath.ToSlash(directory)
	}
	return filepath.ToSlash(relative)
}

// marshalManifest encodes a manifest in the canonical committed form:
// two-space indentation and a trailing newline.
func marshalManifest(value manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
