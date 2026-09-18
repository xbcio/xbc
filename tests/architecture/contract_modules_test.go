package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archContractModules is the complete, intentional set of contract-only modules
// in the repository, as slash-separated paths relative to the repository root.
//
// A contract module publishes a protocol-neutral vocabulary of interfaces and
// value types and owns no Definition, no Config, and no Bundle -- which is
// exactly what TestArchContractModulesOwnNoDefinitionConfigOrBundle asserts, so
// that this comment is a checkable claim rather than a convention. The
// zero-dependency property is what gives the category its value: any plugin, any
// transport, and any application composition root can depend on a contract
// module without inheriting a dependency closure, which is why the modules below
// import nothing outside the standard library.
//
// A contract module sits either directly beneath the extensions namespace
// (authentication) or inside one of its capability groups (coordination/lease).
// It cannot live in core, whose dependency closure must exclude everything
// beneath extensions, and it cannot be a capability group leaf beside plugins,
// because it is not a plugin itself.
//
// Every consumer that must tell a contract module apart from a plugin reads this
// list -- archPluginImplementationRoots is the one today -- so adding a member
// is a single explicit edit here plus archProtocolNeutralContractModules when
// the new module sits directly in the namespace. Adding one is an architecture
// decision.
var archContractModules = []string{
	"extensions/authentication",
	"extensions/coordination/lease",
}

// archContractModuleRoots returns the absolute path of every declared contract
// module, keyed for exclusion lookups.
func archContractModuleRoots(repositoryRoot string) map[string]bool {
	roots := make(map[string]bool, len(archContractModules))
	for _, path := range archContractModules {
		roots[filepath.Join(repositoryRoot, filepath.FromSlash(path))] = true
	}
	return roots
}

// TestArchContractModuleDeclarationsAgree keeps the two spellings of the
// category from drifting. archProtocolNeutralContractModules names the members
// that sit directly in the extensions namespace, because the layout guard
// compares them against the namespace's actual direct children; this list names
// every member wherever it sits. A module added to one and not the other would
// otherwise be excluded from the plugin roots while never being asserted to be a
// contract module at all.
func TestArchContractModuleDeclarationsAgree(t *testing.T) {
	t.Parallel()
	repositoryRoot := archRepositoryRoot(t)

	var directChildren []string
	for _, path := range archContractModules {
		assert.Truef(t, strings.HasPrefix(path, "extensions/"),
			"%s must live beneath the extensions namespace; a contract module exists so that anything may depend on it without inheriting a closure, which core's own rules do not allow", path)
		segments := strings.Split(strings.TrimPrefix(path, "extensions/"), "/")
		if len(segments) == 1 {
			directChildren = append(directChildren, segments[0])
		}
		info, err := filepath.Glob(filepath.Join(repositoryRoot, filepath.FromSlash(path), "go.mod"))
		require.NoError(t, err)
		require.Len(t, info, 1, "%s must be an independently versioned module", path)
	}

	sort.Strings(directChildren)
	declared := append([]string(nil), archProtocolNeutralContractModules...)
	sort.Strings(declared)
	assert.Equal(t, declared, directChildren,
		"archContractModules and archProtocolNeutralContractModules disagree about which contract modules sit directly beneath extensions")
}

// TestArchContractModulesOwnNoDefinitionConfigOrBundle turns the category's
// defining claim into an assertion. Without it, adding a name to
// archContractModules would buy a plugin an exemption from every
// plugin-implementation guard in this suite while proving nothing about it.
func TestArchContractModulesOwnNoDefinitionConfigOrBundle(t *testing.T) {
	t.Parallel()
	repositoryRoot := archRepositoryRoot(t)

	for _, path := range archContractModules {
		directory := filepath.Join(repositoryRoot, filepath.FromSlash(path))
		files := archParseProductionGoFiles(t, directory)
		require.NotEmptyf(t, files, "contract module %s contains no production Go file, so this guard would prove nothing about it", path)
		assert.Emptyf(t, archContractModuleViolations(files),
			"contract module %s must own no Definition, Config, or Bundle", path)
	}
}

// TestArchContractModuleDetectorRejectsEveryShape is the positive control for
// the guard above. A detector that reported nothing for every input would pass
// two clean modules and a violated one alike, so each shape the guard claims to
// catch is driven over synthetic source here, together with a clean module that
// must stay accepted.
func TestArchContractModuleDetectorRejectsEveryShape(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		source     string
		violations []string
	}{
		"vocabulary only": {
			source: `package fixture

import (
	"context"
	"time"
)

type Locker interface {
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

type Lease interface{ Key() string }
`,
		},
		"unaliased definition": {
			source: `package fixture

import "github.com/xbcio/xbc/plugin"

var definition = plugin.Define("contract", nil)
`,
			violations: []string{"var definition"},
		},
		"definition under an alias": {
			source: `package fixture

import xbcplugin "github.com/xbcio/xbc/plugin"

var planned = xbcplugin.DefinePlanned[struct{}, struct{}]("contract", xbcplugin.ConfigSpec[struct{}]{}, nil)
`,
			violations: []string{"var planned"},
		},
		"bundle accessor": {
			source: `package fixture

func Bundle() struct{} { return struct{}{} }
`,
			violations: []string{"func Bundle"},
		},
		"config type": {
			source: `package fixture

// Config configures nothing; a contract module has no configuration.
type Config struct { Enabled bool }
`,
			violations: []string{"type Config"},
		},
		"named config type": {
			source: `package fixture

type LeaseConfig struct{ Instance string }
`,
			violations: []string{"type LeaseConfig"},
		},
		"plugin package import": {
			source: `package fixture

import "github.com/xbcio/xbc/plugin"

var key = plugin.Key("contract")
`,
			violations: []string{archPluginImportPath},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", testCase.source, parser.SkipObjectResolution)
			require.NoError(t, err, "parsing the fixture source failed")

			violations := archContractModuleViolations([]*ast.File{file})
			if len(testCase.violations) == 0 {
				assert.Emptyf(t, violations, "a vocabulary-only contract module must be accepted, got %v", violations)
				return
			}
			require.NotEmptyf(t, violations, "the guard failed to reject a contract module that owns a Definition, Config, or Bundle")
			for _, expected := range testCase.violations {
				assert.Containsf(t, strings.Join(violations, "\n"), expected,
					"violation report must name %q, got %v", expected, violations)
			}
		})
	}
}

// archContractModuleViolations reports every way files fails to be a contract
// module: a Definition handle, a Bundle accessor, a configuration type, or any
// dependence on the package those three are built from.
//
// It is a pure function over parsed files so that the guard and its positive
// control judge the same code, and so that it can be driven over synthetic
// source a real module would never contain.
func archContractModuleViolations(files []*ast.File) []string {
	var violations []string
	for _, file := range files {
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err == nil && (importPath == archPluginImportPath || strings.HasPrefix(importPath, archPluginImportPath+"/")) {
				violations = append(violations, "imports "+importPath)
			}
		}

		topValues := archCollectTopValues([]*ast.File{file})
		names := make([]string, 0, len(topValues))
		for name := range topValues {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value := topValues[name]
			if archIsDefinitionConstructor(value.file, value.initializer) {
				violations = append(violations, value.kind.String()+" "+name)
			}
		}

		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil && declaration.Name.Name == "Bundle" {
					violations = append(violations, "func Bundle")
				}
			case *ast.GenDecl:
				if declaration.Tok != token.TYPE {
					continue
				}
				for _, raw := range declaration.Specs {
					specification, ok := raw.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if name := specification.Name.Name; name == "Config" || strings.HasSuffix(name, "Config") {
						violations = append(violations, "type "+name)
					}
				}
			}
		}
	}
	return violations
}
