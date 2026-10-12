package grpc

import (
	"context"
	"fmt"
	"reflect"
	"runtime/debug"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

const (
	// PanicBoundaryKey is the canonical producer identity of the outermost
	// panic boundary, which the Server assembles itself on both chains rather
	// than selecting it as a plugin. The key is reserved for that stage; see
	// ReservedInterceptorKeys.
	PanicBoundaryKey plugin.Key = "grpc-panic-boundary"
)

// reservedInterceptorKeys are the interceptor identities the Server assembles
// itself. They are ordering anchors, not plugin Definitions: each is a stage a
// framework-owned pin makes required, and a required stage cannot be an opt-in
// plugin a composition root may leave out. Because the anchors share the flat
// plugin-key namespace, the reservation is explicit and enforced, so a plugin
// claiming one fails with the rule it broke rather than a duplicate identity.
var reservedInterceptorKeys = map[plugin.Key]string{
	PanicBoundaryKey:             "the Server assembles the outermost panic boundary from its own grpc.recovery section",
	StatusBoundaryKey:            "the Server assembles the outermost status boundary from the collected StatusMapper plugins",
	AuthenticationInterceptorKey: "the Server assembles the authentication stage from its own grpc.security section",
}

// ReservedInterceptorIdentityError reports a contributed interceptor that
// claims an identity the gRPC transport assembles itself.
type ReservedInterceptorIdentityError struct {
	Identity plugin.Identity
	Reason   string
}

func (e *ReservedInterceptorIdentityError) Error() string {
	return fmt.Sprintf(
		"xbc: interceptor identity %s is reserved by the gRPC transport and cannot be contributed by a plugin: %s",
		e.Identity, e.Reason,
	)
}

// ReservedInterceptorKeys returns the sorted plugin keys the gRPC transport
// reserves for interceptors its Server assembles itself. Contributing an
// interceptor under one of them fails assembly with
// ReservedInterceptorIdentityError, and a Definition in this workspace claiming
// one is rejected by the plugin-snapshot guard.
func ReservedInterceptorKeys() []plugin.Key {
	keys := make([]plugin.Key, 0, len(reservedInterceptorKeys))
	for key := range reservedInterceptorKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func rejectReservedInterceptorIdentities[T ordered](entries []plugin.Entry[T]) error {
	for _, entry := range entries {
		if reason, reserved := reservedInterceptorKeys[entry.Identity.Plugin]; reserved {
			return &ReservedInterceptorIdentityError{Identity: entry.Identity, Reason: reason}
		}
	}
	return nil
}

var panicBoundaryIdentity = plugin.Identity{Plugin: PanicBoundaryKey}

// panicBoundary turns a handler or interceptor panic into codes.Internal. grpc-go
// recovers nothing itself: an unrecovered panic in a handler goroutine
// terminates the process, taking every other in-flight RPC with it. That is why
// the boundary is assembled rather than contributed.
type panicBoundary struct {
	logger log.Logger
	stack  bool
}

func newPanicBoundary(logger log.Logger, stack bool) *panicBoundary {
	if logger == nil {
		logger = log.L()
	}
	return &panicBoundary{logger: logger, stack: stack}
}

func (*panicBoundary) Order() Order { return Order{Phase: PhaseRecover} }

func (b *panicBoundary) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				resp, err = nil, b.recovered(info.FullMethod, recovered)
			}
		}()
		return handler(ctx, req)
	}
}

func (b *panicBoundary) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = b.recovered(info.FullMethod, recovered)
			}
		}()
		return handler(srv, stream)
	}
}

// recovered logs the panic and returns the status the client receives. The
// recovered value is never logged and never returned: panic strings frequently
// carry credentials or caller-controlled data. Its type, the method, and
// optionally the stack are enough to find the code path.
func (b *panicBoundary) recovered(method string, value any) error {
	fields := []any{
		"method", method,
		"panic_type", reflect.TypeOf(value).String(),
	}
	if b.stack {
		fields = append(fields, "stack", string(debug.Stack()))
	}
	b.logger.Error("grpc request panic recovered", fields...)
	return status.Error(codes.Internal, "internal error")
}
