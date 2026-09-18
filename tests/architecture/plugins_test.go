package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	archPluginImportPath       = "github.com/xbcio/xbc/plugin"
	archPluginAutoloadPath     = "github.com/xbcio/xbc/plugin/autoload"
	archRetiredCatalogPath     = "github.com/xbcio/xbc/plugin/catalog"
	archRepositoryImportPrefix = "github.com/xbcio/xbc/"
	archWebImportPath          = "github.com/xbcio/xbc/transport/web"
)

// archManagementEndpointPackages are the built-in Web extensions that expose an
// operator endpoint. Their access control belongs to the application's
// authentication policy, not to the route declaration.
//
// swag and health are deliberately excluded from this list even though they
// also declare Auth(web.Public()): they expose API description and liveness
// surfaces, not an operator control plane, and their Public() is a tier-2
// declaration that an application can still override with a tier-1
// web.security rule. Do not add them here.
var archManagementEndpointPackages = []string{
	filepath.Join("transport", "web", "extensions", "observability", "metrics"),
	filepath.Join("transport", "web", "extensions", "observability", "pprof"),
	filepath.Join("transport", "web", "extensions", "reliability", "gracefulshutdown"),
}

// archWebAuthPolicyConstructors are the web package functions that build a
// route-level authentication policy.
var archWebAuthPolicyConstructors = map[string]bool{
	"Public":  true,
	"Accepts": true,
}

// archRouteRegistrationMethods are the Router methods that contribute a route.
// TestArchRouterExposesNoUnguardedRegistrationMethod holds this list to
// Router's actual exported surface in both directions, so a name here that no
// longer exists, and a new registration method that never got added, both fail.
var archRouteRegistrationMethods = map[string]bool{
	"GET":     true,
	"POST":    true,
	"PUT":     true,
	"PATCH":   true,
	"DELETE":  true,
	"HEAD":    true,
	"OPTIONS": true,
	"Handle":  true,
	"Any":     true,
	"Match":   true,
}

// archRouterNonRegistrationMethods are Router's remaining exported methods:
// the ones that do not add a row to the route table. Group derives a
// sub-router that shares the parent's table rather than registering anything
// itself, so it belongs here instead. Perm and Auth set this Router's
// group-level policy default for routes registered afterward -- they return
// *Router, not *Route, so they belong here too even though they influence
// what Handle later writes into a RouteInfo.
var archRouterNonRegistrationMethods = map[string]bool{
	"Group": true,
	"Perm":  true,
	"Auth":  true,
}

type archTopValue struct {
	file        *ast.File
	initializer ast.Expr
	kind        token.Token
}

type archFunction struct {
	file        *ast.File
	declaration *ast.FuncDecl
}

// TestArchPluginImplementationsExposeCanonicalComposition keeps every reusable
// implementation on the one public composition model: a package-level
// canonical Definition handle, a trivial Definition accessor, and a
// side-effect-free Bundle accessor. Config, concrete implementation, and New
// are intentionally not uniform requirements; not every Plugin needs them.
func TestArchPluginImplementationsExposeCanonicalComposition(t *testing.T) {
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		implementationRoot := implementationRoot
		t.Run(filepath.ToSlash(implementationRoot), func(t *testing.T) {
			files := archParseProductionGoFiles(t, implementationRoot)
			require.NotEmpty(t, files, "%s must contain production Go files", implementationRoot)

			topValues := archCollectTopValues(files)
			canonical := make(map[string]bool)
			for name, value := range topValues {
				if value.kind == token.VAR && archIsDefinitionConstructor(value.file, value.initializer) {
					canonical[name] = true
				}
			}
			require.NotEmpty(t, canonical, "%s must declare a package-level plugin.Define* handle", implementationRoot)

			var definitionAccessors []*ast.FuncDecl
			var bundleAccessors []archFunction
			for _, file := range files {
				for _, declaration := range file.Decls {
					function, ok := declaration.(*ast.FuncDecl)
					if !ok || function.Recv != nil {
						continue
					}
					switch {
					case function.Name.Name == "Definition" && archReturnsType(file, function, archPluginImportPath, "Definition"):
						definitionAccessors = append(definitionAccessors, function)
					case function.Name.Name == "Bundle" && archReturnsType(file, function, archPluginImportPath, "Bundle"):
						bundleAccessors = append(bundleAccessors, archFunction{file: file, declaration: function})
					}
				}
			}

			require.Len(t, definitionAccessors, 1, "%s must expose exactly one Definition() plugin.Definition accessor", implementationRoot)
			returned, ok := archSingleReturnedExpression(definitionAccessors[0])
			require.True(t, ok, "%s Definition() must contain only one return statement", implementationRoot)
			identifier, ok := returned.(*ast.Ident)
			require.True(t, ok, "%s Definition() must return a package-level identifier, never call plugin.Define*", implementationRoot)
			assert.Truef(t, canonical[identifier.Name], "%s Definition() returns %q, which is not a package-level plugin.Define* handle", implementationRoot, identifier.Name)

			require.Len(t, bundleAccessors, 1, "%s must expose exactly one Bundle() plugin.Bundle accessor", implementationRoot)
			bundleExpression, ok := archSingleReturnedExpression(bundleAccessors[0].declaration)
			require.True(t, ok, "%s Bundle() must contain only one return statement", implementationRoot)
			assert.Truef(t, archIsSideEffectFreeBundleExpression(bundleAccessors[0].file, bundleExpression, topValues, make(map[string]bool)), "%s Bundle() must only return a package-level Bundle or compose canonical handles", implementationRoot)
		})
	}
}

// TestArchPluginAutoloadPackagesAreLeafAdapters keeps import-time mutation out
// of implementation packages. Each optional leaf adapter has exactly one init,
// and that init can only declare its parent's canonical Bundle.
func TestArchPluginAutoloadPackagesAreLeafAdapters(t *testing.T) {
	root := archRepositoryRoot(t)
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		parentImport := archRepositoryImportPrefix + filepath.ToSlash(mustRelativePath(t, root, implementationRoot))
		archAssertAutoloadAdapter(t, filepath.Join(implementationRoot, "autoload"), parentImport)
	}

	// The Web aggregate adapter intentionally declares the side-effect-free
	// Prelude Bundle rather than the server package's narrower Bundle.
	archAssertAutoloadAdapter(
		t,
		filepath.Join(root, "transport", "web", "autoload"),
		"github.com/xbcio/xbc/transport/web/prelude",
	)
}

func archAssertAutoloadAdapter(t *testing.T, directory, parentImport string) {
	t.Helper()
	t.Run(filepath.ToSlash(directory), func(t *testing.T) {
		files := archParseProductionGoFiles(t, directory)
		require.NotEmpty(t, files, "%s must provide an explicit autoload package", directory)

		initCount := 0
		for _, file := range files {
			for _, specification := range file.Imports {
				importPath, err := strconv.Unquote(specification.Path.Value)
				require.NoError(t, err)
				assert.Containsf(t, []string{archPluginAutoloadPath, parentImport}, importPath, "%s leaf adapter may import only plugin/autoload and its parent package", directory)
				if specification.Name != nil {
					assert.NotEqual(t, "_", specification.Name.Name, "%s must call the parent Bundle explicitly, not blank-import it", directory)
				}
			}
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.FuncDecl:
					if declaration.Recv != nil || declaration.Name.Name != "init" {
						assert.Failf(t, "autoload implementation logic", "%s may contain no production function except init", directory)
						continue
					}
					initCount++
					assert.Truef(t, archIsCanonicalAutoloadInit(file, declaration, parentImport), "%s init must contain only plugin/autoload.Declare(parent.Bundle())", directory)
				case *ast.GenDecl:
					if declaration.Tok != token.IMPORT {
						assert.Failf(t, "autoload state", "%s may contain no production declarations except imports and init", directory)
					}
				}
			}
		}
		assert.Equal(t, 1, initCount, "%s must contain exactly one init adapter", directory)
	})
}

// TestArchAutoloadMutationIsConfined keeps the process-global optional adapter
// confined to runtime and declared leaf autoload packages. Prelude
// and ordinary implementation packages remain safe to import and call directly.
func TestArchAutoloadMutationIsConfined(t *testing.T) {
	root := archRepositoryRoot(t)
	allowedOwners := map[string]bool{
		"runtime":                true,
		"transport/web/autoload": true,
	}
	for _, implementationRoot := range archPluginImplementationRoots(t) {
		relative, err := filepath.Rel(root, filepath.Join(implementationRoot, "autoload"))
		require.NoError(t, err)
		allowedOwners[filepath.ToSlash(relative)] = true
	}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			require.NoError(t, err)
			assert.NotEqual(t, archRetiredCatalogPath, importPath, "%s must not revive the retired public catalog", path)
			if importPath != archPluginAutoloadPath {
				continue
			}
			relative, err := filepath.Rel(root, filepath.Dir(path))
			require.NoError(t, err)
			owner := filepath.ToSlash(relative)
			assert.Truef(t, allowedOwners[owner], "%s imports plugin/autoload outside a declared leaf adapter or runtime", path)
		}
		return nil
	})
	require.NoError(t, err)
}

// TestArchWebPreludeIsSideEffectFree pins Prelude as ordinary composition
// content. Merely importing it must not register or activate anything.
func TestArchWebPreludeIsSideEffectFree(t *testing.T) {
	preludeRoot := filepath.Join(archRepositoryRoot(t), "transport", "web", "prelude")
	files := archParseProductionGoFiles(t, preludeRoot)
	require.NotEmpty(t, files, "Web prelude must contain production Go files")
	topValues := archCollectTopValues(files)

	var bundleAccessors []archFunction
	for _, file := range files {
		for _, specification := range file.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			require.NoError(t, err)
			assert.NotEqual(t, archPluginAutoloadPath, importPath, "Prelude must not mutate plugin autoload")
			assert.NotEqual(t, archRetiredCatalogPath, importPath, "Prelude must not import the retired catalog")
			if specification.Name != nil {
				assert.NotEqual(t, "_", specification.Name.Name, "Prelude must compose explicit Bundles, not blank imports")
			}
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			assert.NotEqual(t, "init", function.Name.Name, "Prelude must have no import-time side effects")
			if function.Recv == nil && function.Name.Name == "Bundle" && archReturnsType(file, function, archPluginImportPath, "Bundle") {
				bundleAccessors = append(bundleAccessors, archFunction{file: file, declaration: function})
			}
		}
	}
	require.Len(t, bundleAccessors, 1, "Prelude must expose exactly one Bundle() plugin.Bundle")
	expression, ok := archSingleReturnedExpression(bundleAccessors[0].declaration)
	require.True(t, ok, "Prelude Bundle() must contain only one return statement")
	assert.True(t, archIsSideEffectFreeBundleExpression(bundleAccessors[0].file, expression, topValues, make(map[string]bool)), "Prelude Bundle() may only combine explicit child Bundles")
}

// TestArchWorkloadBundlesAreSideEffectFreeComposition pins the workload branch
// of the composition scanner against both directions.
//
// The scanner classifies a Bundle() accessor by reading its source, so a branch
// nothing exercises protects nothing: a scanner that rejected every WorkloadOf
// call and one that accepted every call shape would both pass a corpus that
// contains no workload Bundle at all. The cases below therefore drive the real
// scanner over real source, and the rejected ones are the shapes the branch
// exists to catch -- a key that has to be computed, an unrecognized placement
// option, and a member that is not a package-level Definition handle.
func TestArchWorkloadBundlesAreSideEffectFreeComposition(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		expression string
		accepted   bool
	}{
		"package-level key constant": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.BundleOf(memberDefinition))`,
			accepted:   true,
		},
		"string literal key and placement options": {
			expression: `plugin.WorkloadOf("sast", plugin.BundleOf(memberDefinition), plugin.WithExclusiveProcess(), plugin.WithReplicas(3))`,
			accepted:   true,
		},
		"key from another repository package": {
			expression: `plugin.WorkloadOf(sibling.Key, plugin.BundleOf(memberDefinition))`,
			accepted:   true,
		},
		"nested composition": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.CombineBundles(plugin.BundleOf(memberDefinition), canonicalWorkload))`,
			accepted:   true,
		},
		"package-level composition variable": {
			expression: `canonicalWorkload`,
			accepted:   true,
		},
		"empty workload": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.BundleOf())`,
			accepted:   true,
		},
		"computed key": {
			expression: `plugin.WorkloadOf(workloadKeyFor("sast"), plugin.BundleOf(memberDefinition))`,
			accepted:   false,
		},
		"unknown placement option": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.BundleOf(memberDefinition), plugin.WithSomethingElse())`,
			accepted:   false,
		},
		"bare option argument": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.BundleOf(memberDefinition), options...)`,
			accepted:   false,
		},
		"member built at call time": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.BundleOf(newMemberDefinition()))`,
			accepted:   false,
		},
		"local variable instead of a member": {
			expression: `plugin.WorkloadOf(workloadKey, plugin.BundleOf(undeclaredDefinition))`,
			accepted:   false,
		},
		"missing bundle argument": {
			expression: `plugin.WorkloadOf(workloadKey)`,
			accepted:   false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, topValues := archParseFixturePackage(t, "func Bundle() plugin.Bundle { return "+testCase.expression+" }")
			expression, ok := archSingleReturnedExpression(archFindFunction(t, file, "Bundle"))
			require.True(t, ok, "the fixture Bundle() must contain exactly one return statement")

			assert.Equal(t, testCase.accepted,
				archIsSideEffectFreeBundleExpression(file, expression, topValues, make(map[string]bool)),
				"Bundle() returning %s", testCase.expression)
		})
	}
}

// archParseFixturePackage parses one synthetic package in memory and returns
// its file together with its package-level values. It exists because the
// scanner's workload branch has no production call site yet: the only way to
// prove it classifies a WorkloadOf call correctly is to hand it one.
func archParseFixturePackage(t *testing.T, declarations ...string) (*ast.File, map[string]archTopValue) {
	t.Helper()
	source := `package fixture

import (
	"github.com/xbcio/xbc/plugin"
	sibling "github.com/xbcio/xbc/extensions/reliability/health"
)

const workloadKey plugin.WorkloadKey = "sast"

var memberDefinition = plugin.Define("member", nil)

var canonicalWorkload = plugin.WorkloadOf(workloadKey, plugin.BundleOf(memberDefinition))

var options = []plugin.WorkloadOption{}

func workloadKeyFor(name string) plugin.WorkloadKey { return plugin.WorkloadKey(name) }

func newMemberDefinition() plugin.Definition { return memberDefinition }
`
	for _, declaration := range declarations {
		source += "\n" + declaration + "\n"
	}
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, parser.SkipObjectResolution)
	require.NoError(t, err, "parsing the architecture fixture package failed")
	return file, archCollectTopValues([]*ast.File{file})
}

// archFindFunction returns the named top-level function of file.
func archFindFunction(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && function.Name.Name == name {
			return function
		}
	}
	require.FailNowf(t, "fixture function missing", "the fixture package declares no func %s", name)
	return nil
}

// TestArchPluginImplementationsDocumentUsage keeps package documentation useful
// as the Plugin ecosystem grows. Every reusable implementation must keep a
// discoverable doc.go package comment with a Usage section and code block.
func TestArchPluginImplementationsDocumentUsage(t *testing.T) {
	for _, moduleRoot := range archPluginImplementationRoots(t) {
		t.Run(filepath.ToSlash(moduleRoot), func(t *testing.T) {
			docPath := filepath.Join(moduleRoot, "doc.go")
			source, err := os.ReadFile(docPath)
			require.NoError(t, err, "%s must provide doc.go", moduleRoot)

			file, err := parser.ParseFile(token.NewFileSet(), docPath, source, parser.ParseComments|parser.SkipObjectResolution)
			require.NoError(t, err, "parse %s", docPath)
			require.NotNil(t, file.Doc, "%s must contain the package comment", docPath)

			usageFound, codeFound := archDocUsage(file.Doc)
			assert.Truef(t, usageFound, "%s package comment must contain a '# Usage' heading", docPath)
			assert.Truef(t, codeFound, "%s '# Usage' section must contain an indented Go code block", docPath)
		})
	}
}

// TestArchManagementEndpointsDeclareNoAuthenticationExemption keeps the
// built-in management endpoints on the application's authentication policy.
// These plugins expose operator surfaces (metrics, pprof, shutdown) and must
// not carry their own exemption: a route-level web.Public() declaration
// outranks the security default, so re-adding one here would silently reopen
// an operator endpoint that the deny default is meant to protect. The
// companion runtime guard lives in transport/web
// (TestOpenTrafficFailsWhenARouteFallsToDenyWithoutAuthenticator); this one
// catches the source-level regression directly, in the packages that own it.
func TestArchManagementEndpointsDeclareNoAuthenticationExemption(t *testing.T) {
	root := archRepositoryRoot(t)
	for _, relative := range archManagementEndpointPackages {
		t.Run(filepath.ToSlash(relative), func(t *testing.T) {
			files := archParseProductionGoFiles(t, filepath.Join(root, relative))
			require.NotEmpty(t, files, "%s must contain production Go files", relative)

			contributors := 0
			registrations := 0
			for _, file := range files {
				webAliases := archImportAliases(file, archWebImportPath)
				ast.Inspect(file, func(node ast.Node) bool {
					selector, ok := node.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if selector.Sel.Name == "Auth" {
						assert.Failf(t, "route-level authentication declaration",
							"%s must leave authentication to the application policy; found .Auth(...)", relative)
						return true
					}
					identifier, ok := selector.X.(*ast.Ident)
					if !ok || !webAliases[identifier.Name] {
						return true
					}
					if archWebAuthPolicyConstructors[selector.Sel.Name] {
						assert.Failf(t, "route-level authentication exemption",
							"%s must not construct its own authentication policy; found web.%s",
							relative, selector.Sel.Name)
					}
					return true
				})
				for _, declaration := range file.Decls {
					function, ok := declaration.(*ast.FuncDecl)
					if !ok || function.Recv == nil || function.Name.Name != "RegisterRoutes" {
						continue
					}
					contributors++
					ast.Inspect(function.Body, func(node ast.Node) bool {
						call, ok := node.(*ast.CallExpr)
						if !ok {
							return true
						}
						if selector, ok := call.Fun.(*ast.SelectorExpr); ok && archRouteRegistrationMethods[selector.Sel.Name] {
							registrations++
						}
						return true
					})
				}
			}
			// Positive controls: the guard above only proves an absence, so it
			// would also pass on a package that stopped registering routes at
			// all, or renamed RegisterRoutes out from under the scan.
			require.Equal(t, 1, contributors, "%s must keep exactly one RegisterRoutes route contributor", relative)
			require.NotZero(t, registrations, "%s RegisterRoutes must still register routes", relative)
		})
	}
}

// TestArchRouterExposesNoUnguardedRegistrationMethod pins Router's exported
// surface to the two lists above. It exists because
// archRouteRegistrationMethods is the input to a scan that counts route
// registrations, so the list being wrong is invisible at the call site: a
// registration method missing from it makes that scan silently undercount,
// and a name in it that Router never had makes the list read as coverage it
// does not have. Both directions are asserted here.
//
// The structural signal is the return type. Every Router method that appends
// to the route table hands back the *web.Route metadata handle for the rows it
// just added, which is also what makes .Auth and .Perm declarable at the
// registration site. A future method that registers a route without returning
// *Route would therefore evade this guard -- but it would also be undeclarable
// policy-wise, which is the larger design error the reviewer is meant to catch.
func TestArchRouterExposesNoUnguardedRegistrationMethod(t *testing.T) {
	root := archRepositoryRoot(t)
	files := archParseProductionGoFiles(t, filepath.Join(root, "transport", "web"))

	found := make(map[string]bool)
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || archReceiverTypeName(function.Recv) != "Router" || !function.Name.IsExported() {
				continue
			}
			name := function.Name.Name
			found[name] = true
			registers := archReturnsWebRoute(function)
			if registers {
				assert.Truef(t, archRouteRegistrationMethods[name],
					"Router.%s returns *Route, so it registers routes; add it to archRouteRegistrationMethods or the route-registration scans will not see it",
					name)
				continue
			}
			assert.Truef(t, archRouterNonRegistrationMethods[name],
				"Router.%s is exported but is in neither registration list; classify it deliberately",
				name)
		}
	}

	// Positive control: the scan above proves nothing if it matched no method
	// at all, which is exactly what a renamed receiver would produce.
	require.NotEmpty(t, found, "no exported Router methods were found; the scan is looking at the wrong type")
	for name := range archRouteRegistrationMethods {
		assert.Truef(t, found[name],
			"archRouteRegistrationMethods names Router.%s, which does not exist; the list overstates its coverage",
			name)
	}
	for name := range archRouterNonRegistrationMethods {
		assert.Truef(t, found[name],
			"archRouterNonRegistrationMethods names Router.%s, which does not exist", name)
	}
}

// archReturnsWebRoute reports whether function returns the *Route metadata
// handle. The web package declares Route locally, so the result type is a bare
// identifier rather than a qualified one.
func archReturnsWebRoute(function *ast.FuncDecl) bool {
	results := function.Type.Results
	if results == nil || len(results.List) != 1 {
		return false
	}
	star, ok := results.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	identifier, ok := star.X.(*ast.Ident)
	return ok && identifier.Name == "Route"
}

// TestArchPluginContractsAreCurrent guards the committed plugin identity and
// default-value baselines against unreviewed contract drift.
func TestArchPluginContractsAreCurrent(t *testing.T) {
	root := archRepositoryRoot(t)
	command := exec.Command("go", "run", "./scripts/plugin-snapshots", "-check")
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "plugin contract snapshot check failed:\n%s", output)
}

// TestArchPluginMigrationHasNoBlockers generates the AST-backed inventory in
// memory and checks the zero-blocker invariant without committing the report.
func TestArchPluginMigrationHasNoBlockers(t *testing.T) {
	root := archRepositoryRoot(t)
	command := exec.Command("go", "run", "./scripts/plugin-migration-inventory", "-stdout", "-require-zero")
	command.Dir = root
	command.Stdout = io.Discard
	var diagnostics strings.Builder
	command.Stderr = &diagnostics
	err := command.Run()
	require.NoErrorf(t, err, "plugin migration blocker check failed:\n%s", diagnostics.String())
}

func archDocUsage(group *ast.CommentGroup) (usageFound bool, codeFound bool) {
	inUsage := false
	for _, comment := range group.List {
		if !strings.HasPrefix(comment.Text, "//") {
			continue
		}
		line := strings.TrimPrefix(comment.Text, "//")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			inUsage = trimmed == "# Usage"
			usageFound = usageFound || inUsage
			continue
		}
		if inUsage && strings.HasPrefix(line, "\t") && trimmed != "" {
			codeFound = true
		}
	}
	return usageFound, codeFound
}

func archPluginImplementationRoots(t *testing.T) []string {
	t.Helper()
	repositoryRoot := archRepositoryRoot(t)
	webRoot := filepath.Join(repositoryRoot, "transport", "web")
	roots := make([]string, 0, 37)
	for _, extensionPath := range archWebPackageExtensionPaths {
		roots = append(roots, filepath.Join(webRoot, "extensions", filepath.FromSlash(extensionPath)))
	}

	// A contract module lives beneath extensions but publishes a vocabulary
	// rather than a plugin: it owns no Definition, Config, or Bundle, so it has
	// nothing for the plugin-implementation guards below to inspect and must
	// stay out of these roots. archContractModules declares the category once;
	// TestArchContractModulesOwnNoDefinitionConfigOrBundle is what makes the
	// exclusion safe to grant.
	contractRoots := archContractModuleRoots(repositoryRoot)
	var protocolNeutral []string
	for _, moduleRoot := range archExtensionModuleRoots(t, filepath.Join(repositoryRoot, "extensions")) {
		if !contractRoots[moduleRoot] {
			protocolNeutral = append(protocolNeutral, moduleRoot)
		}
	}
	require.Len(t, protocolNeutral, 13, "expected 13 protocol-neutral extension plugins plus the non-plugin contract modules beneath extensions")
	roots = append(roots, protocolNeutral...)

	webAdapter := filepath.Join(webRoot, "extensions", "authorization", "rbac")
	var webPlugins []string
	for _, moduleRoot := range archExtensionModuleRoots(t, filepath.Join(webRoot, "extensions")) {
		if moduleRoot != webAdapter {
			webPlugins = append(webPlugins, moduleRoot)
		}
	}
	require.Len(t, webPlugins, 9, "expected 9 Web extension plugins plus the non-plugin RBAC adapter")
	roots = append(roots, webPlugins...)

	require.Len(t, roots, 37, "expected 15 Web package extensions, 13 protocol-neutral extension plugins, and 9 independently versioned Web extension plugins; the two contract modules beneath extensions are not plugin implementations")
	sort.Strings(roots)
	return roots
}

func archExtensionModuleRoots(t *testing.T, namespace string) []string {
	t.Helper()
	var roots []string
	err := filepath.WalkDir(namespace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "go.mod" {
			roots = append(roots, filepath.Dir(path))
		}
		return nil
	})
	require.NoError(t, err, "discover extension modules beneath %s", namespace)
	require.NotEmpty(t, roots, "extension namespace %s contains no modules", namespace)
	sort.Strings(roots)
	return roots
}

func archParseProductionGoFiles(t *testing.T, directory string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(directory)
	require.NoError(t, err, "read Go package %s", directory)
	files := make([]*ast.File, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, file)
	}
	return files
}

func archCollectTopValues(files []*ast.File) map[string]archTopValue {
	values := make(map[string]archTopValue)
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR && general.Tok != token.CONST {
				continue
			}
			for _, raw := range general.Specs {
				specification, ok := raw.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range specification.Names {
					if index < len(specification.Values) {
						values[name.Name] = archTopValue{file: file, initializer: specification.Values[index], kind: general.Tok}
					} else if len(specification.Values) == 1 {
						values[name.Name] = archTopValue{file: file, initializer: specification.Values[0], kind: general.Tok}
					}
				}
			}
		}
	}
	return values
}

func archIsDefinitionConstructor(file *ast.File, expression ast.Expr) bool {
	call, ok := archUnwrapExpression(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	for _, name := range []string{"Define", "DefineConfigured", "DefinePlanned"} {
		if archCallMatches(file, call, archPluginImportPath, name) {
			return true
		}
	}
	return false
}

func archReturnsType(file *ast.File, function *ast.FuncDecl, importPath, name string) bool {
	return function.Type.Results != nil && len(function.Type.Results.List) == 1 && archTypeMatches(file, function.Type.Results.List[0].Type, importPath, name)
}

func archTypeMatches(file *ast.File, expression ast.Expr, importPath, name string) bool {
	expression = archUnwrapExpression(expression)
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && archImportAliases(file, importPath)[identifier.Name]
}

func archSingleReturnedExpression(function *ast.FuncDecl) (ast.Expr, bool) {
	if function.Body == nil || len(function.Body.List) != 1 {
		return nil, false
	}
	statement, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(statement.Results) != 1 {
		return nil, false
	}
	return archUnwrapExpression(statement.Results[0]), true
}

func archIsSideEffectFreeBundleExpression(file *ast.File, expression ast.Expr, topValues map[string]archTopValue, seen map[string]bool) bool {
	expression = archUnwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.Ident:
		if seen[expression.Name] {
			return false
		}
		value, ok := topValues[expression.Name]
		if !ok || value.kind != token.VAR || value.initializer == nil {
			return false
		}
		seen[expression.Name] = true
		return archIsSideEffectFreeBundleExpression(value.file, value.initializer, topValues, seen)
	case *ast.CallExpr:
		if archIsCompactBundleComposition(file, expression, topValues, seen) || archIsWorkloadOfCall(file, expression, topValues, seen) {
			return true
		}
		// Prelude composes child package Bundle accessors directly.
		selector, ok := archUnwrapExpression(expression.Fun).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Bundle" || len(expression.Args) != 0 {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		return ok && archImportsRepositoryPackage(file, identifier.Name)
	default:
		return false
	}
}

func archIsSideEffectFreeBundleArgument(file *ast.File, expression ast.Expr, topValues map[string]archTopValue, seen map[string]bool) bool {
	expression = archUnwrapExpression(expression)
	switch expression := expression.(type) {
	case *ast.Ident:
		value, ok := topValues[expression.Name]
		if !ok || value.initializer == nil {
			return false
		}
		if archIsDefinitionConstructor(value.file, value.initializer) {
			return true
		}
		if archIsCompositionConstructorInitializer(value.file, value.initializer) {
			return archIsSideEffectFreeBundleExpression(value.file, expression, topValues, seen)
		}
		return false
	case *ast.CallExpr:
		if archIsCompactBundleComposition(file, expression, topValues, seen) || archIsWorkloadOfCall(file, expression, topValues, seen) {
			return true
		}
		if identifier, ok := archUnwrapExpression(expression.Fun).(*ast.Ident); ok && identifier.Name == "Definition" && len(expression.Args) == 0 {
			return true
		}
		selector, ok := archUnwrapExpression(expression.Fun).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Bundle" || len(expression.Args) != 0 {
			return false
		}
		identifier, ok := selector.X.(*ast.Ident)
		return ok && archImportsRepositoryPackage(file, identifier.Name)
	default:
		return false
	}
}

// archCompactCompositionConstructors are the plugin constructors whose every
// argument is itself composition data, so one rule validates the whole call.
var archCompactCompositionConstructors = []string{"BundleOf", "CombineBundles"}

// archWorkloadOptionConstructors are the only placement option constructors
// plugin.WorkloadOf accepts. The list is exhaustive rather than permissive on
// purpose: an unrecognized call in option position is a value this scanner
// cannot reason about, so it is rejected instead of waved through. Adding a
// third placement option therefore requires the deliberate edit here that its
// new side-effect surface deserves.
var archWorkloadOptionConstructors = []string{"WithExclusiveProcess", "WithReplicas"}

// archIsCompactBundleComposition reports whether call is a BundleOf or
// CombineBundles invocation whose every argument is side-effect-free
// composition data.
func archIsCompactBundleComposition(file *ast.File, call *ast.CallExpr, topValues map[string]archTopValue, seen map[string]bool) bool {
	for _, name := range archCompactCompositionConstructors {
		if !archCallMatches(file, call, archPluginImportPath, name) {
			continue
		}
		for _, argument := range call.Args {
			if !archIsSideEffectFreeBundleArgument(file, argument, topValues, seen) {
				return false
			}
		}
		return true
	}
	return false
}

// archIsWorkloadOfCall reports whether call is a well-formed
// plugin.WorkloadOf invocation: a statically resolvable workload key, one
// side-effect-free Bundle, and nothing but the declared placement option
// constructors after it.
//
// WorkloadOf cannot share the argument rule its two sibling constructors use,
// because its first argument is a key and its trailing ones are options rather
// than Definitions. Splitting the call open here rather than accepting it
// wholesale is what keeps a workload Bundle composable by the same proof as
// any other: every occurrence still has to trace back to a package-level
// Definition handle, and every key still has to be readable without computing
// anything.
func archIsWorkloadOfCall(file *ast.File, call *ast.CallExpr, topValues map[string]archTopValue, seen map[string]bool) bool {
	if !archCallMatches(file, call, archPluginImportPath, "WorkloadOf") {
		return false
	}
	if len(call.Args) < 2 {
		return false
	}
	if !archIsWorkloadKeyExpression(file, call.Args[0], topValues) {
		return false
	}
	if !archIsSideEffectFreeBundleArgument(file, call.Args[1], topValues, seen) {
		return false
	}
	for _, option := range call.Args[2:] {
		if !archIsWorkloadOptionExpression(file, option) {
			return false
		}
	}
	return true
}

// archIsWorkloadKeyExpression accepts the forms a workload key can be written
// in without computing it: a string literal, a package-level constant, or
// another repository package's exported identifier. A key the scanner cannot
// resolve is rejected rather than assumed harmless, because Bundle() has to be
// readable as the same composition on every evaluation.
func archIsWorkloadKeyExpression(file *ast.File, expression ast.Expr, topValues map[string]archTopValue) bool {
	switch expression := archUnwrapExpression(expression).(type) {
	case *ast.BasicLit:
		return expression.Kind == token.STRING
	case *ast.Ident:
		value, ok := topValues[expression.Name]
		return ok && value.kind == token.CONST && value.initializer != nil
	case *ast.SelectorExpr:
		identifier, ok := expression.X.(*ast.Ident)
		return ok && archImportsRepositoryPackage(file, identifier.Name)
	default:
		return false
	}
}

// archIsWorkloadOptionExpression reports whether expression is a call to one of
// the declared placement option constructors.
func archIsWorkloadOptionExpression(file *ast.File, expression ast.Expr) bool {
	call, ok := archUnwrapExpression(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	for _, name := range archWorkloadOptionConstructors {
		if archCallMatches(file, call, archPluginImportPath, name) {
			return true
		}
	}
	return false
}

// archCompositionConstructors are every plugin package constructor that builds
// composition data. A package-level var holding one of these is composition
// data in the same sense a direct call is, so the scanners follow it rather
// than rejecting it.
var archCompositionConstructors = []string{"BundleOf", "CombineBundles", "WorkloadOf"}

// archIsCompositionConstructorInitializer reports whether initializer builds
// composition data through one of those constructors.
func archIsCompositionConstructorInitializer(file *ast.File, initializer ast.Expr) bool {
	for _, name := range archCompositionConstructors {
		if archCallMatchesExpression(file, initializer, archPluginImportPath, name) {
			return true
		}
	}
	return false
}

func archCallMatchesExpression(file *ast.File, expression ast.Expr, importPath, name string) bool {
	call, ok := archUnwrapExpression(expression).(*ast.CallExpr)
	return ok && archCallMatches(file, call, importPath, name)
}

func archCallMatches(file *ast.File, call *ast.CallExpr, importPath, name string) bool {
	if file == nil {
		return false
	}
	function := archUnwrapExpression(call.Fun)
	switch function := function.(type) {
	case *ast.IndexExpr:
		call = &ast.CallExpr{Fun: function.X}
		return archCallMatches(file, call, importPath, name)
	case *ast.IndexListExpr:
		call = &ast.CallExpr{Fun: function.X}
		return archCallMatches(file, call, importPath, name)
	case *ast.SelectorExpr:
		identifier, ok := function.X.(*ast.Ident)
		return ok && function.Sel.Name == name && archImportAliases(file, importPath)[identifier.Name]
	case *ast.Ident:
		return function.Name == name && archDotImports(file, importPath)
	}
	return false
}

func archIsCanonicalAutoloadInit(file *ast.File, function *ast.FuncDecl, parentImport string) bool {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	expression, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	declare, ok := archUnwrapExpression(expression.X).(*ast.CallExpr)
	if !ok || !archCallMatches(file, declare, archPluginAutoloadPath, "Declare") || len(declare.Args) != 1 {
		return false
	}
	bundle, ok := archUnwrapExpression(declare.Args[0]).(*ast.CallExpr)
	return ok && len(bundle.Args) == 0 && archCallMatches(file, bundle, parentImport, "Bundle")
}

func archImportAliases(file *ast.File, importPath string) map[string]bool {
	aliases := make(map[string]bool)
	for _, specification := range file.Imports {
		actual, err := strconv.Unquote(specification.Path.Value)
		if err != nil || actual != importPath {
			continue
		}
		alias := filepath.Base(importPath)
		if specification.Name != nil {
			alias = specification.Name.Name
		}
		if alias != "." && alias != "_" {
			aliases[alias] = true
		}
	}
	return aliases
}

func archDotImports(file *ast.File, importPath string) bool {
	for _, specification := range file.Imports {
		actual, err := strconv.Unquote(specification.Path.Value)
		if err == nil && actual == importPath && specification.Name != nil && specification.Name.Name == "." {
			return true
		}
	}
	return false
}

func archImportsRepositoryPackage(file *ast.File, alias string) bool {
	for _, specification := range file.Imports {
		importPath, err := strconv.Unquote(specification.Path.Value)
		if err != nil || !strings.HasPrefix(importPath, archRepositoryImportPrefix) {
			continue
		}
		name := filepath.Base(importPath)
		if specification.Name != nil {
			name = specification.Name.Name
		}
		if name == alias {
			return true
		}
	}
	return false
}

func archUnwrapExpression(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

func mustRelativePath(t *testing.T, base, target string) string {
	t.Helper()
	relative, err := filepath.Rel(base, target)
	require.NoError(t, err)
	return relative
}
