package grpc

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	v1reflection "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflection "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

const (
	reflectionV1Method      = "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"
	reflectionV1AlphaMethod = "/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo"
)

func reflectionServerConfig() Config {
	cfg := DefaultConfig()
	cfg.Reflection.Enabled = true
	return cfg
}

// TestReflectionCompilesTheStandardMethodsAsPublicDeclarations pins the
// policy-table half of reflection: the standard methods must be compiled as if
// a registrant declared Public, and must be absent entirely when reflection is
// off, where a call to them can only fail closed.
func TestReflectionCompilesTheStandardMethodsAsPublicDeclarations(t *testing.T) {
	host := newFakeHost()
	defer host.shutdown()

	enabled := newServer(reflectionServerConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	a, err := enabled.assemble(contextFromHost(host))
	if err != nil {
		t.Fatalf("assemble() error = %v", err)
	}
	for _, fullMethod := range []string{reflectionV1Method, reflectionV1AlphaMethod} {
		policy, ok := a.auth.compiled[fullMethod]
		if !ok {
			t.Fatalf("%s has no compiled policy although reflection is enabled", fullMethod)
		}
		if !policy.permit || policy.tier != tierDeclaration {
			t.Fatalf("%s policy = %+v, want a public declaration", fullMethod, policy)
		}
	}
	// The method table reports what registrants registered; reflection is not
	// one of them and is explained on its own report line instead.
	if table := renderMethodTable(a.services); strings.Contains(table, "reflection") {
		t.Fatalf("method table lists reflection:\n%s", table)
	}
	line := renderReflection()
	for _, want := range []string{
		"grpc.reflection.v1.ServerReflection",
		"grpc.reflection.v1alpha.ServerReflection",
		"grpc.security",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("reflection report line missing %q: %s", want, line)
		}
	}

	disabled := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{echoContributor(&echoImpl{})}})
	off, err := disabled.assemble(contextFromHost(host))
	if err != nil {
		t.Fatalf("assemble() error = %v", err)
	}
	for _, fullMethod := range []string{reflectionV1Method, reflectionV1AlphaMethod} {
		if _, ok := off.auth.compiled[fullMethod]; ok {
			t.Fatalf("%s was compiled although reflection is disabled", fullMethod)
		}
	}
}

// TestARuleTightensReflectionWithoutDisablingIt pins the tier order: the
// compiled declaration is a declaration, so a grpc.security rule that names a
// reflection method outranks it, and one that does not leaves the other
// reflection service public.
func TestARuleTightensReflectionWithoutDisablingIt(t *testing.T) {
	cfg := reflectionServerConfig()
	cfg.Security.Rules = []SecurityRule{{
		Match:        "/grpc.reflection.v1.ServerReflection/*",
		Authenticate: []authentication.Scheme{schemeToken},
	}}
	s := newServer(cfg, tokenOnly(echoContributor(&echoImpl{})))
	host := newFakeHost()
	defer host.shutdown()
	a, err := s.assemble(contextFromHost(host))
	if err != nil {
		t.Fatalf("assemble() error = %v", err)
	}

	tightened, ok := a.auth.compiled[reflectionV1Method]
	if !ok {
		t.Fatalf("%s has no compiled policy", reflectionV1Method)
	}
	if tightened.permit || tightened.tier != tierApplicationRule {
		t.Fatalf("v1 policy = %+v, want the application rule to outrank the declaration", tightened)
	}
	if got := tightened.selection.Schemes(); !slices.Equal(got, []authentication.Scheme{schemeToken}) {
		t.Fatalf("v1 selection = %v, want the rule's scheme", got)
	}
	untouched, ok := a.auth.compiled[reflectionV1AlphaMethod]
	if !ok || !untouched.permit || untouched.tier != tierDeclaration {
		t.Fatalf("v1alpha policy = %+v, %v; want the untouched public declaration", untouched, ok)
	}
}

// reflectionSquatterDesc is the descriptor of a service that claims the v1
// reflection name. It only ever reaches checkService, so the handlers do not
// need to make sense.
func reflectionSquatterDesc() *grpc.ServiceDesc {
	desc := echoDesc()
	desc.ServiceName = v1reflection.ServerReflection_ServiceDesc.ServiceName
	return desc
}

func reflectionSquatterContributor() plugin.Entry[ServiceRegistrar] {
	return plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "squatter"},
		Value: registrarFunc(func(r *Registrar) error {
			r.RegisterService(reflectionSquatterDesc(), &echoImpl{})
			r.Methods(v1reflection.ServerReflection_ServiceDesc.ServiceName).Auth(Public())
			return nil
		}),
	}
}

// TestReflectionCollisionFailsStartupInsteadOfTerminatingTheProcess pins the
// collision check: grpc-go treats a duplicate service registration as fatal,
// and reflection.Register would reach that check for a squatter's service.
// With reflection disabled the same service is an ordinary registration.
func TestReflectionCollisionFailsStartupInsteadOfTerminatingTheProcess(t *testing.T) {
	host := newFakeHost()
	defer host.shutdown()

	enabled := newServer(reflectionServerConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{reflectionSquatterContributor()}})
	_, err := enabled.assemble(contextFromHost(host))
	if err == nil {
		t.Fatal("assemble() accepted a service that collides with reflection")
	}
	for _, want := range []string{"grpc.reflection", "squatter", v1reflection.ServerReflection_ServiceDesc.ServiceName} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collision error %q does not name %q", err, want)
		}
	}

	disabled := newServer(DefaultConfig(), contributions{registrars: []plugin.Entry[ServiceRegistrar]{reflectionSquatterContributor()}})
	if _, err := disabled.assemble(contextFromHost(host)); err != nil {
		t.Fatalf("assemble() with reflection disabled error = %v", err)
	}
}

// reflectionClient is the shared shape of the generated v1 and v1alpha clients:
// one bidirectional stream answers every reflection query.
type reflectionClient[Req, Resp any] interface {
	ServerReflectionInfo(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[Req, Resp], error)
}

// listServices drives one ListServices exchange over a reflection stream and
// returns the names the server reports.
func listServices[Req, Resp any](
	t *testing.T,
	client reflectionClient[Req, Resp],
	request *Req,
	names func(*Resp) []string,
) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatalf("ServerReflectionInfo: %v", err)
	}
	if err := stream.Send(request); err != nil {
		t.Fatalf("Send: %v", err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return names(response)
}

func v1ServiceNames(response *v1reflection.ServerReflectionResponse) []string {
	var names []string
	for _, service := range response.GetListServicesResponse().GetService() {
		names = append(names, service.GetName())
	}
	return names
}

func v1AlphaServiceNames(response *v1alphareflection.ServerReflectionResponse) []string {
	var names []string
	for _, service := range response.GetListServicesResponse().GetService() {
		names = append(names, service.GetName())
	}
	return names
}

// TestReflectionServesTheSchemaToAnUncredentialedClient is the acceptance
// path: with reflection enabled and default deny in force, an uncredentialed
// standard client learns the served schema over the wire, on both the v1 and
// the v1alpha service. Reaching a handler at all under default deny is itself
// the proof that the compiled public declaration covered the call.
func TestReflectionServesTheSchemaToAnUncredentialedClient(t *testing.T) {
	s := newServer(reflectionServerConfig(), tokenOnly(echoContributor(&echoImpl{})))
	_, conn := startServing(t, s)

	want := []string{
		echoServiceName,
		v1reflection.ServerReflection_ServiceDesc.ServiceName,
		v1alphareflection.ServerReflection_ServiceDesc.ServiceName,
	}

	v1Names := listServices(t, v1reflection.NewServerReflectionClient(conn),
		&v1reflection.ServerReflectionRequest{MessageRequest: &v1reflection.ServerReflectionRequest_ListServices{}},
		v1ServiceNames)
	if !sameNameSet(v1Names, want) {
		t.Fatalf("v1 ListServices = %v, want %v", v1Names, want)
	}

	v1AlphaNames := listServices(t, v1alphareflection.NewServerReflectionClient(conn),
		&v1alphareflection.ServerReflectionRequest{MessageRequest: &v1alphareflection.ServerReflectionRequest_ListServices{}},
		v1AlphaServiceNames)
	if !sameNameSet(v1AlphaNames, want) {
		t.Fatalf("v1alpha ListServices = %v, want %v", v1AlphaNames, want)
	}
}

// TestReflectionIsUnimplementedByDefault pins the default: nothing serves the
// reflection method, so grpc-go answers Unimplemented before any interceptor
// runs -- the same answer an unknown method has always received.
func TestReflectionIsUnimplementedByDefault(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(echoContributor(&echoImpl{})))
	_, conn := startServing(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := v1reflection.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err == nil {
		_ = stream.Send(&v1reflection.ServerReflectionRequest{MessageRequest: &v1reflection.ServerReflectionRequest_ListServices{}})
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("reflection call code = %v, want Unimplemented (error %v)", status.Code(err), err)
	}
}

func sameNameSet(got, want []string) bool {
	got = slices.Sorted(slices.Values(got))
	want = slices.Sorted(slices.Values(want))
	return slices.Equal(got, want)
}
