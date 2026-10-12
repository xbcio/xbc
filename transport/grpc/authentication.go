package grpc

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

const (
	// AuthenticationInterceptorKey is the canonical producer identity of the
	// Server's built-in authentication stage, assembled on both chains from the
	// grpc.security section. Contributed interceptors order against it with
	// After: Require(AuthenticationInterceptorKey); an interceptor implementing
	// authentication.RequiresPrincipal is pinned after it by the framework.
	AuthenticationInterceptorKey plugin.Key = "grpc-authentication"

	// challengeTrailer carries one authentication challenge per value on an
	// Unauthenticated status, the gRPC counterpart of WWW-Authenticate.
	challengeTrailer = "www-authenticate"
)

var authenticationIdentity = plugin.Identity{Plugin: AuthenticationInterceptorKey}

// methodDecision records how one frozen method resolved, for the startup
// report.
type methodDecision struct {
	fullMethod string
	policy     effectivePolicy
}

// authenticationStage is the single place a call's authentication policy is
// decided and a Principal is published.
//
// Like the panic boundary it is assembled rather than contributed: the default
// is deny, so a stage a composition root could leave out would silently serve
// every method unauthenticated, and the policy it enforces lives in the
// Server's own grpc.security section.
type authenticationStage struct {
	logger      log.Logger
	manager     *authentication.Manager
	extractors  map[authentication.Scheme]CredentialExtractor
	compiled    map[string]effectivePolicy
	methodPerms map[string]string
	decisions   []methodDecision
	schemes     []authentication.Scheme
	permitAll   bool
}

// newAuthenticationStage validates grpc.security against the registered
// authenticators and extractors and compiles a policy for every frozen method.
// Every failure is a startup failure: a policy mistake must never degrade into
// a silently permissive service.
//
// reflection extends the compiled table with the standard reflection methods,
// which are served but owned by no registrant. They carry the declaration tier,
// not a rule of their own, and appear in no method table: the startup report
// explains them on their own line.
func newAuthenticationStage(
	logger log.Logger,
	security SecurityConfig,
	authenticators []plugin.Entry[authentication.Authenticator],
	extractorEntries []plugin.Entry[CredentialExtractor],
	services []*registeredService,
	reflection bool,
) (*authenticationStage, error) {
	policies, err := newPolicySet(security)
	if err != nil {
		return nil, err
	}
	if err := policies.validateReachability(); err != nil {
		return nil, err
	}
	extractors, err := newExtractorIndex(extractorEntries)
	if err != nil {
		return nil, err
	}
	stage := &authenticationStage{
		logger:     logger,
		extractors: extractors,
		permitAll:  security.Default == SecurityPermit,
	}

	if len(authenticators) == 0 {
		// A service whose methods are all permitted is legitimate, so the
		// manager stays nil and compilation fails if a method needs one. A
		// declared scheme order has nothing to order here, so it is refused
		// rather than ignored.
		if len(security.Schemes) > 0 {
			return nil, validateDeclaredSchemes(security.Schemes, nil)
		}
		if err := policies.validateRuleSchemes(nil); err != nil {
			return nil, err
		}
	} else {
		manager, schemes, err := newAuthenticationManager(security.Schemes, authenticators, extractors)
		if err != nil {
			return nil, err
		}
		if err := policies.validateRuleSchemes(schemes); err != nil {
			return nil, err
		}
		if err := policies.validateSchemeCoverage(schemes); err != nil {
			return nil, err
		}
		stage.manager = manager
		stage.schemes = schemes
	}

	stage.compiled = make(map[string]effectivePolicy)
	stage.methodPerms = make(map[string]string)
	for _, service := range services {
		for _, method := range service.methods {
			policy := policies.resolve(service.desc.ServiceName, method)
			stage.compiled[method.fullMethod] = policy
			stage.methodPerms[method.fullMethod] = method.perm
			stage.decisions = append(stage.decisions, methodDecision{fullMethod: method.fullMethod, policy: policy})
			if policy.permit {
				continue
			}
			if stage.manager == nil {
				return nil, fmt.Errorf("xbc: gRPC method %s requires authentication but no authenticator is registered", method.fullMethod)
			}
			if err := stage.manager.ValidateSelection(policy.selection); err != nil {
				return nil, fmt.Errorf("xbc: gRPC method %s: %w", method.fullMethod, err)
			}
		}
	}
	if reflection {
		for fullMethod, policy := range reflectionPolicies(policies) {
			stage.compiled[fullMethod] = policy
		}
	}
	return stage, nil
}

// newAuthenticationManager orders the authenticators by the declared
// arbitration order. Order is never inferred from collection order, which is
// plugin-key order: renaming a plugin must not change which scheme wins.
func newAuthenticationManager(
	declared []authentication.Scheme,
	authenticators []plugin.Entry[authentication.Authenticator],
	extractors map[authentication.Scheme]CredentialExtractor,
) (*authentication.Manager, []authentication.Scheme, error) {
	byScheme := make(map[authentication.Scheme]authentication.Authenticator, len(authenticators))
	byIdentity := make(map[authentication.Scheme]plugin.Identity, len(authenticators))
	registered := make([]authentication.Scheme, 0, len(authenticators))
	for _, entry := range authenticators {
		scheme := entry.Value.Scheme()
		if previous, exists := byIdentity[scheme]; exists {
			return nil, nil, fmt.Errorf("xbc: scheme %q is registered by both %s and %s; only one authenticator may register a given scheme",
				scheme, previous, entry.Identity)
		}
		if _, ok := extractors[scheme]; !ok {
			// Without an extractor the scheme can never see a credential, and
			// every call selecting it would fail at request time instead.
			return nil, nil, fmt.Errorf(
				"xbc: authenticator %s registers scheme %q, but no gRPC credential extractor serves it; select the scheme's gRPC Bundle",
				entry.Identity, scheme)
		}
		byScheme[scheme] = entry.Value
		byIdentity[scheme] = entry.Identity
		registered = append(registered, scheme)
	}

	if len(declared) == 0 {
		if len(authenticators) >= 2 {
			return nil, nil, fmt.Errorf(
				"xbc: grpc.security.schemes must declare the arbitration order for %d registered authenticators (%s); authentication order is a security decision and is never inferred",
				len(authenticators), formatSchemeList(registered))
		}
		declared = registered
	} else if err := validateDeclaredSchemes(declared, registered); err != nil {
		return nil, nil, err
	}

	ordered := make([]authentication.Authenticator, len(declared))
	for i, scheme := range declared {
		ordered[i] = byScheme[scheme]
	}
	schemes := append([]authentication.Scheme(nil), declared...)
	// DefaultSchemes is every registered scheme, which is what lets
	// validateSchemeCoverage skip its check under default deny.
	manager, err := authentication.NewManager(authentication.ManagerOptions{
		Authenticators: ordered,
		DefaultSchemes: schemes,
	})
	if err != nil {
		return nil, nil, err
	}
	return manager, schemes, nil
}

func (*authenticationStage) Order() Order { return Order{Phase: PhaseAuth} }

func (a *authenticationStage) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		authenticated, challenges, err := a.authenticate(ctx, info.FullMethod)
		if err != nil {
			if len(challenges) > 0 {
				_ = grpc.SetTrailer(ctx, challenges)
			}
			return nil, err
		}
		return handler(authenticated, req)
	}
}

func (a *authenticationStage) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// A stream is authenticated once, when it is established; messages
		// that follow travel under that decision.
		authenticated, challenges, err := a.authenticate(ss.Context(), info.FullMethod)
		if err != nil {
			if len(challenges) > 0 {
				ss.SetTrailer(challenges)
			}
			return err
		}
		return handler(srv, &contextStream{ServerStream: ss, ctx: authenticated})
	}
}

// authenticate resolves one call. It returns the context the call continues
// with, or a status error and the challenge trailer to send with it.
func (a *authenticationStage) authenticate(ctx context.Context, fullMethod string) (context.Context, metadata.MD, error) {
	policy, known := a.compiled[fullMethod]
	if !known {
		// Every served method was compiled from the same frozen table, so a
		// miss is an internal inconsistency, and it fails closed.
		return nil, nil, a.internalFailure(fullMethod, errors.New("xbc: gRPC method has no compiled authentication policy"))
	}
	// The method's registration-time permission travels with the call, the way
	// AuthenticationExempt does: a downstream authorization interceptor asks
	// this stage what the method declared rather than reading the frozen table
	// through a second seam.
	ctx = withMethodPermission(ctx, a.methodPerms[fullMethod])
	if policy.permit {
		return withAuthenticationExempt(ctx), nil, nil
	}

	md, _ := metadata.FromIncomingContext(ctx)
	result, err := a.manager.Authenticate(ctx, policy.selection, metadataCredentialSource{md: md, extractors: a.extractors})
	if err != nil {
		return nil, nil, a.internalFailure(fullMethod, err)
	}
	if result.Authenticated() {
		if result.Status() == authentication.ResultStatusAuthenticatedWithoutPrincipal {
			// A verified call with no user principal. Being authenticated and
			// being exempt are different facts, so neither is published.
			return ctx, nil, nil
		}
		principal, _ := result.Principal()
		typed, ok := principal.(authentication.Principal)
		if !ok {
			return nil, nil, a.internalFailure(fullMethod, fmt.Errorf("xbc: authenticator returned %T, want authentication.Principal", principal))
		}
		published, ok := withPrincipal(ctx, typed)
		if !ok {
			return nil, nil, a.internalFailure(fullMethod, errors.New("xbc: authenticator returned a principal with an empty subject"))
		}
		return published, nil, nil
	}

	var trailer metadata.MD
	for _, challenge := range result.Challenges() {
		if trailer == nil {
			trailer = metadata.MD{}
		}
		trailer.Append(challengeTrailer, string(challenge))
	}
	message := "unauthenticated"
	if reason, ok := result.Reason(); ok {
		message = string(reason)
	}
	return nil, trailer, status.Error(codes.Unauthenticated, message)
}

// internalFailure logs an operational authentication failure and returns the
// fixed status the client receives. Only err.Error() is logged: an
// authentication.OperationalError renders its operation and scheme and elides
// its cause precisely because that cause may quote the credential. The status
// is identical for every cause so it cannot be used as an oracle.
func (a *authenticationStage) internalFailure(fullMethod string, err error) error {
	a.logger.Error("grpc authentication failed", "method", fullMethod, "error", err.Error())
	return status.Error(codes.Internal, "authentication failed")
}

// contextStream carries a context the authentication stage derived, so stream
// handlers and later interceptors see the published Principal through
// ServerStream.Context().
type contextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *contextStream) Context() context.Context { return s.ctx }
