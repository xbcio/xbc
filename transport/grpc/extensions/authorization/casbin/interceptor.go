package casbin

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Order implements grpctransport.UnaryInterceptor and
// grpctransport.StreamInterceptor. The phase is the whole of the ordering it
// declares: implementing authentication.RequiresPrincipal is what the
// framework turns into a required predecessor edge on the authentication
// stage, in both chains, so authorization never runs before the identity it
// consumes was published.
func (*Plugin) Order() grpctransport.Order {
	return grpctransport.Order{Phase: grpctransport.PhaseAuth}
}

// UnaryInterceptor implements grpctransport.UnaryInterceptor.
func (p *Plugin) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if grpctransport.AuthenticationExempt(ctx) {
			return handler(ctx, req)
		}
		if err := p.authorize(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor implements grpctransport.StreamInterceptor. A stream is
// authorized once, when it is established; messages that follow travel under
// that decision.
func (p *Plugin) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if grpctransport.AuthenticationExempt(ss.Context()) {
			return handler(srv, ss)
		}
		if err := p.authorize(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// authorize resolves one call. It asks the framework what exemption was
// resolved (the caller's job, done before this runs) and what permission the
// method declared, never a registration captured by this interceptor itself:
// the authentication stage answered for the exact method the call resolved to.
func (p *Plugin) authorize(ctx context.Context, fullMethod string) error {
	// A value without a runtime state denies rather than skips: authorization
	// fails closed on every path that is not a proven allow.
	if p == nil || p.state == nil {
		return forbidden()
	}
	if p.stopped.Load() {
		return forbidden()
	}
	state := p.state

	enforcer, active := state.provider.Enforcer()
	if !active || enforcer == nil {
		// The engine reports itself unavailable after its own Stop, so an
		// interceptor that outlives the engine fails closed here instead of
		// enforcing against a torn-down policy source.
		return forbidden()
	}

	principal, ok := grpctransport.PrincipalFromContext(ctx)
	subject := strings.TrimSpace(principal.Subject)
	if !ok || subject == "" {
		return forbidden()
	}

	var request []any
	switch state.convention {
	case casbincore.ConventionRoutePermission:
		permission, declared := grpctransport.MethodPermission(ctx)
		if !declared {
			if state.cfg.missingPermission == MissingPermissionAllow {
				return nil
			}
			return forbidden()
		}
		request = []any{subject, strings.TrimSpace(permission)}
	case casbincore.ConventionPathMethod:
		object := strings.TrimSpace(fullMethod)
		if object == "" {
			return forbidden()
		}
		request = []any{subject, object, "RPC"}
	}

	allowed, err := enforcer.Enforce(request...)
	if err != nil {
		// The error is logged, never returned or rendered into the status: it
		// may quote policy structure, and the caller must not be able to read
		// the policy back out of a rejection.
		state.logger.Error("casbin-grpc: authorization evaluation failed", "method", fullMethod, "error", err)
		return forbidden()
	}
	if !allowed {
		return forbidden()
	}
	return nil
}

// forbidden is the one rejection this interceptor produces. It is identical
// for every cause -- no principal, no permission, no engine, a policy miss --
// so a caller cannot use the message to probe policy structure.
func forbidden() error {
	return status.Error(codes.PermissionDenied, "forbidden")
}
