package grpc

import (
	"fmt"
	"slices"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

// AuthPolicy is a sealed, registration-time authentication declaration for a
// service or one of its methods. Its fields are private: a policy can only be
// constructed through Public or Accepts, so the two mutually exclusive states
// cannot be combined, and the zero value is rejected when the method table is
// frozen rather than read as either of them.
//
// A method with no declaration is a third, distinct state: it falls through to
// grpc.security.default, which is deny unless the application chose otherwise.
// No declaration never means public.
type AuthPolicy struct {
	kind    authPolicyKind
	schemes []authentication.Scheme
}

type authPolicyKind uint8

const (
	authPolicyInvalid authPolicyKind = iota
	authPolicyPublic
	authPolicyExplicit
)

// Public declares that a method needs no authentication.
func Public() AuthPolicy { return AuthPolicy{kind: authPolicyPublic} }

// Accepts restricts a method to the supplied authentication schemes, evaluated
// in grpc.security.schemes order. The input is copied immediately. An empty or
// duplicated list is rejected when the method table is frozen, where the error
// can name the method.
func Accepts(schemes ...authentication.Scheme) AuthPolicy {
	return AuthPolicy{kind: authPolicyExplicit, schemes: slices.Clone(schemes)}
}

// IsPublic reports whether this is the Public policy.
func (p AuthPolicy) IsPublic() bool { return p.kind == authPolicyPublic }

// Schemes returns a copy of an Accepts policy's schemes, and nil otherwise.
func (p AuthPolicy) Schemes() []authentication.Scheme {
	if p.kind != authPolicyExplicit {
		return nil
	}
	return slices.Clone(p.schemes)
}

func (p AuthPolicy) String() string {
	switch p.kind {
	case authPolicyPublic:
		return "public"
	case authPolicyExplicit:
		return "accepts " + formatSchemeList(p.schemes)
	default:
		return "invalid"
	}
}

func (p AuthPolicy) validate() error {
	switch p.kind {
	case authPolicyPublic:
		return nil
	case authPolicyExplicit:
		if len(p.schemes) == 0 {
			return fmt.Errorf("Accepts names no scheme")
		}
		seen := make(map[authentication.Scheme]struct{}, len(p.schemes))
		for _, scheme := range p.schemes {
			if err := scheme.Validate(); err != nil {
				return err
			}
			if _, duplicate := seen[scheme]; duplicate {
				return fmt.Errorf("Accepts names scheme %q more than once", scheme)
			}
			seen[scheme] = struct{}{}
		}
		return nil
	default:
		return fmt.Errorf("the policy was not built with Public or Accepts")
	}
}

// MethodOptions declares registration-time properties of one service's
// methods. It is the code-side counterpart of a Web route's own declarations:
// a declaration travels with the service that needs it, and an application rule
// in grpc.security still outranks it.
//
// Declarations are recorded, not applied: they are checked against the frozen
// method table once the contributor's RegisterServices returns, so they may be
// written before or after the generated Register call. Like the Registrar it
// came from, a MethodOptions is valid only during that call.
type MethodOptions struct {
	registrar *Registrar
	service   string
}

// Methods returns the declaration handle for service, which must be registered
// by the same contributor in the same RegisterServices call.
func (r *Registrar) Methods(service string) *MethodOptions {
	if r == nil || r.table == nil {
		panic("xbc: gRPC Registrar used outside the Server that created it")
	}
	return &MethodOptions{registrar: r, service: service}
}

// Auth declares the authentication policy of every method of the service that
// has no method-level declaration of its own.
func (m *MethodOptions) Auth(policy AuthPolicy) *MethodOptions {
	m.record("", policy, false)
	return m
}

// MethodAuth declares the authentication policy of one method, by its short
// name ("Get", not "/pkg.Service/Get"). It overrides the service-level Auth.
func (m *MethodOptions) MethodAuth(method string, policy AuthPolicy) *MethodOptions {
	// An empty name is recorded as such rather than as the service-level
	// declaration it would otherwise silently become.
	m.record(method, policy, method == "")
	return m
}

func (m *MethodOptions) record(method string, policy AuthPolicy, emptyMethod bool) {
	policy.schemes = slices.Clone(policy.schemes)
	m.registrar.table.declare(m.registrar, authDeclaration{
		contributor: m.registrar.contributor,
		service:     m.service,
		method:      method,
		policy:      policy,
		emptyMethod: emptyMethod,
	})
}

// authDeclaration is one recorded Auth or MethodAuth call. method is empty for
// a service-level declaration.
type authDeclaration struct {
	contributor plugin.Identity
	service     string
	method      string
	policy      AuthPolicy
	emptyMethod bool
}

// applyDeclarations resolves every recorded declaration against the frozen
// service table, attaching each method's effective declaration. Every mistake
// is an error naming the contributor: a declaration that silently matched
// nothing would leave a method on the default policy its author meant to
// override.
func applyDeclarations(services map[string]*registeredService, declarations []authDeclaration) error {
	type slot struct{ service, method string }
	declared := make(map[slot]bool, len(declarations))
	for _, d := range declarations {
		where := fmt.Sprintf("gRPC service %q", d.service)
		if d.method != "" || d.emptyMethod {
			where = fmt.Sprintf("gRPC method /%s/%s", d.service, d.method)
		}
		if d.emptyMethod {
			return fmt.Errorf("xbc: plugin %s declared authentication for %s with an empty method name", d.contributor, where)
		}
		service, ok := services[d.service]
		if !ok {
			return fmt.Errorf("xbc: plugin %s declared authentication for %s, which no plugin registers", d.contributor, where)
		}
		if service.contributor != d.contributor {
			return fmt.Errorf("xbc: plugin %s declared authentication for %s, which is registered by %s; only the registering plugin may declare it",
				d.contributor, where, service.contributor)
		}
		if err := d.policy.validate(); err != nil {
			return fmt.Errorf("xbc: plugin %s declared an invalid authentication policy for %s: %w", d.contributor, where, err)
		}
		key := slot{d.service, d.method}
		if declared[key] {
			return fmt.Errorf("xbc: plugin %s declared authentication for %s more than once", d.contributor, where)
		}
		declared[key] = true

		policy := d.policy
		if d.method == "" {
			service.serviceAuth = &policy
			continue
		}
		index := slices.IndexFunc(service.methods, func(m methodInfo) bool { return m.name == d.method })
		if index < 0 {
			return fmt.Errorf("xbc: plugin %s declared authentication for %s, which the service does not define", d.contributor, where)
		}
		service.methods[index].auth = &policy
	}
	for _, service := range services {
		if service.serviceAuth == nil {
			continue
		}
		for i := range service.methods {
			if service.methods[i].auth == nil {
				inherited := *service.serviceAuth
				service.methods[i].auth = &inherited
			}
		}
	}
	return nil
}
