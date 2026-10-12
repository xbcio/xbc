// Package greeter is the gRPC quickstart's business Plugin. It owns a canonical
// Definition and contributes the generated GreeterService through an explicit
// Bundle.
package greeter

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	greeterv1 "github.com/xbcio/xbc/examples/grpcquickstart/internal/gen/greeter/v1"
	"github.com/xbcio/xbc/plugin"
	xbcgrpc "github.com/xbcio/xbc/transport/grpc"
)

// Key is greeter's stable configuration and runtime identity.
const Key plugin.Key = "greeter"

// maxGreetings bounds one stream, so a request cannot ask the server to
// produce an unbounded response.
const maxGreetings = 10

// Plugin serves greeter.v1.GreeterService.
type Plugin struct {
	greeterv1.UnimplementedGreeterServiceServer
}

var (
	_ xbcgrpc.ServiceRegistrar       = (*Plugin)(nil)
	_ greeterv1.GreeterServiceServer = (*Plugin)(nil)
)

var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (*Plugin, error) { return &Plugin{}, nil },
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[xbcgrpc.ServiceRegistrar](func(value *Plugin) xbcgrpc.ServiceRegistrar { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns greeter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns greeter's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// RegisterServices implements xbcgrpc.ServiceRegistrar. The generated
// registration function takes the Registrar directly. The service is declared
// Public beside its registration: every method is denied by default, and an
// example that composes no authenticator has nothing else to admit it with.
func (p *Plugin) RegisterServices(r *xbcgrpc.Registrar) error {
	greeterv1.RegisterGreeterServiceServer(r, p)
	r.Methods(greeterv1.GreeterService_ServiceDesc.ServiceName).Auth(xbcgrpc.Public())
	return nil
}

// SayHello implements greeterv1.GreeterServiceServer.
func (*Plugin) SayHello(_ context.Context, request *greeterv1.SayHelloRequest) (*greeterv1.SayHelloResponse, error) {
	name, err := validName(request.GetName())
	if err != nil {
		return nil, err
	}
	return &greeterv1.SayHelloResponse{Message: "hello, " + name}, nil
}

// StreamGreetings implements greeterv1.GreeterServiceServer. It stops early,
// with the stream's own context error, when the client goes away or the
// server is shutting down.
func (*Plugin) StreamGreetings(request *greeterv1.StreamGreetingsRequest, stream grpc.ServerStreamingServer[greeterv1.StreamGreetingsResponse]) error {
	name, err := validName(request.GetName())
	if err != nil {
		return err
	}
	count := request.GetCount()
	if count < 1 || count > maxGreetings {
		return status.Errorf(codes.InvalidArgument, "count must be between 1 and %d", maxGreetings)
	}
	for sequence := int32(1); sequence <= count; sequence++ {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		message := &greeterv1.StreamGreetingsResponse{
			Message:  fmt.Sprintf("hello #%d, %s", sequence, name),
			Sequence: sequence,
		}
		if err := stream.Send(message); err != nil {
			return err
		}
	}
	return nil
}

// validName applies the same bounds the Web quickstart's request binding
// does. The rejection names the rule, never the input.
func validName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if length := len([]rune(name)); length < 2 || length > 80 {
		return "", status.Error(codes.InvalidArgument, "name must be between 2 and 80 characters")
	}
	return name, nil
}
