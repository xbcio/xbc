package accesslog

import (
	"context"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	grpctransport "github.com/xbcio/xbc/transport/grpc"
)

// Order implements grpctransport.UnaryInterceptor and
// grpctransport.StreamInterceptor. The observation phase sits outside the
// status boundary, so the code a record carries is the one the client
// receives, not the raw error a handler returned.
func (*Plugin) Order() grpctransport.Order {
	return grpctransport.Order{Phase: grpctransport.PhaseObserve}
}

// UnaryInterceptor implements grpctransport.UnaryInterceptor.
func (p *Plugin) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		state := p.state.Load()
		if state == nil || state.config.skip(info.FullMethod) {
			return handler(ctx, req)
		}
		started := time.Now()
		panicked := true
		defer func() { write(state, ctx, info.FullMethod, "unary", err, panicked, started) }()
		resp, err = handler(ctx, req)
		panicked = false
		return resp, err
	}
}

// StreamInterceptor implements grpctransport.StreamInterceptor. A stream is
// one record, written when its handler returns; individual messages are not
// logged.
func (p *Plugin) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		state := p.state.Load()
		if state == nil || state.config.skip(info.FullMethod) {
			return handler(srv, ss)
		}
		started := time.Now()
		panicked := true
		defer func() { write(state, ss.Context(), info.FullMethod, streamKind(info), err, panicked, started) }()
		err = handler(srv, ss)
		panicked = false
		return err
	}
}

// write emits one record. A panic is not recovered here -- the Server's panic
// boundary owns that -- it is only recorded as the codes.Internal the boundary
// will answer with, so the log and the wire agree for the same call.
func write(state *runtimeState, ctx context.Context, fullMethod, kind string, err error, panicked bool, started time.Time) {
	latency := time.Since(started)
	code := status.Code(err)
	if panicked {
		code = codes.Internal
	}
	service, method := splitFullMethod(fullMethod)
	fields := []any{
		"service", service,
		"method", method,
		"kind", kind,
		"code", code.String(),
		"latency", latency,
		"peer", peerIP(ctx),
		"panicked", panicked,
	}

	// Deliberately do not include metadata, messages, status messages, error
	// text, or panic values: metadata carries credentials, and the rest are
	// common paths for payload and PII leaks.
	switch {
	case panicked || serverFault(code):
		state.logger.Error("grpc call completed", fields...)
	case state.config.slowCall > 0 && latency >= state.config.slowCall:
		state.logger.Warn("grpc call completed slowly", fields...)
	default:
		state.logger.Info("grpc call completed", fields...)
	}
}

// serverFault reports the codes that name a failure of the server rather than
// a request the server correctly refused, the gRPC counterpart of an HTTP 5xx.
func serverFault(code codes.Code) bool {
	switch code {
	case codes.Unknown, codes.Internal, codes.Unimplemented, codes.Unavailable, codes.DataLoss, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

func streamKind(info *grpc.StreamServerInfo) string {
	switch {
	case info.IsClientStream && info.IsServerStream:
		return "bidi_stream"
	case info.IsClientStream:
		return "client_stream"
	case info.IsServerStream:
		return "server_stream"
	default:
		return "stream"
	}
}

// peerIP reports the transport peer's address without its port. Proxy
// metadata such as x-forwarded-for is never read: it is caller-controlled.
func peerIP(ctx context.Context) string {
	value, ok := peer.FromContext(ctx)
	if !ok || value.Addr == nil {
		return "unknown"
	}
	address := value.Addr.String()
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "unknown"
}

// splitFullMethod splits a full gRPC method "/service/method". Interceptors
// only run for registered methods, so both halves are bounded; an unparsable
// form degrades to a placeholder rather than logging the raw input.
func splitFullMethod(fullMethod string) (service, method string) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(fullMethod), "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		service, method = trimmed[:index], trimmed[index+1:]
	}
	if service == "" || method == "" {
		return "unknown", "unknown"
	}
	return service, method
}
