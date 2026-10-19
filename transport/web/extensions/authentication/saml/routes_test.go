package saml

import (
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	federationcore "github.com/xbcio/xbc/extensions/authentication/federation"
	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

// routeRegistration is one route declaration found in RegisterRoutes.
type routeRegistration struct {
	method string
	path   string
	name   string
	public bool
}

// TestRegisterRoutesMountsTheLoginRoutesAsPublic is a structural guard over
// RegisterRoutes. A *web.Router can only be constructed inside transport/web,
// so an adapter test beside this package cannot freeze a real route table and
// ask it whether these routes are public. Instead the guard reads the one
// declaration that makes that decision -- the registration chain itself -- and
// fails if a route stops being public, changes its method or path, or loses its
// operation name. The paired handler behavior tests then cover everything the
// chain dispatches to.
func TestRegisterRoutesMountsTheLoginRoutesAsPublic(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "routes.go", nil, 0)
	if err != nil {
		t.Fatalf("parse routes.go: %v", err)
	}

	declaration := findMethod(t, file, "RegisterRoutes")
	if !groupedByConfiguredPrefix(declaration) {
		t.Fatal("RegisterRoutes does not mount its routes on router.Group(p.config.prefix)")
	}

	registrations := routeRegistrations(t, declaration)
	if len(registrations) != 3 {
		t.Fatalf("RegisterRoutes declares %d routes, want exactly 3: %#v", len(registrations), registrations)
	}
	want := map[string]routeRegistration{
		"/:provider/start":    {method: "GET", path: "/:provider/start", name: "saml.start", public: true},
		"/:provider/acs":      {method: "POST", path: "/:provider/acs", name: "saml.acs", public: true},
		"/:provider/metadata": {method: "GET", path: "/:provider/metadata", name: "saml.metadata", public: true},
	}
	for path, expected := range want {
		got, ok := registrations[path]
		if !ok {
			t.Fatalf("RegisterRoutes does not declare %q: %#v", path, registrations)
		}
		if got != expected {
			t.Fatalf("registration for %q = %#v, want %#v", path, got, expected)
		}
	}
}

// findMethod returns the declaration of the named method in file.
func findMethod(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name && function.Recv != nil {
			return function
		}
	}
	t.Fatalf("routes.go declares no method %s", name)
	return nil
}

// groupedByConfiguredPrefix reports whether the body derives its route group
// from the configured prefix rather than hard-coding a path.
func groupedByConfiguredPrefix(function *ast.FuncDecl) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Group" || len(call.Args) != 1 {
			return true
		}
		argument, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		settings, ok := argument.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := settings.X.(*ast.Ident)
		if ok && owner.Name == "p" && settings.Sel.Name == "config" && argument.Sel.Name == "prefix" {
			found = true
		}
		return true
	})
	return found
}

// routeRegistrations flattens every fluent registration chain in body and
// records the method, path, operation name, and whether the chain declares the
// explicit public policy.
func routeRegistrations(t *testing.T, function *ast.FuncDecl) map[string]routeRegistration {
	t.Helper()
	registrations := make(map[string]routeRegistration)
	for _, statement := range function.Body.List {
		expression, ok := statement.(*ast.ExprStmt)
		if !ok {
			continue
		}
		chain := flattenChain(expression.X)
		if len(chain) == 0 {
			continue
		}

		record := routeRegistration{}
		registered := false
		for _, call := range chain {
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			switch selector.Sel.Name {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
				path, ok := stringLiteral(call)
				if !ok {
					t.Fatalf("route registration uses a non-literal path")
				}
				record.method = selector.Sel.Name
				record.path = path
				registered = true
			case "Name":
				name, ok := stringLiteral(call)
				if !ok {
					t.Fatalf("route name is not a literal")
				}
				record.name = name
			case "Auth":
				record.public = isPublicPolicy(call)
			}
		}
		if registered {
			registrations[record.path] = record
		}
	}
	return registrations
}

// flattenChain returns every call in a fluent chain, outermost first.
func flattenChain(expression ast.Expr) []*ast.CallExpr {
	var calls []*ast.CallExpr
	var walk func(ast.Expr)
	walk = func(current ast.Expr) {
		call, ok := current.(*ast.CallExpr)
		if !ok {
			return
		}
		calls = append(calls, call)
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			walk(selector.X)
		}
	}
	walk(expression)
	return calls
}

// stringLiteral returns a call's first argument when it is a string literal.
func stringLiteral(call *ast.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// isPublicPolicy reports whether the Auth argument is a call to Public() on the
// transport/web import.
func isPublicPolicy(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	policy, ok := call.Args[0].(*ast.CallExpr)
	if !ok || len(policy.Args) != 0 {
		return false
	}
	selector, ok := policy.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Public" {
		return false
	}
	owner, ok := selector.X.(*ast.Ident)
	return ok && owner.Name == "transportweb"
}

// TestProviderNameRuleMatchesTheRegistryContract pins the provider-name rule
// the adapter duplicates from federation.Registry: the first character is
// alphanumeric, later characters may also be "_" or "-", and the bound is
// inclusive at MaxProviderNameLength. The routes answer a malformed name and an
// unregistered one identically, so only a direct test can freeze the rule
// itself.
func TestProviderNameRuleMatchesTheRegistryContract(t *testing.T) {
	bounded := strings.Repeat("a", federationcore.MaxProviderNameLength)
	for name, testCase := range map[string]struct {
		value string
		valid bool
	}{
		"single letter":          {"a", true},
		"single digit":           {"0", true},
		"mixed":                  {"corp-idp_2", true},
		"at the length bound":    {bounded, true},
		"above the length bound": {bounded + "a", false},
		"leading dash":           {"-corp", false},
		"leading underscore":     {"_corp", false},
		"whitespace":             {"bad name", false},
		"dot segments":           {"..", false},
		"non-ASCII":              {"café", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validProviderName(testCase.value); got != testCase.valid {
				t.Fatalf("validProviderName(%q) = %v, want %v", testCase.value, got, testCase.valid)
			}
		})
	}
}

// TestStateCarriesAtLeast128Bits pins the RelayState generation: 16 random
// bytes rendered as lowercase hexadecimal, which is 32 unreserved characters
// and needs no escaping in the redirect query.
func TestStateCarriesAtLeast128Bits(t *testing.T) {
	first, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	second, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	if len(first) != 2*stateBytes {
		t.Fatalf("state length = %d, want %d", len(first), 2*stateBytes)
	}
	// The lower bound is stated independently of stateBytes so shrinking the
	// constant below 128 bits cannot keep the test green.
	if len(first) != 32 {
		t.Fatalf("state length = %d, want 32 hexadecimal characters (128 bits)", len(first))
	}
	for _, character := range first {
		if !strings.ContainsRune("0123456789abcdef", character) {
			t.Fatalf("state %q carries a non-hexadecimal character %q", first, character)
		}
	}
	if first == second {
		t.Fatal("two states are identical")
	}
}

func TestResponseErrorCodeMapsToTheClosedVocabulary(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want federationweb.ErrorCode
	}{
		"replay":                {samlcore.ErrReplay, federationweb.CodeBadRequest},
		"invalid response":      {samlcore.ErrInvalidResponse, federationweb.CodeBadRequest},
		"wrapped invalid":       {errors.Join(errors.New("stage failed"), samlcore.ErrInvalidResponse), federationweb.CodeBadRequest},
		"unavailable":           {samlcore.ErrUnavailable, federationweb.CodeFailed},
		"wrapped unavailable":   {errors.Join(errors.New("cache failed"), samlcore.ErrUnavailable), federationweb.CodeFailed},
		"nil context":           {samlcore.ErrNilContext, federationweb.CodeFailed},
		"config":                {samlcore.ErrInvalidConfig, federationweb.CodeFailed},
		"arbitrary operational": {errors.New("boom"), federationweb.CodeFailed},
	} {
		t.Run(name, func(t *testing.T) {
			if got := responseErrorCode(testCase.err); got != testCase.want {
				t.Fatalf("responseErrorCode(%v) = %q, want %q", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestDecodeSAMLResponseBoundsTheDecodedDocument(t *testing.T) {
	valid := []byte("<samlp:Response/>")
	decoded, err := decodeSAMLResponse(base64.StdEncoding.EncodeToString(valid))
	if err != nil {
		t.Fatalf("decodeSAMLResponse: %v", err)
	}
	if string(decoded) != string(valid) {
		t.Fatalf("decoded = %q, want %q", decoded, valid)
	}

	for name, value := range map[string]string{
		"empty":          "",
		"not base64":     "not base64!",
		"binary garbage": "\x00\x01",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSAMLResponse(value); err == nil {
				t.Fatalf("decodeSAMLResponse(%q) = nil error", value)
			}
		})
	}

	oversized := base64.StdEncoding.EncodeToString(make([]byte, maxDecodedResponseBytes+1))
	if _, err := decodeSAMLResponse(oversized); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("oversized decode error = %v, want errResponseTooLarge", err)
	}
}
