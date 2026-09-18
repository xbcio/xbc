package lease_test

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

// modulePath is this contract module's own import path. The dependency guards
// below must recognise it as self rather than as a third-party package.
const modulePath = "github.com/xbcio/xbc/extensions/coordination/lease"

// productionGoFiles returns this module's production file names. The test
// binary runs in the module directory, so "." is the module root.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "reading the contract module directory failed")

	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	return files
}

// TestContractModuleImportsOnlyTheStandardLibrary is the source-level half of
// "zero dependencies". It reads the module's own import declarations rather
// than shelling out, so it holds even where the go command cannot run, and it
// attributes a violation to the exact file and import that caused it.
func TestContractModuleImportsOnlyTheStandardLibrary(t *testing.T) {
	files := productionGoFiles(t)
	require.NotEmpty(t, files, "the contract module contains no production Go file, the guard below would prove nothing")

	checked := 0
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		require.NoError(t, err, "parsing %s failed", name)
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			require.NoError(t, err, "parsing the import path in %s failed", name)
			checked++
			assert.Truef(t, isStdlib(importPath),
				"%s imports %s; a contract module must be built from the standard library alone so that any consumer can depend on it without inheriting a dependency closure",
				name, importPath)
		}
	}
	require.NotZero(t, checked,
		"no import was inspected, so this guard cannot distinguish a stdlib-only module from one that imports nothing")
}

// TestContractModuleClosureIsStandardLibraryOnly asks the same question of the
// resolved build closure rather than of the source, which is what catches a
// transitive dependency an innocuous intermediate package would otherwise
// smuggle in.
func TestContractModuleClosureIsStandardLibraryOnly(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command is unavailable, skipping dependency closure check")
	}
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	require.NoError(t, err, "go list -deps ./... failed in the contract module")

	listed := 0
	for _, line := range strings.Split(string(out), "\n") {
		dependency := strings.TrimSpace(line)
		if dependency == "" || dependency == modulePath || strings.HasPrefix(dependency, modulePath+"/") {
			continue
		}
		listed++
		assert.Truef(t, isStdlib(dependency),
			"the contract module's dependency closure contains %s; it must contain the standard library alone", dependency)
	}
	require.NotZero(t, listed,
		"go list -deps returned no dependency at all, so this guard actually did not take effect")
}

// TestContractModuleManifestDeclaresNoRequirement keeps the manifest honest
// beside the source guards above. A requirement that no import needs is how a
// "zero dependency" module quietly acquires one.
func TestContractModuleManifestDeclaresNoRequirement(t *testing.T) {
	source, err := os.ReadFile("go.mod")
	require.NoError(t, err, "reading the contract module manifest failed")

	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		assert.Falsef(t, strings.HasPrefix(trimmed, "require"),
			"the contract module manifest declares %q; it must require nothing", trimmed)
	}
}

// isStdlib reports whether path is a standard-library package.
//
// The test is whether the first path segment contains a dot. Every module path
// the go command can resolve starts with a hostname, and no standard-library
// path does -- the same rule the go command itself uses to decide what needs
// resolving.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// memoryLocker is the smallest useful implementation of the contract. It is
// here as a positive control: the two interfaces must stay implementable by a
// consumer outside this module, with no unexported helper and no backend
// assumption leaking through the vocabulary.
type memoryLocker struct {
	held map[string]*memoryLease
}

type memoryLease struct {
	locker *memoryLocker
	key    string
	owner  string
}

var (
	_ lease.Locker = (*memoryLocker)(nil)
	_ lease.Lease  = (*memoryLease)(nil)
)

func (l *memoryLocker) TryAcquire(_ context.Context, key string, ttl time.Duration) (lease.Lease, bool, error) {
	if ttl < time.Millisecond {
		return nil, false, nil
	}
	if l.held == nil {
		l.held = make(map[string]*memoryLease)
	}
	if _, occupied := l.held[key]; occupied {
		return nil, false, nil
	}
	held := &memoryLease{locker: l, key: key, owner: key + ":owner"}
	l.held[key] = held
	return held, true, nil
}

func (l *memoryLease) Key() string   { return l.key }
func (l *memoryLease) Owner() string { return l.owner }

func (l *memoryLease) Renew(_ context.Context, _ time.Duration) (bool, error) {
	return l.locker.held[l.key] == l, nil
}

func (l *memoryLease) Release(_ context.Context) (bool, error) {
	if l.locker.held[l.key] != l {
		return false, nil
	}
	delete(l.locker.held, l.key)
	return true, nil
}

// TestContractIsImplementableWithoutABackend pins the shape a consumer relies
// on: contention is reported as (nil, false, nil), and a superseded lease
// reports false without an error rather than failing.
func TestContractIsImplementableWithoutABackend(t *testing.T) {
	ctx := context.Background()
	var locker lease.Locker = &memoryLocker{}

	first, acquired, err := locker.TryAcquire(ctx, "placement:sast:0", time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NotNil(t, first)

	second, acquired, err := locker.TryAcquire(ctx, "placement:sast:0", time.Second)
	assert.NoError(t, err, "contention is expected control flow, not an error")
	assert.False(t, acquired)
	assert.Nil(t, second)

	released, err := first.Release(ctx)
	require.NoError(t, err)
	assert.True(t, released)

	released, err = first.Release(ctx)
	assert.NoError(t, err, "a lease that no longer owns its key is not an operational failure")
	assert.False(t, released)
}
