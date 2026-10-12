package casbin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/authentication"
	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

const testScheme authentication.Scheme = "token"

// testAuth is the smallest authenticator and extractor pair the composition
// needs: "Token good:<subject>" authenticates as that subject, "Token
// service" authenticates without a principal, everything else is rejected.
type testAuth struct{}

func (testAuth) Scheme() authentication.Scheme { return testScheme }

func (testAuth) Authenticate(_ context.Context, credential authentication.Credential) (authentication.Result, error) {
	value, _ := authentication.CredentialAs[string](credential)
	switch {
	case strings.HasPrefix(value, "good:"):
		return authentication.Accepted(authentication.Principal{
			Subject:    strings.TrimPrefix(value, "good:"),
			AuthMethod: string(testScheme),
		}), nil
	case value == "service":
		return authentication.AcceptedWithoutPrincipal(), nil
	default:
		return authentication.Rejected("invalid credential"), nil
	}
}

func (testAuth) ExtractCredential(_ context.Context, md metadata.MD) (authentication.CredentialResult, error) {
	for _, value := range md.Get("authorization") {
		if !strings.HasPrefix(value, "Token ") {
			continue
		}
		credential := strings.TrimSpace(strings.TrimPrefix(value, "Token "))
		if credential == "" {
			return authentication.Malformed("empty credential"), nil
		}
		return authentication.Presented(credential), nil
	}
	return authentication.Absent(), nil
}

var testAuthBundle = plugin.BundleOf(plugin.Define(
	"test-auth",
	func(plugin.BuildContext) (*testAuth, error) { return new(testAuth), nil },
	plugin.Options[*testAuth]{
		Exports: plugin.Contracts(
			plugin.ExportAs[authentication.Authenticator](func(value *testAuth) authentication.Authenticator { return value }),
			plugin.ExportAs[grpctransport.CredentialExtractor](func(value *testAuth) grpctransport.CredentialExtractor { return value }),
		),
	},
))

// echoPlugin registers the test service with declare's registration-time
// declarations when one is given. It implements ServiceRegistrar on the
// primary type, which is what ExportAs requires.
type echoPlugin struct {
	declare func(*grpctransport.MethodOptions)
}

func (p *echoPlugin) RegisterServices(r *grpctransport.Registrar) error {
	r.RegisterService(echoDesc(), echoEcho{})
	if p.declare != nil {
		p.declare(r.Methods(echoServiceName))
	}
	return nil
}

// echoBundle selects the test service.
func echoBundle(declare func(*grpctransport.MethodOptions)) plugin.Bundle {
	return plugin.BundleOf(plugin.Define(
		"test-echo",
		func(plugin.BuildContext) (*echoPlugin, error) { return &echoPlugin{declare: declare}, nil },
		plugin.Options[*echoPlugin]{
			Exports: plugin.Contracts(
				plugin.ExportAs[grpctransport.ServiceRegistrar](func(value *echoPlugin) grpctransport.ServiceRegistrar { return value }),
			),
		},
	))
}

// bootHost is the smallest faithful RuntimeHost a full-transport probe test
// needs: one execution context, one traffic gate, and task admission.
type bootHost struct {
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	tasks     sync.WaitGroup
}

var _ plugin.RuntimeHost = (*bootHost)(nil)

func newBootHost() *bootHost {
	execution, cancel := context.WithCancel(context.Background())
	return &bootHost{execution: execution, cancel: cancel, gate: make(chan struct{})}
}

func (h *bootHost) ExecutionContext() context.Context { return h.execution }
func (*bootHost) Logger() log.Logger                  { return log.Nop() }
func (*bootHost) ProcessInstance() string             { return "casbin-grpc-test-process" }

func (*bootHost) Service() plugin.ServiceIdentity { return plugin.ServiceIdentity{} }

func (*bootHost) IngressStopBudget() time.Duration           { return 0 }
func (h *bootHost) TrafficGate() <-chan struct{}             { return h.gate }
func (*bootHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*bootHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }
func (*bootHost) RequestShutdown(plugin.Identity, string) bool                      { return false }

func (h *bootHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	h.tasks.Add(1)
	go func() {
		defer h.tasks.Done()
		fn(h.execution)
	}()
	return true
}

func (h *bootHost) releaseTraffic() { h.gateOnce.Do(func() { close(h.gate) }) }

func (h *bootHost) shutdown() {
	h.cancel()
	h.tasks.Wait()
}

func (h *bootHost) contextFor(key plugin.Key) *plugin.Context {
	return plugin.NewRuntimeContext(h, plugin.Identity{Plugin: key, Instance: plugin.DefaultInstance})
}

// bootEcho composes the real gRPC server, this interceptor, the neutral
// engine, and the test service and authenticator through the real assembly,
// starts the server, opens the traffic gate, and returns a connected client.
func bootEcho(t *testing.T, declare func(*grpctransport.MethodOptions), env map[string]any) (*grpc.ClientConn, *grpctransport.Server, *casbincore.Plugin) {
	t.Helper()
	host := newBootHost()
	t.Cleanup(host.shutdown)

	environment, err := config.NewEnvironment(env, "")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{grpctransport.Bundle(), Bundle(), testAuthBundle, echoBundle(declare)},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}

	engineInstance, found := constructed.Instance(plugin.Identity{Plugin: casbincore.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the engine was not constructed")
	}
	engine, ok := engineInstance.Primary().(*casbincore.Plugin)
	if !ok {
		t.Fatalf("engine primary = %T, want *casbincore.Plugin", engineInstance.Primary())
	}
	serverInstance, found := constructed.Instance(plugin.Identity{Plugin: grpctransport.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the gRPC server was not constructed")
	}
	server, ok := serverInstance.Primary().(*grpctransport.Server)
	if !ok {
		t.Fatalf("gRPC server primary = %T, want *grpctransport.Server", serverInstance.Primary())
	}

	serverContext := host.contextFor(grpctransport.Key)
	if err := server.Start(serverContext); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := server.OpenTraffic(serverContext); err != nil {
		t.Fatalf("OpenTraffic() error = %v", err)
	}
	host.releaseTraffic()
	// stopServer runs before the host shutdown registered above (cleanups are
	// LIFO): the serving task ends only when the server it serves is stopped.
	t.Cleanup(func() {
		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(stopContext)
	})

	conn, err := grpc.NewClient(server.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, server, engine
}

// securityEnv is the shared grpc section: an ephemeral address and one
// declared scheme, which is the arbitration order for the one authenticator.
func securityEnv(plugins map[string]any) map[string]any {
	return map[string]any{
		"grpc": map[string]any{
			"addr":     "127.0.0.1:0",
			"security": map[string]any{"schemes": []string{string(testScheme)}},
		},
		"plugins": plugins,
	}
}

// invokeEcho calls the unary method, retrying while the channel connects.
func invokeEcho(t *testing.T, conn *grpc.ClientConn, credential string) (string, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if credential != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", credential)
		}
		out := new(wrapperspb.StringValue)
		err := conn.Invoke(ctx, echoMethod, wrapperspb.String("x"), out)
		cancel()
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			return out.GetValue(), err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// invokeRepeat opens the server-streaming method and reads its single answer.
func invokeRepeat(t *testing.T, conn *grpc.ClientConn, credential string) (string, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if credential != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", credential)
		}
		out, err := repeatOnce(conn, ctx)
		cancel()
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func repeatOnce(conn *grpc.ClientConn, ctx context.Context) (string, error) {
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Repeat", ServerStreams: true}, repeatMethod)
	if err != nil {
		return "", err
	}
	if err := stream.SendMsg(wrapperspb.String("x")); err != nil {
		return "", err
	}
	if err := stream.CloseSend(); err != nil {
		return "", err
	}
	out := new(wrapperspb.StringValue)
	err = stream.RecvMsg(out)
	return out.GetValue(), err
}

func TestRoutePermissionAllowsAndDeniesOnBothChains(t *testing.T) {
	conn, _, _ := bootEcho(t, func(m *grpctransport.MethodOptions) {
		m.Perm("orders:read")
	}, securityEnv(map[string]any{
		"casbin":      map[string]any{"policy": "p, user:alice, orders:read"},
		"casbin-grpc": map[string]any{},
	}))

	if got, err := invokeEcho(t, conn, "Token good:user:alice"); err != nil || got != "principal:user:alice" {
		t.Fatalf("authorized unary = %q, %v", got, err)
	}
	if got, err := invokeRepeat(t, conn, "Token good:user:alice"); err != nil || got != "principal:user:alice" {
		t.Fatalf("authorized stream = %q, %v", got, err)
	}
	for name, call := range map[string]func() (string, error){
		"unary":  func() (string, error) { return invokeEcho(t, conn, "Token good:user:bob") },
		"stream": func() (string, error) { return invokeRepeat(t, conn, "Token good:user:bob") },
	} {
		got, err := call()
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "forbidden" || got != "" {
			t.Fatalf("denied %s = %q, %v; want PermissionDenied forbidden", name, got, err)
		}
	}
	// Authentication precedes authorization: no credential never reaches the
	// policy decision.
	if _, err := invokeEcho(t, conn, ""); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous err = %v, want Unauthenticated", err)
	}
}

func TestMissingPermissionFollowsTheConfiguredPolicy(t *testing.T) {
	conn, _, _ := bootEcho(t, nil, securityEnv(map[string]any{
		"casbin":      map[string]any{"policy": "p, user:alice, orders:read"},
		"casbin-grpc": map[string]any{},
	}))
	if _, err := invokeEcho(t, conn, "Token good:user:alice"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("default deny err = %v, want PermissionDenied for an undeclared permission", err)
	}

	conn, _, _ = bootEcho(t, nil, securityEnv(map[string]any{
		"casbin":      map[string]any{"policy": "p, user:alice, orders:read"},
		"casbin-grpc": map[string]any{"missing_permission": "allow"},
	}))
	if got, err := invokeEcho(t, conn, "Token good:user:alice"); err != nil || got != "principal:user:alice" {
		t.Fatalf("missing_permission allow unary = %q, %v; want the verified subject", got, err)
	}
	// allow skips Casbin for the method; it never skips authentication.
	if got, err := invokeEcho(t, conn, "Token good:user:bob"); err != nil || got != "principal:user:bob" {
		t.Fatalf("missing_permission allow for bob = %q, %v; want the verified subject", got, err)
	}
}

func TestPathMethodConventionUsesTheFullMethodAndRPC(t *testing.T) {
	conn, _, _ := bootEcho(t, nil, securityEnv(map[string]any{
		"casbin": map[string]any{
			"request_convention": "path_method",
			"policy":             "p, user:alice, /xbc.test.Echo/Echo, RPC",
		},
		"casbin-grpc": map[string]any{},
	}))
	if got, err := invokeEcho(t, conn, "Token good:user:alice"); err != nil || got != "principal:user:alice" {
		t.Fatalf("granted unary = %q, %v", got, err)
	}
	if _, err := invokeRepeat(t, conn, "Token good:user:alice"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stream err = %v, want PermissionDenied for a method the policy does not grant", err)
	}
	if _, err := invokeEcho(t, conn, "Token good:user:bob"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other subject err = %v, want PermissionDenied", err)
	}
}

func TestExemptMethodBypassesAuthorization(t *testing.T) {
	conn, _, _ := bootEcho(t, func(m *grpctransport.MethodOptions) {
		m.Auth(grpctransport.Public())
	}, securityEnv(map[string]any{
		"casbin":      map[string]any{"policy": ""},
		"casbin-grpc": map[string]any{},
	}))
	if got, err := invokeEcho(t, conn, ""); err != nil || got != "exempt" {
		t.Fatalf("public method = %q, %v; want the exempt classification with no credentials", got, err)
	}
}

func TestVerifiedCallWithoutPrincipalIsDenied(t *testing.T) {
	conn, _, _ := bootEcho(t, func(m *grpctransport.MethodOptions) {
		m.Perm("orders:read")
	}, securityEnv(map[string]any{
		"casbin":      map[string]any{"policy": "p, user:alice, orders:read"},
		"casbin-grpc": map[string]any{},
	}))
	if _, err := invokeEcho(t, conn, "Token service"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("principal-less err = %v, want PermissionDenied", err)
	}
}

func TestEngineStoppedFailsClosedWhileTheServerServes(t *testing.T) {
	conn, _, engine := bootEcho(t, func(m *grpctransport.MethodOptions) {
		m.Perm("orders:read")
	}, securityEnv(map[string]any{
		"casbin":      map[string]any{"policy": "p, user:alice, orders:read"},
		"casbin-grpc": map[string]any{},
	}))
	if got, err := invokeEcho(t, conn, "Token good:user:alice"); err != nil || got != "principal:user:alice" {
		t.Fatalf("pre-stop unary = %q, %v", got, err)
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := invokeEcho(t, conn, "Token good:user:alice"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("post-stop err = %v, want PermissionDenied", err)
	}
}

// enforceErrorModel keeps the two request fields the route_permission
// convention requires -- so the engine constructs -- but its matcher reads
// r.obj, which the request definition does not declare. Every Enforce call
// therefore fails while evaluating the matcher, which is what puts this model
// on the evaluator-error branch through a perfectly honest engine.
const enforceErrorModel = `[request_definition]
r = sub, perm

[policy_definition]
p = sub, perm

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || g(r.sub, p.sub)) && r.obj == p.perm
`

// TestEnforceErrorFailsClosed pins the evaluator-error branch: when Enforce
// reports an error rather than a decision, the call must end in the same
// forbidden status as every other denial -- an evaluation failure is never an
// allow -- and the evaluator's error text, which quotes the policy shape, must
// not be readable from the client-visible status. Both chains share the
// branch, so both are asserted.
func TestEnforceErrorFailsClosed(t *testing.T) {
	conn, _, _ := bootEcho(t, func(m *grpctransport.MethodOptions) {
		m.Perm("orders:read")
	}, securityEnv(map[string]any{
		"casbin": map[string]any{
			"policy": "p, user:alice, orders:read",
			"model":  enforceErrorModel,
		},
		"casbin-grpc": map[string]any{},
	}))
	for name, call := range map[string]func() (string, error){
		"unary":  func() (string, error) { return invokeEcho(t, conn, "Token good:user:alice") },
		"stream": func() (string, error) { return invokeRepeat(t, conn, "Token good:user:alice") },
	} {
		got, err := call()
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "forbidden" || got != "" {
			t.Fatalf("evaluator-error %s = %q, %v; want PermissionDenied forbidden", name, got, err)
		}
		for _, leak := range []string{"invalid request size", "request size", "casbin", "enforce"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("client-visible %s status %q leaks evaluator detail %q", name, err, leak)
			}
		}
	}
}
