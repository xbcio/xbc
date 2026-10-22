package architecture_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archProcessKnobOwner is the one directory allowed to install the
// process-level knobs directly. The runtime package owns them because they are
// the process's own resource policy: GOMAXPROCS, GOGC and the soft memory limit
// all apply to every goroutine and every allocation in the binary, so a plugin
// that sets one is changing the budget of the components beside it, from inside
// one of them.
//
// The configuration that reaches them is xbc.runtime, resolved once during
// bootstrap (see runtime/runtime_knobs.go). Nothing here is about keeping a
// capability unreachable -- the same package is open to callers -- it is about
// keeping a single owner of a decision that only makes sense once per process.
//
// One deliberate exception exists, and it is not a second owner: a plugin whose
// entire purpose is the process memory policy may install the soft limit
// through the automemlimit library (see archAutomemlimitOwner below). The core
// cannot derive a container limit without carrying cgroup discovery, so the
// decision moves to the composition root, which selects that plugin explicitly.
// The plugin still may not touch runtime/debug itself -- the scan below refuses
// that everywhere outside this directory -- and it must refuse to install when
// another owner already has (automemlimit's Init reports the conflict instead
// of overwriting the limit).
const archProcessKnobOwner = "runtime"

// Reserved selectors, spelled as the effective-value line and the doctor report
// spell them. runtime.GOMAXPROCS is only reserved in its writing form; the
// read-only GOMAXPROCS(0) is how any package asks what this process was sized
// to, and forbidding that would forbid reading the very value the owner
// installed.
const (
	archSelectorSetGCPercent   = "debug.SetGCPercent"
	archSelectorSetMemoryLimit = "debug.SetMemoryLimit"
	archSelectorGOMAXPROCS     = "runtime.GOMAXPROCS"
)

// archAutomemlimitImportPath is the library that installs the soft memory
// limit. Its Set is reserved for the same reason the three standard library
// calls above are: it changes the budget of every goroutine and allocation in
// the process. Because the call lives in a third-party module, the
// standard-library scan cannot see it, so this guard recognizes it explicitly.
const (
	archAutomemlimitImportPath = "github.com/KimMachineGun/automemlimit/memlimit"
	archSelectorMemlimitSet    = "memlimit.Set"
)

// archAutomemlimitOwner is the single directory allowed to call that setter.
// The exception is narrow on purpose: the member is a plugin whose only
// responsibility is the process memory policy, selected explicitly by the
// composition root, and the call must go through the library -- never
// runtime/debug. A future capability that happens to want the setter is an
// architecture decision, not a configuration change, so this list is an exact
// directory rather than a prefix.
const archAutomemlimitOwner = "extensions/reliability/automemlimit"

// archProcessKnobViolation is one call site that its file is not entitled to.
type archProcessKnobViolation struct {
	file     string
	line     int
	selector string
	detail   string
}

func (v archProcessKnobViolation) String() string {
	return fmt.Sprintf("%s line %d calls %s; %s", v.file, v.line, v.selector, v.detail)
}

// TestArchProcessKnobsAreInstalledOnlyByTheRuntime scans every production Go
// file in the repository and fails on any call to debug.SetGCPercent,
// debug.SetMemoryLimit or runtime.GOMAXPROCS outside runtime/. Test files are
// excluded by the shared walker, so this is a statement about what ships.
//
// The scan is by syntax rather than by type: tests/architecture is part of the
// root module and must not import transport/web (see
// TestArchCorePackagesDoNotImportTransportStacks), so importing the packages it
// polices to reason about them is not available to it. Resolving the import
// alias of each call is enough -- a call that reaches these three selectors
// through the standard library is exactly the call this guard exists to catch,
// whichever package it is written in.
func TestArchProcessKnobsAreInstalledOnlyByTheRuntime(t *testing.T) {
	root := archRepositoryRoot(t)
	fset := token.NewFileSet()
	seen := make(map[string]bool)

	for _, name := range archAllProductionGoFiles(t, root) {
		file, err := parser.ParseFile(fset, filepath.Join(root, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err, "failed to parse %s", name)

		if archProcessKnobOwnerFile(name) {
			for _, call := range archProcessKnobCalls(file, fset) {
				seen[call.selector] = true
			}
			continue
		}
		for _, violation := range archProcessKnobViolations(name, file, fset) {
			assert.Fail(t, "process-level knob owner drift", "%s", violation.String())
		}
	}

	// An owner that no longer calls anything makes every assertion above pass
	// for the wrong reason: the scan would find nothing because nothing
	// installs the knobs at all.
	for _, selector := range []string{
		archSelectorSetGCPercent,
		archSelectorSetMemoryLimit,
		archSelectorGOMAXPROCS,
	} {
		assert.Truef(t, seen[selector],
			"no production file under %s/ calls %s, so this guard may be running empty",
			archProcessKnobOwner, selector)
	}
}

// archProcessKnobOwnerFile reports whether a repository-relative slash path is
// inside the one directory allowed to install the knobs.
func archProcessKnobOwnerFile(name string) bool {
	return path.Dir(name) == archProcessKnobOwner
}

// TestArchThirdPartyProcessKnobSettersAreConfined extends the standard-library
// scan above to the one third-party setter that reaches the same process-wide
// limit. It fails on any call to the automemlimit library's Set outside the one
// declared plugin module, and it fails when that module stops calling it, so
// the exemption cannot become empty permission.
func TestArchThirdPartyProcessKnobSettersAreConfined(t *testing.T) {
	root := archRepositoryRoot(t)
	fset := token.NewFileSet()
	seenOwnerCall := false

	for _, name := range archAllProductionGoFiles(t, root) {
		file, err := parser.ParseFile(fset, filepath.Join(root, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err, "failed to parse %s", name)

		for _, violation := range archAutomemlimitSetterViolations(name, file, fset) {
			assert.Fail(t, "process-level knob owner drift", "%s", violation.String())
		}
		if archAutomemlimitOwnerFile(name) && len(archAutomemlimitSetCalls(file, fset)) > 0 {
			seenOwnerCall = true
		}
	}

	assert.Truef(t, seenOwnerCall,
		"no production file under %s/ calls %s, so the single exemption is empty and this guard would not notice the plugin no longer installing the limit",
		archAutomemlimitOwner, archSelectorMemlimitSet)
}

// archAutomemlimitOwnerFile reports whether a repository-relative slash path
// is the one directory allowed to call the automemlimit setter.
func archAutomemlimitOwnerFile(name string) bool {
	return path.Dir(name) == archAutomemlimitOwner
}

// archAutomemlimitSetterViolations reports every way a file installs the
// process memory limit through the automemlimit library that the file's path
// does not entitle it to: a call through any import alias outside the allowed
// directory, and a dot import anywhere, which would erase the qualifier this
// guard resolves calls through.
func archAutomemlimitSetterViolations(name string, file *ast.File, fset *token.FileSet) []archProcessKnobViolation {
	var violations []archProcessKnobViolation
	if line := archAutomemlimitDotImportLine(file, fset); line != 0 {
		violations = append(violations, archProcessKnobViolation{
			file:     name,
			line:     line,
			selector: archAutomemlimitImportPath,
			detail:   "dot-importing the package that installs the process memory limit hides its calls from this guard, so the setter could be called here without the guard noticing",
		})
		return violations
	}
	if archAutomemlimitOwnerFile(name) {
		return nil
	}
	for _, line := range archAutomemlimitSetCalls(file, fset) {
		violations = append(violations, archProcessKnobViolation{
			file:     name,
			line:     line,
			selector: archSelectorMemlimitSet,
			detail:   "the soft memory limit applies to the whole process; it is installed once by the framework from xbc.runtime, or by the one explicitly selected automemlimit plugin module",
		})
	}
	return violations
}

// archAutomemlimitSetCalls returns the line of every call reaching the
// library's Set through an import alias, so the owner's non-empty check and
// the violation report agree on what counts as a call.
func archAutomemlimitSetCalls(file *ast.File, fset *token.FileSet) []int {
	aliases := archImportAliases(file, archAutomemlimitImportPath)
	if len(aliases) == 0 {
		return nil
	}
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := archUnwrapExpression(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualified, ok := selector.X.(*ast.Ident)
		if !ok || !aliases[qualified.Name] || selector.Sel.Name != "Set" {
			return true
		}
		lines = append(lines, fset.Position(selector.Pos()).Line)
		return true
	})
	return lines
}

// archAutomemlimitDotImportLine returns the line of a dot import of the
// automemlimit package, or zero when the file imports it any other way.
func archAutomemlimitDotImportLine(file *ast.File, fset *token.FileSet) int {
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil || importPath != archAutomemlimitImportPath {
			continue
		}
		if specification.Name != nil && specification.Name.Name == "." {
			return fset.Position(specification.Pos()).Line
		}
	}
	return 0
}

// archProcessKnobViolations reports every process-level knob call in one
// production file that the file's path does not entitle it to make.
func archProcessKnobViolations(name string, file *ast.File, fset *token.FileSet) []archProcessKnobViolation {
	if archProcessKnobOwnerFile(name) {
		return nil
	}

	var violations []archProcessKnobViolation
	// A dot import erases the qualifier this guard resolves calls through, so
	// it is refused outright rather than allowed to hide a call.
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil || specification.Name == nil || specification.Name.Name != "." {
			continue
		}
		if importPath != "runtime" && importPath != "runtime/debug" {
			continue
		}
		violations = append(violations, archProcessKnobViolation{
			file:     name,
			line:     fset.Position(specification.Pos()).Line,
			selector: importPath,
			detail:   "dot-importing the package that holds a process-level knob hides its calls from this guard, so the knob could be installed here without the guard noticing",
		})
	}

	for _, call := range archProcessKnobCalls(file, fset) {
		if call.readOnly {
			continue
		}
		violations = append(violations, archProcessKnobViolation{
			file:     name,
			line:     call.line,
			selector: call.selector,
			detail:   archProcessKnobDetail(call),
		})
	}
	return violations
}

func archProcessKnobDetail(call archProcessKnobCall) string {
	if call.computed {
		return "the count is not a literal, so this guard cannot prove it is the read-only runtime.GOMAXPROCS(0) form; the processor count, the GC percentage and the soft memory limit are installed once during bootstrap from xbc.runtime"
	}
	return "this knob applies to the whole process, and the framework installs it once during bootstrap from xbc.runtime"
}

// archProcessKnobCall is one call to a reserved process-level knob.
type archProcessKnobCall struct {
	selector string
	line     int
	// readOnly marks the one form every package may use:
	// runtime.GOMAXPROCS(0), which reads what the owner installed.
	readOnly bool
	// computed marks a GOMAXPROCS argument that is not a literal integer.
	computed bool
}

// archProcessKnobCalls returns every reserved process-level knob call in file,
// read-only ones included, so the presence check in the test above can see that
// the owner still calls all three.
func archProcessKnobCalls(file *ast.File, fset *token.FileSet) []archProcessKnobCall {
	debugAliases := archImportAliases(file, "runtime/debug")
	runtimeAliases := archImportAliases(file, "runtime")

	var calls []archProcessKnobCall
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := archUnwrapExpression(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualified, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		line := fset.Position(selector.Pos()).Line
		switch {
		case debugAliases[qualified.Name] &&
			(selector.Sel.Name == "SetGCPercent" || selector.Sel.Name == "SetMemoryLimit"):
			calls = append(calls, archProcessKnobCall{
				selector: "debug." + selector.Sel.Name,
				line:     line,
			})
		case runtimeAliases[qualified.Name] && selector.Sel.Name == "GOMAXPROCS":
			count, literal := archProcessKnobCount(call)
			calls = append(calls, archProcessKnobCall{
				selector: archSelectorGOMAXPROCS,
				line:     line,
				readOnly: literal && count == 0,
				computed: !literal,
			})
		}
		return true
	})
	return calls
}

// archProcessKnobCount reads the single integer argument of a call. Anything
// that is not a literal integer -- a variable, a field, another call -- is
// reported as not literal, which is what makes the guard refuse to guess rather
// than assume a computed count happens to be zero.
func archProcessKnobCount(call *ast.CallExpr) (int64, bool) {
	if len(call.Args) != 1 {
		return 0, false
	}
	return archProcessKnobIntLiteral(call.Args[0])
}

func archProcessKnobIntLiteral(expression ast.Expr) (int64, bool) {
	switch expression := archUnwrapExpression(expression).(type) {
	case *ast.BasicLit:
		if expression.Kind != token.INT {
			return 0, false
		}
		value, err := strconv.ParseInt(expression.Value, 0, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	case *ast.UnaryExpr:
		if expression.Op != token.SUB {
			return 0, false
		}
		value, ok := archProcessKnobIntLiteral(expression.X)
		return -value, ok
	default:
		return 0, false
	}
}

// TestArchProcessKnobDetection proves the guard actually recognizes the calls
// it forbids, and -- just as important -- does not fire on the shapes that must
// keep working: a read of the installed count, and an import of runtime/debug
// for anything other than these two setters, which several production files
// legitimately do for debug.Stack.
func TestArchProcessKnobDetection(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		source string
		want   []string
	}{
		"gc percent": {
			source: `package p
import "runtime/debug"
func configure() { debug.SetGCPercent(200) }
`,
			want: []string{archSelectorSetGCPercent},
		},
		"memory limit": {
			source: `package p
import "runtime/debug"
func configure() { debug.SetMemoryLimit(1 << 30) }
`,
			want: []string{archSelectorSetMemoryLimit},
		},
		"processor count": {
			source: `package p
import "runtime"
func configure() { runtime.GOMAXPROCS(4) }
`,
			want: []string{archSelectorGOMAXPROCS},
		},
		"processor count through an alias": {
			source: `package p
import goruntime "runtime"
func configure() { goruntime.GOMAXPROCS(4) }
`,
			want: []string{archSelectorGOMAXPROCS},
		},
		"processor count computed from configuration": {
			source: `package p
import "runtime"
func configure(count int) { runtime.GOMAXPROCS(count) }
`,
			want: []string{archSelectorGOMAXPROCS},
		},
		"read only processor count": {
			source: `package p
import "runtime"
func observe() int { return runtime.GOMAXPROCS(0) }
`,
		},
		"stack traces keep working": {
			source: `package p
import "runtime/debug"
func observe() []byte { return debug.Stack() }
`,
		},
		"every forbidden call at once": {
			source: `package p
import (
	"runtime"
	"runtime/debug"
)

func configure() {
	debug.SetGCPercent(200)
	debug.SetMemoryLimit(1 << 30)
	runtime.GOMAXPROCS(4)
}
`,
			want: []string{archSelectorSetGCPercent, archSelectorSetMemoryLimit, archSelectorGOMAXPROCS},
		},
		"dot import hides the calls": {
			source: `package p
import . "runtime/debug"
func configure() { SetGCPercent(200) }
`,
			want: []string{"runtime/debug"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, fset := archParseSyntheticFile(t, testCase.source)
			var selectors []string
			for _, violation := range archProcessKnobViolations("extensions/example/knobs.go", file, fset) {
				selectors = append(selectors, violation.selector)
			}
			assert.Equal(t, testCase.want, selectors)
		})
	}
}

// TestArchProcessKnobOwnerExemptionHolds drives the same entry point the
// repository scan uses, so the exemption is exercised rather than assumed: the
// identical call is refused in an extension and accepted in the knob owner.
func TestArchProcessKnobOwnerExemptionHolds(t *testing.T) {
	t.Parallel()
	const source = `package runtime
import "runtime/debug"
func apply() { debug.SetGCPercent(200) }
`
	file, fset := archParseSyntheticFile(t, source)

	refused := archProcessKnobViolations("extensions/example/knobs.go", file, fset)
	require.Len(t, refused, 1)
	assert.Contains(t, refused[0].String(), "debug.SetGCPercent")
	assert.Empty(t, archProcessKnobViolations("runtime/runtime_knobs.go", file, fset),
		"the knob owner is the one place these calls belong")

	assert.True(t, archProcessKnobOwnerFile("runtime/runtime_knobs.go"))
	assert.True(t, archProcessKnobOwnerFile("runtime/bootstrap.go"))
	assert.False(t, archProcessKnobOwnerFile("plugin/assembly/plan.go"))
	assert.False(t, archProcessKnobOwnerFile("extensions/example/runtime/knobs.go"),
		"a directory merely called runtime is not the knob owner")
	assert.False(t, archProcessKnobOwnerFile("runtime.go"),
		"a file merely called runtime is not the knob owner")
}

// TestArchAutomemlimitSetterDetection is the positive control for the
// third-party half of this guard: it proves the scan recognizes the call it
// forbids, the alias and dot-import shapes that could hide it, and the shapes
// that must keep working.
func TestArchAutomemlimitSetterDetection(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		file   string
		source string
		want   []string
	}{
		"the call this guard exists for": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import "github.com/KimMachineGun/automemlimit/memlimit"
func configure() { _, _ = memlimit.Set() }
`,
			want: []string{archSelectorMemlimitSet},
		},
		"an aliased import cannot hide it": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import limits "github.com/KimMachineGun/automemlimit/memlimit"
func configure() { _, _ = limits.Set() }
`,
			want: []string{archSelectorMemlimitSet},
		},
		"the declared plugin module is accepted": {
			file: archAutomemlimitOwner + "/plugin.go",
			source: `package automemlimit
import "github.com/KimMachineGun/automemlimit/memlimit"
func apply() { _, _ = memlimit.Set() }
`,
		},
		"a dot import is refused even in the plugin module": {
			file: archAutomemlimitOwner + "/plugin.go",
			source: `package automemlimit
import . "github.com/KimMachineGun/automemlimit/memlimit"
func apply() { _, _ = Set() }
`,
			want: []string{archAutomemlimitImportPath},
		},
		"other selectors on the same package are not setters": {
			file: "extensions/jobs/cron/plugin.go",
			source: `package cron
import "github.com/KimMachineGun/automemlimit/memlimit"
var ratio = memlimit.WithRatio(0.5)
`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, fset := archParseSyntheticFile(t, testCase.source)
			var selectors []string
			for _, violation := range archAutomemlimitSetterViolations(testCase.file, file, fset) {
				selectors = append(selectors, violation.selector)
			}
			assert.Equal(t, testCase.want, selectors)
		})
	}
}

func archParseSyntheticFile(t *testing.T, source string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", source, parser.SkipObjectResolution)
	require.NoError(t, err, "failed to parse the synthetic file this control is built from")
	return file, fset
}
