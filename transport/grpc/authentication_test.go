package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

const (
	schemeToken authentication.Scheme = "token"
	schemeKey   authentication.Scheme = "key"

	tokenChallenge authentication.Challenge = `Token realm="test"`
)

// prefixExtractor reads one metadata key and claims values carrying prefix.
type prefixExtractor struct {
	scheme    authentication.Scheme
	key       string
	prefix    string
	challenge authentication.Challenge
}

func (e prefixExtractor) Scheme() authentication.Scheme { return e.scheme }

func (e prefixExtractor) ExtractCredential(_ context.Context, md metadata.MD) (authentication.CredentialResult, error) {
	for _, value := range md.Get(e.key) {
		if !strings.HasPrefix(value, e.prefix) {
			continue
		}
		credential := strings.TrimSpace(strings.TrimPrefix(value, e.prefix))
		if credential == "" {
			return authentication.Malformed("empty credential"), nil
		}
		return authentication.Presented(credential), nil
	}
	if e.challenge != "" {
		return authentication.AbsentWithChallenge(e.challenge), nil
	}
	return authentication.Absent(), nil
}

// fakeAuthenticator accepts "good-<subject>", "service" without a principal,
// fails operationally on "boom", and rejects everything else.
type fakeAuthenticator struct{ scheme authentication.Scheme }

func (a fakeAuthenticator) Scheme() authentication.Scheme { return a.scheme }

func (a fakeAuthenticator) Authenticate(_ context.Context, credential authentication.Credential) (authentication.Result, error) {
	value, _ := authentication.CredentialAs[string](credential)
	switch {
	case strings.HasPrefix(value, "good-"):
		return authentication.Accepted(authentication.Principal{
			Subject:    strings.TrimPrefix(value, "good-"),
			AuthMethod: string(a.scheme),
		}), nil
	case value == "service":
		return authentication.AcceptedWithoutPrincipal(), nil
	case value == "boom":
		return authentication.Result{}, errors.New("backend at 10.0.0.7 refused secret-credential-value")
	default:
		return authentication.Rejected("invalid credential"), nil
	}
}

func tokenExtractor() prefixExtractor {
	return prefixExtractor{scheme: schemeToken, key: "authorization", prefix: "Token ", challenge: tokenChallenge}
}

func keyExtractor() prefixExtractor {
	return prefixExtractor{scheme: schemeKey, key: "x-api-key", prefix: ""}
}

func authenticatorEntry(key plugin.Key, scheme authentication.Scheme) plugin.Entry[authentication.Authenticator] {
	return plugin.Entry[authentication.Authenticator]{Identity: plugin.Identity{Plugin: key}, Value: fakeAuthenticator{scheme: scheme}}
}

func extractorEntry(key plugin.Key, e prefixExtractor) plugin.Entry[CredentialExtractor] {
	return plugin.Entry[CredentialExtractor]{Identity: plugin.Identity{Plugin: key}, Value: e}
}

// tokenOnly is the single-scheme composition most tests share.
func tokenOnly(registrars ...plugin.Entry[ServiceRegistrar]) contributions {
	return contributions{
		registrars:     registrars,
		authenticators: []plugin.Entry[authentication.Authenticator]{authenticatorEntry("token-auth", schemeToken)},
		extractors:     []plugin.Entry[CredentialExtractor]{extractorEntry("token-grpc", tokenExtractor())},
	}
}

// whoamiEcho answers with what the authentication stage published.
type whoamiEcho struct{}

func describeCall(ctx context.Context) string {
	if principal, ok := PrincipalFromContext(ctx); ok {
		return "principal:" + principal.Subject + "/" + principal.AuthMethod
	}
	if AuthenticationExempt(ctx) {
		return "exempt"
	}
	return "anonymous"
}

func (whoamiEcho) Echo(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return wrapperspb.String(describeCall(ctx)), nil
}

func (whoamiEcho) Repeat(_ *wrapperspb.StringValue, stream grpc.ServerStream) error {
	return stream.SendMsg(wrapperspb.String(describeCall(stream.Context())))
}

func whoamiContributor(declare func(*MethodOptions)) plugin.Entry[ServiceRegistrar] {
	return plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "whoami"},
		Value: registrarFunc(func(r *Registrar) error {
			r.RegisterService(echoDesc(), whoamiEcho{})
			if declare != nil {
				declare(r.Methods(echoServiceName))
			}
			return nil
		}),
	}
}

func outgoing(pairs ...string) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = cancel // released by the timeout; the test process outlives it
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

func invokeEcho(conn *grpc.ClientConn, ctx context.Context) (string, metadata.MD, error) {
	var trailer metadata.MD
	out := new(wrapperspb.StringValue)
	err := conn.Invoke(ctx, "/"+echoServiceName+"/Echo", wrapperspb.String("x"), out, grpc.Trailer(&trailer))
	return out.GetValue(), trailer, err
}

func invokeRepeat(t *testing.T, conn *grpc.ClientConn, ctx context.Context) (string, metadata.MD, error) {
	t.Helper()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true}, "/"+echoServiceName+"/Repeat")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	out := new(wrapperspb.StringValue)
	err = stream.RecvMsg(out)
	return out.GetValue(), stream.Trailer(), err
}

func TestDefaultDenyRejectsWithAChallengeTrailerOnBothChains(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(whoamiContributor(nil)))
	_, conn := startServing(t, s)

	_, trailer, err := invokeEcho(conn, outgoing())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unary error = %v, want Unauthenticated", err)
	}
	if got := trailer.Get(challengeTrailer); len(got) != 1 || got[0] != string(tokenChallenge) {
		t.Fatalf("unary trailer %s = %v, want [%s]", challengeTrailer, got, tokenChallenge)
	}

	_, trailer, err = invokeRepeat(t, conn, outgoing())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("stream error = %v, want Unauthenticated", err)
	}
	if got := trailer.Get(challengeTrailer); len(got) != 1 || got[0] != string(tokenChallenge) {
		t.Fatalf("stream trailer %s = %v, want [%s]", challengeTrailer, got, tokenChallenge)
	}
}

func TestAuthenticatedCallsSeeThePrincipalOnBothChains(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(whoamiContributor(nil)))
	_, conn := startServing(t, s)

	got, _, err := invokeEcho(conn, outgoing("authorization", "Token good-alice"))
	if err != nil || got != "principal:alice/token" {
		t.Fatalf("unary = %q, %v; want principal:alice/token", got, err)
	}
	got, _, err = invokeRepeat(t, conn, outgoing("authorization", "Token good-bob"))
	if err != nil || got != "principal:bob/token" {
		t.Fatalf("stream = %q, %v; want principal:bob/token", got, err)
	}
	// A verified call with no user principal is neither a principal nor exempt.
	got, _, err = invokeEcho(conn, outgoing("authorization", "Token service"))
	if err != nil || got != "anonymous" {
		t.Fatalf("service call = %q, %v; want anonymous", got, err)
	}
}

func TestRejectionsCarryOnlyTheSafeReason(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(whoamiContributor(nil)))
	_, conn := startServing(t, s)

	_, _, err := invokeEcho(conn, outgoing("authorization", "Token wrong"))
	if status.Code(err) != codes.Unauthenticated || status.Convert(err).Message() != "invalid credential" {
		t.Fatalf("rejected = %v, want Unauthenticated with the safe reason", err)
	}
	_, _, err = invokeEcho(conn, outgoing("authorization", "Token "))
	if status.Code(err) != codes.Unauthenticated || status.Convert(err).Message() != "empty credential" {
		t.Fatalf("malformed = %v, want Unauthenticated with the malformed reason", err)
	}
}

func TestOperationalFailureIsGenericAndNeverLeaksTheCause(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(whoamiContributor(nil)))
	_, conn := startServing(t, s)
	_, trailer, err := invokeEcho(conn, outgoing("authorization", "Token boom"))
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "authentication failed" {
		t.Fatalf("operational failure = %v, want Internal \"authentication failed\"", err)
	}
	for _, leaked := range []string{"10.0.0.7", "secret-credential-value", "boom"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("status leaked %q: %v", leaked, err)
		}
	}
	if len(trailer.Get(challengeTrailer)) != 0 {
		t.Fatal("an operational failure must not send a challenge")
	}
}

func TestPermittedCallsAreExemptAndPublishNoPrincipal(t *testing.T) {
	cases := map[string]struct {
		cfg     func(*Config)
		declare func(*MethodOptions)
	}{
		"declared public": {declare: func(m *MethodOptions) { m.Auth(Public()) }},
		"rule permit": {cfg: func(c *Config) {
			c.Security.Rules = []SecurityRule{{Match: "/" + echoServiceName + "/*", Permit: true}}
		}},
		"default permit": {cfg: func(c *Config) {
			c.Security.Default = SecurityPermit
			c.Security.Rules = []SecurityRule{{Match: "/other.Service/*", Authenticate: []authentication.Scheme{schemeToken}}}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			s := newServer(cfg, tokenOnly(whoamiContributor(tc.declare)))
			_, conn := startServing(t, s)
			// A credential on a permitted call is ignored, not verified.
			got, _, err := invokeEcho(conn, outgoing("authorization", "Token good-alice"))
			if err != nil || got != "exempt" {
				t.Fatalf("unary = %q, %v; want exempt", got, err)
			}
			got, _, err = invokeRepeat(t, conn, outgoing())
			if err != nil || got != "exempt" {
				t.Fatalf("stream = %q, %v; want exempt", got, err)
			}
		})
	}
}

func TestApplicationRuleOutranksDeclarationAndMethodOutranksService(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Security.Rules = []SecurityRule{{Match: "/" + echoServiceName + "/Repeat", Authenticate: []authentication.Scheme{schemeToken}}}
	declare := func(m *MethodOptions) {
		m.Auth(Public()).MethodAuth("Echo", Accepts(schemeToken))
	}
	s := newServer(cfg, tokenOnly(whoamiContributor(declare)))
	_, conn := startServing(t, s)

	// Echo: the method-level declaration overrides the service-level Public.
	if _, _, err := invokeEcho(conn, outgoing()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Echo error = %v, want Unauthenticated", err)
	}
	// Repeat: the rule overrides the inherited Public declaration.
	if _, _, err := invokeRepeat(t, conn, outgoing()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Repeat error = %v, want Unauthenticated", err)
	}

	report := renderAuthentication(s.assembly.auth)
	for _, want := range []string{
		"grpc: authentication order: token",
		"/xbc.test.Echo/Echo    authenticate token  [declaration]",
		"/xbc.test.Echo/Repeat  authenticate token  [application-rule #0]",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("authentication report missing %q:\n%s", want, report)
		}
	}
}

func TestSchemeArbitrationFollowsTheDeclaredOrder(t *testing.T) {
	two := func(order ...authentication.Scheme) (*Server, contributions) {
		cfg := DefaultConfig()
		cfg.Security.Schemes = order
		c := contributions{
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(nil)},
			authenticators: []plugin.Entry[authentication.Authenticator]{
				authenticatorEntry("key-auth", schemeKey),
				authenticatorEntry("token-auth", schemeToken),
			},
			extractors: []plugin.Entry[CredentialExtractor]{
				extractorEntry("key-grpc", keyExtractor()),
				extractorEntry("token-grpc", tokenExtractor()),
			},
		}
		return newServer(cfg, c), c
	}
	// Both credentials verify; the first declared scheme wins.
	for _, tc := range []struct {
		order []authentication.Scheme
		want  string
	}{
		{[]authentication.Scheme{schemeToken, schemeKey}, "principal:alice/token"},
		{[]authentication.Scheme{schemeKey, schemeToken}, "principal:bob/key"},
	} {
		s, _ := two(tc.order...)
		_, conn := startServing(t, s)
		got, _, err := invokeEcho(conn, outgoing("authorization", "Token good-alice", "x-api-key", "good-bob"))
		if err != nil || got != tc.want {
			t.Fatalf("order %v: Echo = %q, %v; want %q", tc.order, got, err, tc.want)
		}
	}
}

func TestSecurityMisconfigurationsFailAssembly(t *testing.T) {
	both := func(c *contributions) {
		c.authenticators = append(c.authenticators, authenticatorEntry("key-auth", schemeKey))
		c.extractors = append(c.extractors, extractorEntry("key-grpc", keyExtractor()))
	}
	cases := map[string]struct {
		cfg     func(*Config)
		contrib func(*contributions)
		want    string
	}{
		"unknown scheme": {
			cfg:  func(c *Config) { c.Security.Schemes = []authentication.Scheme{schemeToken, "ghost"} },
			want: `declares unknown scheme "ghost"`,
		},
		"duplicate declared scheme": {
			cfg:  func(c *Config) { c.Security.Schemes = []authentication.Scheme{schemeToken, schemeToken} },
			want: `duplicate scheme "token"`,
		},
		"missing declared scheme": {
			cfg:     func(c *Config) { c.Security.Schemes = []authentication.Scheme{schemeToken} },
			contrib: both,
			want:    `missing registered scheme "key"`,
		},
		"undeclared order for two": {
			contrib: both,
			want:    "must declare the arbitration order for 2 registered authenticators",
		},
		"schemes without authenticators": {
			cfg: func(c *Config) { c.Security.Schemes = []authentication.Scheme{schemeToken} },
			contrib: func(c *contributions) {
				c.authenticators, c.extractors = nil, nil
				c.registrars = []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(Public()) })}
			},
			want: `declares unknown scheme "token"`,
		},
		"authenticator without extractor": {
			contrib: func(c *contributions) { c.extractors = nil },
			want:    "no gRPC credential extractor serves it",
		},
		"duplicate authenticator scheme": {
			contrib: func(c *contributions) {
				c.authenticators = append(c.authenticators, authenticatorEntry("token-auth-2", schemeToken))
			},
			want: `scheme "token" is registered by both`,
		},
		"duplicate extractor scheme": {
			contrib: func(c *contributions) {
				c.extractors = append(c.extractors, extractorEntry("token-grpc-2", tokenExtractor()))
			},
			want: `both claim scheme "token"`,
		},
		"invalid extractor scheme": {
			contrib: func(c *contributions) {
				c.extractors = append(c.extractors, extractorEntry("bad", prefixExtractor{scheme: "Bad"}))
			},
			want: "declares an invalid scheme",
		},
		"unreachable rule": {
			cfg: func(c *Config) {
				c.Security.Rules = []SecurityRule{
					{Match: "/" + echoServiceName + "/*", Permit: true},
					{Match: "/" + echoServiceName + "/Echo", Authenticate: []authentication.Scheme{schemeToken}},
				}
			},
			want: "security rule 1",
		},
		"uncovered scheme under permit": {
			cfg:  func(c *Config) { c.Security.Default = SecurityPermit },
			want: `scheme "token" is registered but no security rule references it`,
		},
		"rule selects unregistered scheme": {
			cfg: func(c *Config) {
				c.Security.Rules = []SecurityRule{{Match: "/**", Authenticate: []authentication.Scheme{schemeKey}}}
			},
			want: `rule 0 ("/**") names unknown scheme "key"`,
		},
		// A rule governing no served method is still configuration.
		"unmatched rule names unregistered scheme": {
			cfg: func(c *Config) {
				c.Security.Rules = []SecurityRule{{Match: "/other.Service/*", Authenticate: []authentication.Scheme{schemeKey}}}
			},
			want: `names unknown scheme "key"`,
		},
		"rule scheme without authenticators": {
			cfg: func(c *Config) {
				c.Security.Rules = []SecurityRule{{Match: "/other.Service/*", Authenticate: []authentication.Scheme{schemeToken}}}
			},
			contrib: func(c *contributions) {
				c.authenticators, c.extractors = nil, nil
				c.registrars = []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(Public()) })}
			},
			want: `names unknown scheme "token"; registered schemes: (none)`,
		},
		"declaration selects unregistered scheme": {
			contrib: func(c *contributions) {
				c.registrars = []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(Accepts(schemeKey)) })}
			},
			want: `/xbc.test.Echo/Echo: authentication: unknown scheme "key"`,
		},
		"auth needed without authenticator": {
			contrib: func(c *contributions) { c.authenticators, c.extractors = nil, nil },
			want:    "requires authentication but no authenticator is registered",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			c := tokenOnly(whoamiContributor(nil))
			if tc.contrib != nil {
				tc.contrib(&c)
			}
			err := newServer(cfg, c).Preflight(contextFromHost(newFakeHost()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Preflight error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMethodDeclarationsAreCheckedAgainstTheFrozenTable(t *testing.T) {
	other := plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "intruder"},
		Value: registrarFunc(func(r *Registrar) error {
			r.Methods(echoServiceName).Auth(Public())
			return nil
		}),
	}
	cases := map[string]struct {
		registrars []plugin.Entry[ServiceRegistrar]
		want       string
	}{
		"unknown service": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(nil), {
				Identity: plugin.Identity{Plugin: "ghost"},
				Value:    registrarFunc(func(r *Registrar) error { r.Methods("ghost.Service").Auth(Public()); return nil }),
			}},
			want: `"ghost.Service", which no plugin registers`,
		},
		"another contributor's service": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(nil), other},
			want:       "only the registering plugin may declare it",
		},
		"unknown method": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.MethodAuth("Nope", Public()) })},
			want:       "/xbc.test.Echo/Nope, which the service does not define",
		},
		"empty method": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.MethodAuth("", Public()) })},
			want:       "with an empty method name",
		},
		"zero policy": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(AuthPolicy{}) })},
			want:       "Public or Accepts",
		},
		"empty accepts": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(Accepts()) })},
			want:       "names no scheme",
		},
		"duplicate accepts": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(Accepts(schemeToken, schemeToken)) })},
			want:       "more than once",
		},
		"declared twice": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Auth(Public()).Auth(Public()) })},
			want:       "more than once",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := tokenOnly(tc.registrars...)
			err := newServer(DefaultConfig(), c).Preflight(contextFromHost(newFakeHost()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Preflight error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// principalInterceptor declares it needs a Principal and records what it saw.
type principalInterceptor struct {
	testInterceptor
	seen chan string
}

func (principalInterceptor) RequiresPrincipal() {}

func (i principalInterceptor) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		i.seen <- describeCall(ctx)
		return handler(ctx, req)
	}
}

func TestRequiresPrincipalIsPinnedAfterAuthentication(t *testing.T) {
	// In the auth phase a consumer runs after the stage and sees the Principal,
	// although "aaa" sorts before "grpc-authentication" by identity.
	consumer := principalInterceptor{testInterceptor: testInterceptor{order: Order{Phase: PhaseAuth}, rec: &recorder{}}, seen: make(chan string, 1)}
	c := tokenOnly(whoamiContributor(nil))
	c.unary = []plugin.Entry[UnaryInterceptor]{{Identity: plugin.Identity{Plugin: "aaa"}, Value: consumer}}
	s := newServer(DefaultConfig(), c)
	_, conn := startServing(t, s)
	if _, _, err := invokeEcho(conn, outgoing("authorization", "Token good-alice")); err != nil {
		t.Fatal(err)
	}
	if got := <-consumer.seen; got != "principal:alice/token" {
		t.Fatalf("consumer saw %q, want the published principal", got)
	}

	// A consumer that insists on running before authentication is a conflict.
	early := consumer
	early.order = Order{Phase: PhaseAuth, Before: []OrderRef{Require(AuthenticationInterceptorKey)}}
	c.unary = []plugin.Entry[UnaryInterceptor]{{Identity: plugin.Identity{Plugin: "aaa"}, Value: early}}
	if err := newServer(DefaultConfig(), c).Preflight(contextFromHost(newFakeHost())); err == nil {
		t.Fatal("a principal consumer ordered before authentication was accepted")
	}

	// A consumer declared in an earlier phase would otherwise run outside the
	// authentication stage without any ordering of its own saying so; the
	// framework pin turns that into a startup phase conflict.
	observer := consumer
	observer.order = Order{Phase: PhaseObserve}
	c.unary = []plugin.Entry[UnaryInterceptor]{{Identity: plugin.Identity{Plugin: "aaa"}, Value: observer}}
	var conflict *InterceptorPhaseConflictError
	if err := newServer(DefaultConfig(), c).Preflight(contextFromHost(newFakeHost())); !errors.As(err, &conflict) {
		t.Fatalf("Preflight error = %v, want InterceptorPhaseConflictError for an observe-phase principal consumer", err)
	}
}

func TestContributedInterceptorsCannotClaimAssembledStageKeys(t *testing.T) {
	for _, key := range ReservedInterceptorKeys() {
		c := tokenOnly(whoamiContributor(nil))
		c.unary = []plugin.Entry[UnaryInterceptor]{unaryEntry(key, testInterceptor{order: Order{Phase: PhaseAuth}, rec: &recorder{}})}
		var reserved *ReservedInterceptorIdentityError
		if err := newServer(DefaultConfig(), c).Preflight(contextFromHost(newFakeHost())); !errors.As(err, &reserved) {
			t.Fatalf("%s: Preflight error = %v, want ReservedInterceptorIdentityError", key, err)
		}
	}
}

func TestPrincipalAndExemptAccessorsFailClosed(t *testing.T) {
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("a bare context carries no principal")
	}
	if AuthenticationExempt(context.Background()) {
		t.Fatal("a call that never passed authentication is not exempt")
	}
	if _, ok := withPrincipal(context.Background(), authentication.Principal{Subject: "  "}); ok {
		t.Fatal("an empty subject must not be published")
	}
	attrs := map[string]any{"role": "admin"}
	ctx, _ := withPrincipal(context.Background(), authentication.Principal{Subject: "alice", Attributes: attrs})
	attrs["role"] = "mutated"
	got, _ := PrincipalFromContext(ctx)
	got.Attributes["role"] = "mutated-again"
	if again, _ := PrincipalFromContext(ctx); again.Attributes["role"] != "admin" {
		t.Fatalf("published attributes changed to %v", again.Attributes["role"])
	}
}
