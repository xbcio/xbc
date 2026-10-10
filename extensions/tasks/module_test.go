package tasks

import (
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestModuleImportsOnlyTheStandardLibrary keeps the module's defining property
// checkable: the task facade must resolve without any workspace context, so
// that a plugin can define or submit tasks without inheriting a dependency
// closure. It scans production and test files alike, because a test-only
// dependency would still make the module unresolvable with GOWORK=off.
func TestModuleImportsOnlyTheStandardLibrary(t *testing.T) {
	production, testFiles := moduleGoFiles(t)
	files := append(production, testFiles...)
	if len(files) == 0 {
		t.Fatal("no Go files were found, so the guard would pass vacuously")
	}

	var offenders []string
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s failed: %v", name, err)
		}
		for _, specification := range file.Imports {
			path, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				t.Fatalf("unquoting the import in %s failed: %v", name, err)
			}
			if !isStandardLibrary(path) {
				offenders = append(offenders, name+" imports "+path)
			}
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("the tasks module must import only the standard library, found:\n\t%s", strings.Join(offenders, "\n\t"))
	}
}

// TestModuleRequiresNothing asserts that go.mod declares no requirement. A
// requirement would defeat the module's purpose even when no file imports the
// dependency yet: it would enter every consumer's module graph.
func TestModuleRequiresNothing(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod failed: %v", err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "require (" || strings.HasPrefix(trimmed, "require ") {
			t.Fatalf("go.mod must not require anything, found %q; the module deliberately depends on nothing so importing tasks never adds a dependency closure", trimmed)
		}
	}
}

// TestStandardLibraryPredicateRejectsForeignPaths is the positive control for
// the guard above: a predicate that accepted everything would let a third-party
// import through unnoticed.
func TestStandardLibraryPredicateRejectsForeignPaths(t *testing.T) {
	for _, path := range []string{"context", "encoding/json", "go/parser"} {
		if !isStandardLibrary(path) {
			t.Errorf("isStandardLibrary(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"github.com/xbcio/xbc/plugin",
		"github.com/hibiken/asynq",
		"gopkg.in/yaml.v3",
	} {
		if isStandardLibrary(path) {
			t.Errorf("isStandardLibrary(%q) = true, want false", path)
		}
	}
}

// moduleGoFiles returns the module's own Go files, split into production and
// test files. The module is a single package, so the current directory is the
// whole surface the guard must cover.
func moduleGoFiles(t *testing.T) (production, testFiles []string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory failed: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			testFiles = append(testFiles, name)
			continue
		}
		production = append(production, name)
	}
	sort.Strings(production)
	sort.Strings(testFiles)
	return production, testFiles
}

// isStandardLibrary reports whether an import path names a standard library
// package. Standard library paths never carry a dot in their first segment;
// every module path has a domain there.
func isStandardLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
