package lease_test

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/lease"
)

// modulePath is this contract module's own import path. The dependency guards
// below must recognise it as self rather than as a third-party package.
const modulePath = "github.com/xbcio/xbc/extensions/coordination/lease"

// mustNoError stops the test when err is not nil.
func mustNoError(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}

// mustTrue stops the test when condition is false. Together with mustNoError it
// is the whole assertion vocabulary the suite needs to stay dependency-free.
func mustTrue(t *testing.T, condition bool, format string, args ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(format, args...)
	}
}

// productionGoFiles returns this module's production file names. The test
// binary runs in the module directory, so "." is the module root.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	mustNoError(t, err, "reading the contract module directory failed")

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
	mustTrue(t, len(files) > 0, "the contract module contains no production Go file, the guard below would prove nothing")

	checked := 0
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		mustNoError(t, err, "parsing %s failed", name)
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			mustNoError(t, err, "parsing the import path in %s failed", name)
			checked++
			if !isStdlib(importPath) {
				t.Errorf("%s imports %s; a contract module must be built from the standard library alone so that any consumer can depend on it without inheriting a dependency closure",
					name, importPath)
			}
		}
	}
	mustTrue(t, checked > 0,
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
	mustNoError(t, err, "go list -deps ./... failed in the contract module")

	listed := 0
	for _, line := range strings.Split(string(out), "\n") {
		dependency := strings.TrimSpace(line)
		if dependency == "" || dependency == modulePath || strings.HasPrefix(dependency, modulePath+"/") {
			continue
		}
		listed++
		if !isStdlib(dependency) {
			t.Errorf("the contract module's dependency closure contains %s; it must contain the standard library alone", dependency)
		}
	}
	mustTrue(t, listed > 0,
		"go list -deps returned no dependency at all, so this guard actually did not take effect")
}

// TestContractModuleManifestDeclaresNoRequirement keeps the manifest honest
// beside the source guards above. A requirement that no import needs is how a
// "zero dependency" module quietly acquires one.
func TestContractModuleManifestDeclaresNoRequirement(t *testing.T) {
	source, err := os.ReadFile("go.mod")
	mustNoError(t, err, "reading the contract module manifest failed")

	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "require") {
			t.Errorf("the contract module manifest declares %q; it must require nothing", trimmed)
		}
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
	held         map[string]*memoryLease
	acquisitions int
}

type memoryLease struct {
	locker *memoryLocker
	key    string
	owner  string
}

var (
	_ lease.Locker      = (*memoryLocker)(nil)
	_ lease.Lease       = (*memoryLease)(nil)
	_ lease.StrictLease = (*memoryLease)(nil)
)

func (l *memoryLocker) TryAcquire(_ context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
	if ttl < time.Millisecond {
		return nil, false, nil
	}
	if l.held == nil {
		l.held = make(map[string]*memoryLease)
	}
	if _, occupied := l.held[key]; occupied {
		return nil, false, nil
	}
	l.acquisitions++
	// The token is composed the way the contract requires: the claimant, when
	// one was named, over a part unique to this acquisition. The counter is that
	// unique part, which is all a single-process implementation needs.
	owner := fmt.Sprintf("%s:owner-%d", key, l.acquisitions)
	if claimant != "" {
		owner = claimant + "/" + owner
	}
	held := &memoryLease{locker: l, key: key, owner: owner}
	l.held[key] = held
	return held, true, nil
}

func (l *memoryLease) Key() string   { return l.key }
func (l *memoryLease) Owner() string { return l.owner }

// Renew and Release compare the stored token rather than this value's identity,
// which is what the contract requires of a real backend and what makes a token
// that repeated across acquisitions genuinely unsafe here too.
func (l *memoryLease) Renew(_ context.Context, _ time.Duration) (bool, error) {
	held, exists := l.locker.held[l.key]
	return exists && held.owner == l.owner, nil
}

func (l *memoryLease) Release(_ context.Context) (bool, error) {
	held, exists := l.locker.held[l.key]
	if !exists || held.owner != l.owner {
		return false, nil
	}
	delete(l.locker.held, l.key)
	return true, nil
}

// RenewStrict is the opt-in half of the contract: it confirms only a claim the
// store still holds and reports ErrLeaseLost otherwise. The memory locker has
// no ttl, so a released claim and a superseded one are the two losses it can
// express; both must fail rather than reconstruct.
func (l *memoryLease) RenewStrict(_ context.Context, _ time.Duration) (bool, error) {
	held, exists := l.locker.held[l.key]
	if !exists || held.owner != l.owner {
		return false, lease.ErrLeaseLost
	}
	return true, nil
}

// TestContractIsImplementableWithoutABackend pins the shape a consumer relies
// on: contention is reported as (nil, false, nil), and a superseded lease
// reports false without an error rather than failing.
func TestContractIsImplementableWithoutABackend(t *testing.T) {
	ctx := context.Background()
	var locker lease.Locker = &memoryLocker{}

	first, acquired, err := locker.TryAcquire(ctx, "placement:slot:0", "scanner-1", time.Second)
	mustNoError(t, err, "the first acquisition failed")
	mustTrue(t, acquired, "the first acquisition must win the free key")
	mustTrue(t, first != nil, "a granted acquisition must return a lease")

	second, acquired, err := locker.TryAcquire(ctx, "placement:slot:0", "scanner-1", time.Second)
	if err != nil {
		t.Errorf("contention is expected control flow, not an error: %v", err)
	}
	if acquired {
		t.Error("a second acquisition of a held key must be refused")
	}
	if second != nil {
		t.Errorf("a contended attempt must return no lease, got %v", second)
	}

	released, err := first.Release(ctx)
	mustNoError(t, err, "releasing the held lease failed")
	if !released {
		t.Error("the owner must release its own key")
	}

	released, err = first.Release(ctx)
	if err != nil {
		t.Errorf("a lease that no longer owns its key is not an operational failure: %v", err)
	}
	if released {
		t.Error("a second release must report that nothing was released")
	}
}

// TestAClaimantIsPublishedWithoutBecomingTheToken pins the rule that makes a
// held key readable without making it forgeable.
//
// Both halves are load-bearing and pull in opposite directions. Publishing the
// claimant is what turns "someone holds this slot" into "this process holds this
// slot", which is the only way an operator reading the store finds the process
// to go and look at. Composing it with something unique to the one acquisition
// is what keeps the token from repeating: a claimant is a chosen name that
// survives a restart, so a token that were only the claimant would make a dead
// process's lease compare equal to its successor's and renew a lock it no longer
// holds. An implementation that satisfies one half and not the other passes
// nothing here.
func TestAClaimantIsPublishedWithoutBecomingTheToken(t *testing.T) {
	ctx := context.Background()
	var locker lease.Locker = &memoryLocker{}

	first, acquired, err := locker.TryAcquire(ctx, "placement:slot:0", "scanner-1", time.Second)
	mustNoError(t, err, "the first acquisition failed")
	mustTrue(t, acquired, "the first acquisition must win the free key")
	if !strings.HasPrefix(first.Owner(), "scanner-1/") {
		t.Errorf("the token names the claimant, so reading the store answers which process holds the key: %q", first.Owner())
	}

	released, err := first.Release(ctx)
	mustNoError(t, err, "releasing the held lease failed")
	mustTrue(t, released, "the owner must release its own key")

	// The same process, having restarted, claims the same key under the same
	// name. This is the case the unique half exists for.
	second, acquired, err := locker.TryAcquire(ctx, "placement:slot:0", "scanner-1", time.Second)
	mustNoError(t, err, "the replacement acquisition failed")
	mustTrue(t, acquired, "the replacement must win the released key")
	if first.Owner() == second.Owner() {
		t.Error("two acquisitions under one claimant must not share a token, or the earlier lease could renew the later one's lock")
	}

	owned, err := first.Renew(ctx, time.Second)
	mustNoError(t, err, "renewing the superseded lease failed")
	if owned {
		t.Error("the superseded lease must not renew its successor's lock")
	}

	// An anonymous caller is still served; it gives up only readability.
	anonymous, acquired, err := locker.TryAcquire(ctx, "placement:slot:1", "", time.Second)
	mustNoError(t, err, "the anonymous acquisition failed")
	mustTrue(t, acquired, "an anonymous caller must still be served")
	if anonymous.Owner() == "" {
		t.Error("a token is still minted when no claimant was named")
	}
	if strings.Contains(anonymous.Owner(), "/") {
		t.Errorf("with no claimant there is no prefix to separate: %q", anonymous.Owner())
	}
}

// TestStrictRenewalIsOptInAndReportsALostClaim pins the second renewal rule a
// consumer can select. Renew keeps the default (and is covered above); a lease
// that implements StrictLease confirms only a claim the store still holds, and
// reports ErrLeaseLost when it does not instead of reconstructing it.
func TestStrictRenewalIsOptInAndReportsALostClaim(t *testing.T) {
	ctx := context.Background()
	var locker lease.Locker = &memoryLocker{}

	held, acquired, err := locker.TryAcquire(ctx, "locks:workload:0", "scanner-1", time.Second)
	mustNoError(t, err, "the acquisition failed")
	mustTrue(t, acquired, "the first acquisition must win the free key")

	strict, ok := held.(lease.StrictLease)
	if !ok {
		t.Fatal("a lease.Locker may return a lease.Lease; strict renewal is an explicit assertion the backend opts into")
	}

	owned, err := strict.RenewStrict(ctx, time.Second)
	mustNoError(t, err, "strict renewal of the held claim failed")
	if !owned {
		t.Error("a held claim must renew")
	}

	released, err := held.Release(ctx)
	mustNoError(t, err, "releasing the held lease failed")
	mustTrue(t, released, "the owner must release its own key")

	owned, err = strict.RenewStrict(ctx, time.Second)
	if owned {
		t.Error("a lapsed claim must not renew")
	}
	if !errors.Is(err, lease.ErrLeaseLost) {
		t.Errorf("a lapsed claim must be reported, not re-established: %v", err)
	}

	successor, acquired, err := locker.TryAcquire(ctx, "locks:workload:0", "scanner-1", time.Second)
	mustNoError(t, err, "the successor acquisition failed")
	mustTrue(t, acquired, "the successor must win the released key")

	owned, err = strict.RenewStrict(ctx, time.Second)
	if owned {
		t.Error("a superseded claim must not renew")
	}
	if !errors.Is(err, lease.ErrLeaseLost) {
		t.Errorf("a superseded claim must not renew its successor's lock: %v", err)
	}

	owned, err = successor.(lease.StrictLease).RenewStrict(ctx, time.Second)
	mustNoError(t, err, "strict renewal of the successor failed")
	if !owned {
		t.Error("the successor's held claim must renew")
	}
}
