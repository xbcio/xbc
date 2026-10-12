package casbin

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	casbincore "github.com/xbcio/xbc/extensions/authorization/casbin"
)

// neverCalled is the handler for paths that must be denied before the handler
// runs.
func neverCalled(t *testing.T) grpc.UnaryHandler {
	t.Helper()
	return func(context.Context, any) (any, error) {
		t.Fatal("the handler ran on a call that had to be denied")
		return nil, nil
	}
}

func TestZeroValueDeniesInsteadOfPanicking(t *testing.T) {
	var p Plugin
	_, err := p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: echoMethod}, neverCalled(t))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("zero-value unary err = %v, want PermissionDenied", err)
	}
	stream := &fakeStream{ctx: context.Background()}
	if err := p.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: repeatMethod},
		func(any, grpc.ServerStream) error {
			t.Fatal("the handler ran on a call that had to be denied")
			return nil
		}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("zero-value stream err = %v, want PermissionDenied", err)
	}
}

func TestStoppedInterceptorDeniesEveryCall(t *testing.T) {
	p, _ := initializedPlugin(t, nil, DefaultConfig())
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: echoMethod}, neverCalled(t))
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "forbidden" {
		t.Fatalf("stopped err = %v, want PermissionDenied forbidden", err)
	}
}

func TestCallsWithoutAPublishedPrincipalAreDenied(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, user:alice, orders:read"
	}, DefaultConfig())

	// A bare context is what a call reaches the interceptor with when the
	// authentication stage published no Principal, so the deny here is the
	// fail-closed side of the identity dependency, not a test artifact.
	_, err := p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: echoMethod}, neverCalled(t))
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "forbidden" {
		t.Fatalf("no-principal err = %v, want PermissionDenied forbidden", err)
	}
}

func TestCallsAfterTheEngineStopsAreDenied(t *testing.T) {
	p, engine := initializedPlugin(t, func(cfg *casbincore.Config) {
		cfg.Policy = "p, user:alice, orders:read"
	}, DefaultConfig())
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := p.UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: echoMethod}, neverCalled(t))
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "forbidden" {
		t.Fatalf("stopped-engine err = %v, want PermissionDenied forbidden", err)
	}
}

// fakeStream is the minimal ServerStream the stream interceptor touches before
// a denial.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeStream) Context() context.Context { return s.ctx }
