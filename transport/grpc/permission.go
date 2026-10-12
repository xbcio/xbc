package grpc

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/xbcio/xbc/plugin"
)

// Perm declares the permission of every method of the service that has no
// method-level permission of its own. It is the code-side counterpart of a Web
// route's own .Perm(): a permission travels with the service that needs it,
// and an authorization interceptor consults the frozen value through
// MethodPermission.
func (m *MethodOptions) Perm(permission string) *MethodOptions {
	m.recordPerm("", permission, false, strings.TrimSpace(permission) == "")
	return m
}

// MethodPerm declares the permission of one method, by its short name ("Get",
// not "/pkg.Service/Get"). It overrides the service-level Perm.
func (m *MethodOptions) MethodPerm(method, permission string) *MethodOptions {
	// An empty name is recorded as such rather than as the service-level
	// declaration it would otherwise silently become.
	m.recordPerm(method, permission, method == "", strings.TrimSpace(permission) == "")
	return m
}

func (m *MethodOptions) recordPerm(method, permission string, emptyMethod, emptyPerm bool) {
	m.registrar.table.declarePerm(m.registrar, permDeclaration{
		contributor: m.registrar.contributor,
		service:     m.service,
		method:      method,
		perm:        permission,
		emptyMethod: emptyMethod,
		emptyPerm:   emptyPerm,
	})
}

// permDeclaration is one recorded Perm or MethodPerm call. method is empty for
// a service-level declaration.
type permDeclaration struct {
	contributor plugin.Identity
	service     string
	method      string
	perm        string
	emptyMethod bool
	emptyPerm   bool
}

// applyPermDeclarations resolves every recorded permission declaration against
// the frozen service table and attaches each method's effective permission.
// The rules are the authentication declarations' rules: only the registering
// plugin may declare, the service and method must exist, and a slot may be
// declared once. An empty permission is rejected rather than recorded as the
// absence it would otherwise be indistinguishable from -- a declaration that
// silently declared nothing would leave the method on the missing-permission
// fallback its author meant to cover.
func applyPermDeclarations(services map[string]*registeredService, declarations []permDeclaration) error {
	type slot struct{ service, method string }
	declared := make(map[slot]bool, len(declarations))
	for _, d := range declarations {
		where := fmt.Sprintf("gRPC service %q", d.service)
		if d.method != "" || d.emptyMethod {
			where = fmt.Sprintf("gRPC method /%s/%s", d.service, d.method)
		}
		if d.emptyMethod {
			return fmt.Errorf("xbc: plugin %s declared a permission for %s with an empty method name", d.contributor, where)
		}
		if d.emptyPerm {
			return fmt.Errorf("xbc: plugin %s declared an empty permission for %s", d.contributor, where)
		}
		service, ok := services[d.service]
		if !ok {
			return fmt.Errorf("xbc: plugin %s declared a permission for %s, which no plugin registers", d.contributor, where)
		}
		if service.contributor != d.contributor {
			return fmt.Errorf("xbc: plugin %s declared a permission for %s, which is registered by %s; only the registering plugin may declare it",
				d.contributor, where, service.contributor)
		}
		key := slot{d.service, d.method}
		if declared[key] {
			return fmt.Errorf("xbc: plugin %s declared a permission for %s more than once", d.contributor, where)
		}
		declared[key] = true

		if d.method == "" {
			service.servicePerm = d.perm
			continue
		}
		index := slices.IndexFunc(service.methods, func(m methodInfo) bool { return m.name == d.method })
		if index < 0 {
			return fmt.Errorf("xbc: plugin %s declared a permission for %s, which the service does not define", d.contributor, where)
		}
		service.methods[index].perm = d.perm
	}
	for _, service := range services {
		if service.servicePerm == "" {
			continue
		}
		for i := range service.methods {
			if service.methods[i].perm == "" {
				service.methods[i].perm = service.servicePerm
			}
		}
	}
	return nil
}

type methodPermissionContextKey struct{}

func withMethodPermission(ctx context.Context, permission string) context.Context {
	return context.WithValue(ctx, methodPermissionContextKey{}, permission)
}

// MethodPermission returns the registration-time permission in effect for the
// calling method -- its own MethodPerm, its service's Perm, or none --
// published by the authentication stage, which resolves every call against the
// same frozen method table. It is the gRPC counterpart of a Web route's
// RouteInfo.Perm.
//
// Authorization interceptors must ask this rather than reading a registration
// they captured themselves: the authentication stage answers for the exact
// method the call resolved to, and a call that never passed that stage reports
// declared=false -- fail closed, not open.
func MethodPermission(ctx context.Context) (permission string, declared bool) {
	if ctx == nil {
		return "", false
	}
	permission, ok := ctx.Value(methodPermissionContextKey{}).(string)
	if !ok || permission == "" {
		return "", false
	}
	return permission, true
}
